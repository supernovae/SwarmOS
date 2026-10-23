package pr

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workflow"
	"github.com/supernovae/swarmos/internal/workspace"
)

func fixture(t *testing.T) (*Service, int, int, string) {
	t.Helper()
	t.Setenv("SWARM_EXECUTION_MODE", "trusted-local")
	controlHome := t.TempDir()
	t.Setenv("HOME", controlHome)
	t.Setenv("XDG_CONFIG_HOME", controlHome)
	t.Setenv("APPDATA", controlHome)
	root := t.TempDir()
	for name, body := range map[string]string{
		".swarm/config.yaml":                     "version: 1\ndefault_agent: mock\ndefault_gate: none\nlease:\n  duration: 4h\n  renewable: true\n  max_renewals: 3\nsecurity:\n  require_ready_label: true\n  ready_label: swarm:ready\nagents:\n  mock:\n    binary: mock\n",
		".swarm/workflows/feature.yaml":          "name: feature\ndefault_gate: none\nsteps:\n  - name: design\n    role: planner\n    artifact: design\n",
		".swarm/project.md":                      "# Project\n",
		".swarm/architecture.md":                 "# Architecture\n",
		".swarm/conventions.md":                  "# Conventions\n",
		".swarm/prompts/planner.md":              "# Planner\n",
		".swarm/prompts/reviewer.md":             "# Reviewer\n",
		".swarm/prompts/developer.md":            "# Developer\n",
		".swarm/prompts/tester.md":               "# Tester\n",
		".swarm/validation/required-checks.yaml": "required_checks:\n  - name: git-available\n    command: git --version\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**']\n",
	} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("git %v: %v: %s", args, e, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.invalid")
	git("add", ".")
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "base")
	base := git("rev-parse", "HEAD")
	ctx := context.Background()
	tr := tracker.NewLocalTracker(root).WithUser("alice", "OWNER")
	body := "```yaml swarm-workunit\nid: pr-test\nobjective: Revise linked PR\nconstraints: [Keep issue contract]\nacceptance: [Preserve PR identity]\nworkflow: feature\ngate: none\n```"
	if _, e := tr.CreateIssue(ctx, "unrelated", body, nil); e != nil {
		t.Fatal(e)
	}
	issue, e := tr.CreateIssue(ctx, "linked", body, nil)
	if e != nil {
		t.Fatal(e)
	}
	manager := lease.NewManager(tr, contract.SecurityConfig{RequireReadyLabel: true, ReadyLabel: "swarm:ready"})
	if e = manager.ConfigureStore(root); e != nil {
		t.Fatal(e)
	}
	if _, e = manager.ApproveTask(ctx, issue.Number, "alice"); e != nil {
		t.Fatal(e)
	}
	state, e := manager.Acquire(ctx, issue.Number, "alice", "mock", "all", time.Hour, 3)
	if e != nil {
		t.Fatal(e)
	}
	branch := workspace.BranchName(issue.Number, issue.Title)
	git("branch", branch, base)
	if _, e = manager.RecordCheckpoint(ctx, issue.Number, "alice", branch, base); e != nil {
		t.Fatal(e)
	}
	pr, e := tr.CreateDraftPR(ctx, tracker.DraftPRRequest{IssueNumber: issue.Number, Title: "linked", Body: "candidate", Head: branch, Base: git("branch", "--show-current")})
	if e != nil {
		t.Fatal(e)
	}
	if pr.Number == issue.Number {
		t.Fatal("fixture must distinguish PR and issue identifiers")
	}
	if _, e = manager.RecordSubmission(ctx, issue.Number, "alice", pr.Number, pr.HTMLURL); e != nil {
		t.Fatal(e)
	}
	if e = lease.SaveClaimToken(root, issue.Number, state.Lease.ClaimID); e != nil {
		t.Fatal(e)
	}
	return &Service{Root: root, Tracker: tr, Manager: manager}, issue.Number, pr.Number, base
}

func TestLinkedPRIdentityAndPendingState(t *testing.T) {
	s, issue, number, base := fixture(t)
	ctx := context.Background()
	r, e := s.Status(ctx, number)
	if e != nil {
		t.Fatal(e)
	}
	if r.IssueNumber != issue || r.PR.Number != number || r.HeadSHA != base || r.State.PRNumber != number {
		t.Fatalf("incorrect link: %+v", r)
	}
	if _, e = s.Status(ctx, issue); e == nil {
		t.Fatal("issue identifier was accepted as PR identifier")
	}
	if _, e = s.Validate(ctx, number); e == nil {
		t.Fatal("unprepared PR accepted validation")
	}
	if _, e = s.Submit(ctx, number); e != nil {
		t.Fatalf("idempotent submit: %v", e)
	}
	if _, e = s.Rebase(ctx, Options{Number: number, Continue: true, Abort: true}); e == nil {
		t.Fatal("conflicting rebase selectors accepted")
	}
}

func TestPRRevisionCannotPublishViaIssueCheckpoint(t *testing.T) {
	s, issue, number, base := fixture(t)
	ctx := context.Background()
	r, e := s.withClaim(ctx, &Result{PR: &tracker.PullRequest{Number: number, HeadSHA: base}, IssueNumber: issue}, func(owner string, st *lease.IssueState) (*Result, error) {
		if st.PRNumber != number {
			t.Fatalf("incorrect claim identity: %+v", st)
		}
		_, err := s.Manager.BeginPRRevision(ctx, issue, owner, number, base, base, "swarm/pr-1")
		return nil, err
	})
	if e != nil {
		t.Fatal(e)
	}
	_ = r
	if _, e = s.Manager.RecordCheckpoint(ctx, issue, "alice", workspace.BranchName(issue, "linked"), base); e == nil {
		t.Fatal("issue checkpoint bypassed pending PR revision")
	}
	if _, e = s.Validate(ctx, number); e == nil {
		t.Fatal("revision without PR workspace/approval accepted validation")
	}
}

func TestPRLinkRejectsUnrecordedAndClosedCandidates(t *testing.T) {
	s, issue, number, _ := fixture(t)
	ctx := context.Background()
	linked, err := s.Tracker.GetPullRequest(ctx, number)
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := s.Tracker.CreateDraftPR(ctx, tracker.DraftPRRequest{
		IssueNumber: issue, Title: "unrecorded", Head: linked.Head, Base: linked.Base,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Status(ctx, orphan.Number); err == nil {
		t.Fatal("metadata-only issue link bypassed authoritative state")
	}
	path := filepath.Join(s.Root, ".swarm", "local-tracker", "state.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		PullRequests map[int]*tracker.PullRequest `json:"pull_requests"`
	}
	if err = json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	state.PullRequests[number].State = "closed"
	var document map[string]json.RawMessage
	if err = json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(state.PullRequests)
	if err != nil {
		t.Fatal(err)
	}
	document["pull_requests"] = encoded
	encoded, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Status(ctx, number); err == nil {
		t.Fatal("closed PR accepted as active WorkUnit revision")
	}
}

func TestPRWorkDryRunExposesPreflightWithoutClaim(t *testing.T) {
	s, issue, number, _ := fixture(t)
	ctx := context.Background()
	before, err := s.Manager.GetCurrentState(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Work(ctx, Options{Number: number, Work: workflow.WorkOptions{InspectOnly: true, AgentName: "mock"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "ready" || result.Preflight == nil || result.Preflight.Issue.Number != issue || result.Preflight.Agents["planner"] != "mock" {
		t.Fatalf("dry-run omitted execution selection: %+v", result)
	}
	after, err := s.Manager.GetCurrentState(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	if after.PRRevision != nil || after.Lease.ClaimID != before.Lease.ClaimID {
		t.Fatalf("dry-run mutated claim or PR revision: before=%+v after=%+v", before, after)
	}
}

func TestPRWorkPreservesGenuineDirtyEdits(t *testing.T) {
	s, _, number, _ := fixture(t)
	ctx := context.Background()
	first, err := s.Work(ctx, Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock"}})
	if err != nil {
		t.Fatal(err)
	}
	assertNoPRMarkers(t, first.Worktree)
	if err := os.WriteFile(filepath.Join(first.Worktree, "real-edit.txt"), []byte("keep me"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Work(ctx, Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock"}}); err == nil {
		t.Fatalf("genuine dirty edit was lost or accepted: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(first.Worktree, "real-edit.txt"))
	if err != nil || string(content) != "keep me" {
		t.Fatalf("genuine edit changed: %q, %v", content, err)
	}
}

func TestPRWorkRelativeRootAndValidation(t *testing.T) {
	s, _, number, _ := fixture(t)
	absolute := s.Root
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, absolute)
	if err != nil {
		t.Fatal(err)
	}
	s.Root = relative
	result, err := s.Work(context.Background(), Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Worktree != workspace.PRWorktreePath(absolute, number) {
		t.Fatalf("relative root yielded wrong path: %q", result.Worktree)
	}
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		marker := workspace.ActiveMarkerStart + "\nTemporary PR pointer\n" + workspace.ActiveMarkerEnd
		if err = os.WriteFile(filepath.Join(result.Worktree, name), []byte(marker), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.Validate(context.Background(), number); err != nil {
		t.Fatal(err)
	}
	assertNoPRMarkers(t, result.Worktree)
}

func assertNoPRMarkers(t *testing.T, path string) {
	t.Helper()
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		content, err := os.ReadFile(filepath.Join(path, name))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if strings.Contains(string(content), workspace.ActiveMarkerStart) {
			t.Fatalf("%s retained ephemeral marker after PR work", name)
		}
	}
}

func TestPRSubmitRejectsUnrecordedRemoteUpdate(t *testing.T) {
	s, issue, number, base := fixture(t)
	ctx := context.Background()
	cmd := exec.Command("git", "-C", s.Root, "commit-tree", base+"^{tree}", "-p", base, "-m", "External PR update")
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("external commit: %v: %s", err, raw)
	}
	external := strings.TrimSpace(string(raw))
	if err := s.Tracker.(*tracker.LocalTracker).PublishPRHead(ctx, number, base, external); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(ctx, number); err == nil {
		t.Fatal("submit blessed an unrecorded remote update without PR work or validation")
	}
	state, err := s.Manager.GetCurrentState(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	if state.HeadSHA != base || state.PRRevision != nil || state.Lease.Status != "completed" {
		t.Fatalf("rejected submit changed authoritative checkpoint: %+v", state)
	}
}
