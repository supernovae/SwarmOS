package pr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workflow"
	"github.com/supernovae/swarmos/internal/workspace"
)

// fixture submits a linked PR; a policy argument replaces the trusted
// required checks committed at its base.
func fixture(t *testing.T, policy ...string) (*Service, int, int, string) {
	t.Helper()
	t.Setenv("SWARM_EXECUTION_MODE", "trusted-local")
	controlHome := t.TempDir()
	t.Setenv("HOME", controlHome)
	t.Setenv("XDG_CONFIG_HOME", controlHome)
	t.Setenv("APPDATA", controlHome)
	root := t.TempDir()
	files := map[string]string{
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
	}
	for _, p := range policy {
		files[".swarm/validation/required-checks.yaml"] = p
	}
	for name, body := range files {
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
	if e = lease.SaveClaimToken(root, issue.Number, state.Lease.ClaimID, state.BaseSHA); e != nil {
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
	r, e := s.withClaim(ctx, &Result{PR: &tracker.PullRequest{Number: number, HeadSHA: base}, IssueNumber: issue}, false, nil, func(ctx context.Context, owner string, st *lease.IssueState) (*Result, error) {
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
	if r, e := s.Status(ctx, number); e != nil || r.Status != "closed-unmerged" {
		t.Fatalf("closed PR status unavailable: %+v %v", r, e)
	}
	if _, _, err = s.openForMutation(ctx, number, false); err == nil {
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
	// Work from the fixture's parent: on Windows the checkout and temp
	// directories can be on different volumes, which have no relative path.
	t.Chdir(filepath.Dir(absolute))
	relative := filepath.Base(absolute)
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

// hintTracker presents PRs the way GitHub does: no tracker-recorded issue
// link, only the editable PR body.
type hintTracker struct {
	tracker.Tracker
	body  string
	lists int
}

func (h *hintTracker) GetPullRequest(ctx context.Context, number int) (*tracker.PullRequest, error) {
	pr, err := h.Tracker.GetPullRequest(ctx, number)
	if pr != nil {
		copy := *pr
		copy.IssueNumber, copy.Body = 0, h.body
		pr = &copy
	}
	return pr, err
}

func (h *hintTracker) ListIssues(ctx context.Context, filter tracker.IssueFilter) ([]*tracker.Issue, error) {
	h.lists++
	return h.Tracker.ListIssues(ctx, filter)
}

func TestLinkedUsesVerifiedHintsBeforeScanning(t *testing.T) {
	s, issue, number, _ := fixture(t)
	ctx := context.Background()
	for _, tc := range []struct {
		body      string
		wantLists int
	}{
		{body: "Closes #" + strconv.Itoa(issue) + "\n\n## Objective", wantLists: 0},
		{body: "Closes #1\n", wantLists: 1}, // hint names an issue that does not record this PR
		{body: "no closing reference", wantLists: 1},
	} {
		hinted := &hintTracker{Tracker: s.Tracker, body: tc.body}
		svc := *s
		svc.Tracker = hinted
		r, err := svc.linked(ctx, number)
		if err != nil || r.IssueNumber != issue {
			t.Fatalf("body %q: linked = %+v, %v", tc.body, r, err)
		}
		if hinted.lists != tc.wantLists {
			t.Fatalf("body %q: scanned issues %d times, want %d", tc.body, hinted.lists, tc.wantLists)
		}
	}
}

func TestRecordedValidationChecksMatchPublishedHead(t *testing.T) {
	head, other := strings.Repeat("a", 40), strings.Repeat("b", 40)
	st := &lease.IssueState{Artifacts: []contract.Artifact{
		{Type: contract.ArtifactValidation, Verdict: "FAIL", Metadata: map[string]string{"head_sha": head}},
		{Type: contract.ArtifactValidation, Verdict: "PASS", Metadata: map[string]string{"head_sha": head}},
		{Type: contract.ArtifactValidation, Verdict: "FAIL", Metadata: map[string]string{"head_sha": other}},
	}}
	got := recordedValidationChecks(st, head)
	if len(got) != 1 || got[0].Name != RecordedValidationCheck || got[0].Conclusion != "success" {
		t.Fatalf("checks for published head = %+v", got)
	}
	if got := recordedValidationChecks(st, strings.Repeat("c", 40)); got != nil {
		t.Fatalf("unvalidated head reported checks: %+v", got)
	}
}

// upstreamTracker presents the local fixture as a GitHub upstream so the
// repository-identity and remote-selection paths run.
type upstreamTracker struct {
	tracker.Tracker
	upstream, headRepo, headSHA string
}

func (u *upstreamTracker) UpstreamRepo() string { return u.upstream }

func (u *upstreamTracker) GetPullRequest(ctx context.Context, number int) (*tracker.PullRequest, error) {
	pr, err := u.Tracker.GetPullRequest(ctx, number)
	if pr != nil {
		copy := *pr
		copy.BaseRepo, copy.HeadRepo = u.upstream, u.upstream
		if u.headRepo != "" {
			copy.HeadRepo = u.headRepo
		}
		if u.headSHA != "" {
			copy.HeadSHA = u.headSHA
		}
		pr = &copy
	}
	return pr, err
}

func TestRemoteSelectsUpstreamOrVerifiedForkHead(t *testing.T) {
	s, _, number, _ := fixture(t)
	ctx := context.Background()
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", s.Root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("remote", "add", "origin", "https://github.com/octo/swarm.git")
	git("remote", "add", "fork", "https://github.com/bob/swarm.git")
	pr, err := s.Tracker.GetPullRequest(ctx, number)
	if err != nil {
		t.Fatal(err)
	}
	upstream := &upstreamTracker{Tracker: s.Tracker, upstream: "octo/swarm"}
	svc := *s
	svc.Tracker = upstream
	same := *pr
	same.HeadRepo = "octo/swarm"
	if remote, err := svc.remote(ctx, &Result{PR: &same}); err != nil || remote != "origin" {
		t.Fatalf("same-repository PR remote = %q, %v", remote, err)
	}
	forked := *pr
	forked.HeadRepo = "alice/swarm"
	if _, err := svc.remote(ctx, &Result{PR: &forked}); err == nil || !strings.Contains(err.Error(), "explicit writable remote") {
		t.Fatalf("fork head without a remote = %v", err)
	}
	svc.ForkRemote = "fork"
	if _, err := svc.remote(ctx, &Result{PR: &forked}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("fork head with another fork's remote = %v", err)
	}
	git("remote", "set-url", "fork", "https://github.com/alice/swarm.git")
	if remote, err := svc.remote(ctx, &Result{PR: &forked}); err != nil || remote != "fork" {
		t.Fatalf("verified fork head remote = %q, %v", remote, err)
	}
	svc.Fork = true
	if _, err := svc.remote(ctx, &Result{PR: &same}); err == nil {
		t.Fatal("contributor --fork mode selected a PR mutation remote")
	}
}

func TestLinkedReportsRevisionPublicationStatus(t *testing.T) {
	s, issue, number, base := fixture(t)
	ctx := context.Background()
	local := strings.Repeat("b", 40)
	if _, err := s.withClaim(ctx, &Result{PR: &tracker.PullRequest{Number: number, HeadSHA: base}, IssueNumber: issue}, false, nil, func(ctx context.Context, owner string, st *lease.IssueState) (*Result, error) {
		_, err := s.Manager.BeginPRRevision(ctx, issue, owner, number, base, local, "swarm/pr-1")
		return nil, err
	}); err != nil {
		t.Fatal(err)
	}
	for head, want := range map[string]string{
		base:                    "pending",
		local:                   "published-pending-reconciliation",
		strings.Repeat("c", 40): "diverged",
	} {
		svc := *s
		svc.Tracker = &upstreamTracker{Tracker: s.Tracker, upstream: "octo/swarm", headSHA: head}
		r, err := svc.linked(ctx, number)
		if err != nil || r.Status != want {
			t.Fatalf("PR head %s: status = %+v, %v; want %s", head[:7], r, err, want)
		}
	}
	svc := *s
	svc.Tracker = &upstreamTracker{Tracker: s.Tracker, upstream: "octo/swarm", headSHA: strings.Repeat("c", 40)}
	if _, err := svc.Validate(ctx, number); err == nil || !strings.Contains(err.Error(), "changed outside the recorded revision") {
		t.Fatalf("validate on diverged PR = %v", err)
	}
	if _, err := svc.Submit(ctx, number); err == nil || !strings.Contains(err.Error(), "changed outside the recorded revision") {
		t.Fatalf("submit on diverged PR = %v", err)
	}
}

// loopbackGateway applies contributor requests directly to an upstream manager.
// It stands in for the fork coordinator (whose GitHub-side verification is
// covered in internal/fork) so the contributor's client wiring runs end to end.
type loopbackGateway struct {
	t          *testing.T
	upstream   *lease.Manager
	actor      string
	operations []string
	contexts   []lease.RebaseContext
}

func (g *loopbackGateway) Apply(ctx context.Context, req lease.ForkRequest) (*lease.IssueState, error) {
	g.operations = append(g.operations, req.Operation)
	if req.Operation == "acquire-pr" && req.Agent != "mock" {
		g.t.Errorf("acquire-pr requested agent %q, want the configured default agent", req.Agent)
	}
	up, n, actor := g.upstream, req.IssueNumber, g.actor
	if req.ClaimID != "" {
		up.BindClaim(n, req.ClaimID)
	}
	switch req.Operation {
	case "acquire-pr":
		return up.AcquirePR(ctx, n, req.PRNumber, actor, req.Agent, req.Phase, time.Hour, 3)
	case "begin-pr-revision":
		if req.RevisionIntent != "" {
			return up.BeginPRRevision(ctx, n, actor, req.PRNumber, req.ExpectedHeadSHA, req.HeadSHA, req.Branch, req.RevisionIntent)
		}
		return up.BeginPRRevision(ctx, n, actor, req.PRNumber, req.ExpectedHeadSHA, req.HeadSHA, req.Branch)
	case "record-pr-head":
		return up.RecordPRHead(ctx, n, actor, req.PRNumber, req.Branch, req.HeadSHA)
	case "invalidate-pr-evidence":
		// Mirrors the coordinator: rebase context is replayed, never trusted as-is.
		if req.RebaseContext != nil {
			g.contexts = append(g.contexts, *req.RebaseContext)
			return up.UpdateRebaseContext(ctx, n, actor, req.PRNumber, *req.RebaseContext)
		}
		if req.RevisionIntent != "" {
			return up.InvalidatePRRevisionEvidence(ctx, n, actor, req.PRNumber, req.RevisionIntent)
		}
		return up.InvalidatePRRevisionEvidence(ctx, n, actor, req.PRNumber)
	case "complete-pr-revision":
		return up.CompletePRRevisionLocal(ctx, n, actor, req.PRNumber, req.ExpectedHeadSHA, req.HeadSHA)
	case "update-artifact":
		return up.UpdateArtifact(ctx, n, actor, *req.Artifact)
	case "update-phase":
		return up.UpdatePhase(ctx, n, actor, req.Phase, req.Status)
	case "renew":
		return up.Renew(ctx, n, actor, time.Hour)
	case "require-gate":
		return up.RequireGate(ctx, n, actor, req.Gate)
	case "handoff":
		return up.Handoff(ctx, n, actor, req.Phase, req.NextPhase)
	case "release":
		return up.Release(ctx, n, actor, req.Reason)
	default:
		g.t.Fatalf("unexpected contributor operation %q", req.Operation)
		return nil, nil
	}
}

// forkContributor re-records the fixture's submission as a fork contribution
// whose head lives in alice/swarm and returns the contributor's service.
func forkContributor(t *testing.T, s *Service, issue, number int, base string) (*Service, *loopbackGateway, *forkPRTracker, func() string) {
	t.Helper()
	ctx := context.Background()
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	// Re-record the submission as a fork contribution whose head lives in alice/swarm.
	if _, err := s.Manager.ApproveTask(ctx, issue, "alice"); err != nil {
		t.Fatal(err)
	}
	st, err := s.Manager.GetCurrentState(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	forked, err := s.Manager.AcquireFork(ctx, issue, "alice", "mock", "all", "alice/swarm", time.Hour, 3)
	if err != nil {
		t.Fatal(err)
	}
	branch := workspace.ForkBranchName(issue, forked.Lease.ClaimID)
	forkBare := filepath.Join(t.TempDir(), "alice-swarm.git")
	git(s.Root, "init", "-q", "--bare", forkBare)
	git(s.Root, "remote", "add", "fork", forkBare)
	git(s.Root, "config", "remote.fork.swarmSlug", "alice/swarm")
	git(s.Root, "push", "-q", "fork", base+":refs/heads/"+branch)
	if _, err = s.Manager.RecordCheckpoint(ctx, issue, "alice", branch, base); err != nil {
		t.Fatal(err)
	}
	for _, a := range []contract.Artifact{{Type: contract.ArtifactDesign, Body: "original fork plan", Producer: "mock", Summary: "original"}, {Type: contract.ArtifactTestPlan, Body: "original fork tests", Producer: "mock", Summary: "original tests"}} {
		if _, err = s.Manager.UpdateArtifact(ctx, issue, "alice", a); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.Manager.RecordSubmission(ctx, issue, "alice", number, st.PRURL); err != nil {
		t.Fatal(err)
	}
	forkHead := func() string { return strings.Fields(git(s.Root, "ls-remote", forkBare, "refs/heads/"+branch))[0] }
	contributorView := &forkPRTracker{Tracker: s.Tracker, branch: branch, head: forkHead}

	gateway := &loopbackGateway{t: t, upstream: s.Manager, actor: "alice"}
	contributor := lease.NewManager(contributorView, contract.SecurityConfig{RequireReadyLabel: true, ReadyLabel: "swarm:ready"})
	contributor.SetStore(s.Root, s.Manager.Store())
	contributor.SetWorkerGateway(gateway)
	svc := &Service{Root: s.Root, Tracker: contributorView, Manager: contributor, Fork: true, ForkRemote: "fork"}
	return svc, gateway, contributorView, forkHead
}

func TestForkContributorRevisesOwnPR(t *testing.T) {
	s, issue, number, base := fixture(t)
	ctx := context.Background()
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	svc, gateway, contributorView, forkHead := forkContributor(t, s, issue, number, base)
	branch := contributorView.branch

	if r, err := svc.Status(ctx, number); err != nil || r.Status != "open" {
		t.Fatalf("contributor status = %+v, %v", r, err)
	}
	if len(contributorView.reviewRequests) != 0 {
		t.Fatal("status fetched review bodies")
	}
	if _, err := svc.Work(ctx, Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock", InspectOnly: true}}); err != nil {
		t.Fatal(err)
	}
	if len(contributorView.reviewRequests) != 0 {
		t.Fatal("dry-run fetched review bodies")
	}
	worked, err := svc.Work(ctx, Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock"}})
	if err != nil {
		t.Fatalf("contributor pr work: %v", err)
	}
	if worked.State.RevisionPlan == nil || worked.State.RevisionPlan.Original.Plan != "original fork plan" {
		t.Fatal("coordinator original missing in fork response")
	}
	if len(contributorView.reviewRequests) != 1 || contributorView.reviewRequests[0] != number {
		t.Fatal("wrong upstream review identity")
	}
	report, err := os.ReadFile(filepath.Join(worked.Worktree, ".swarm/active/PR_REVIEW_EVIDENCE.md"))
	if err != nil || !strings.Contains(string(report), "> fork review finding") {
		t.Fatal(string(report), err)
	}
	if worked.HeadSHA == base {
		t.Fatal("contributor revision produced no new commit")
	}
	// Upstream main advancing after approval must not block the revision.
	contributorView.baseTip = strings.Repeat("d", 40)
	submitted, err := svc.Submit(ctx, number)
	if err != nil {
		t.Fatalf("contributor pr submit: %v", err)
	}
	if forkHead() != worked.HeadSHA || submitted.HeadSHA != worked.HeadSHA {
		t.Fatalf("fork branch %s, submitted %s, want %s", forkHead(), submitted.HeadSHA, worked.HeadSHA)
	}
	final, err := s.Manager.GetCurrentState(ctx, issue)
	if err != nil || final.PRRevision != nil || final.HeadSHA != worked.HeadSHA || final.ForkRepo != "alice/swarm" {
		t.Fatalf("upstream state after contributor revision = %+v, %v", final, err)
	}
	for _, op := range []string{"acquire-pr", "begin-pr-revision", "record-pr-head", "complete-pr-revision"} {
		if !slices.Contains(gateway.operations, op) {
			t.Fatalf("contributor flow never sent %s through the coordinator: %v", op, gateway.operations)
		}
	}

	// A push outside SwarmOS leaves the PR head unverified.
	git(s.Root, "push", "-q", "fork", base+":refs/heads/"+branch, "--force")
	if r, err := svc.Status(ctx, number); err != nil || r.Status != StatusUnverifiedHead {
		t.Fatalf("status after an out-of-band push = %+v, %v", r, err)
	}
}

// forkPRTracker presents the fixture's PR as GitHub would for a fork
// contribution: head in alice/swarm, base in the upstream repository.
type forkPRTracker struct {
	reviewRequests []int
	tracker.Tracker
	branch string
	head   func() string
	// baseTip and target, when set, report the target branch advancing or the
	// PR being retargeted.
	baseTip, target string
}

func (f *forkPRTracker) GetReviewEvidence(ctx context.Context, number int) (tracker.ReviewEvidence, error) {
	f.reviewRequests = append(f.reviewRequests, number)
	return tracker.ReviewEvidence{Records: []tracker.ReviewEvidenceRecord{{Body: "fork review finding", CommitSHA: "older"}}}, nil
}

func (f *forkPRTracker) UpstreamRepo() string { return "octo/swarm" }

func (f *forkPRTracker) GetPullRequest(ctx context.Context, number int) (*tracker.PullRequest, error) {
	pr, err := f.Tracker.GetPullRequest(ctx, number)
	if pr != nil {
		copy := *pr
		copy.BaseRepo, copy.HeadRepo, copy.Head, copy.HeadSHA = "octo/swarm", "alice/swarm", f.branch, f.head()
		if f.baseTip != "" {
			copy.BaseSHA = f.baseTip
		}
		if f.target != "" {
			copy.Base = f.target
		}
		pr = &copy
	}
	return pr, err
}

// laggingTracker reports a PR's previous head for a few reads, as GitHub does
// briefly after a push.
type laggingTracker struct {
	tracker.Tracker
	stale string
	reads int
}

func (l *laggingTracker) GetPullRequest(ctx context.Context, number int) (*tracker.PullRequest, error) {
	pr, err := l.Tracker.GetPullRequest(ctx, number)
	if pr != nil && l.reads > 0 {
		l.reads--
		copy := *pr
		copy.HeadSHA = l.stale
		pr = &copy
	}
	return pr, err
}

func TestAwaitPublishedHeadToleratesGitHubLag(t *testing.T) {
	s, _, number, base := fixture(t)
	publishPollInterval = time.Millisecond
	t.Cleanup(func() { publishPollInterval = 2 * time.Second })
	lagging := &laggingTracker{Tracker: s.Tracker, stale: strings.Repeat("e", 40), reads: 3}
	svc := *s
	svc.Tracker = lagging
	observed, err := svc.awaitPublishedHead(context.Background(), number, base)
	if err != nil || observed.HeadSHA != base || lagging.reads != 0 {
		t.Fatalf("awaited head = %+v, %v (stale reads left %d)", observed, err, lagging.reads)
	}
	lagging.reads = 1 << 30
	old := PublishVisibilityTimeout
	PublishVisibilityTimeout = 20 * time.Millisecond
	t.Cleanup(func() { PublishVisibilityTimeout = old })
	if observed, err = svc.awaitPublishedHead(context.Background(), number, base); err != nil || observed.HeadSHA == base {
		t.Fatalf("wait did not give up on a head that never appears: %+v, %v", observed, err)
	}
}

type terminalTracker struct {
	tracker.Tracker
	pr *tracker.PullRequest
}

func (t terminalTracker) GetPullRequest(context.Context, int) (*tracker.PullRequest, error) {
	return t.pr, nil
}
func (t terminalTracker) GetCheckStatus(context.Context, string) ([]tracker.CheckRunResult, error) {
	return nil, fmt.Errorf("terminal status must not query checks")
}
func TestMergedStatusAndClosedStatusRemainReadOnly(t *testing.T) {
	for _, merged := range []bool{false, true} {
		t.Run(strconv.FormatBool(merged), func(t *testing.T) {
			s, issue, n, _ := fixture(t)
			ctx := context.Background()
			before, e := s.Manager.GetCurrentState(ctx, issue)
			if e != nil {
				t.Fatal(e)
			}
			p, e := s.Tracker.GetPullRequest(ctx, n)
			if e != nil {
				t.Fatal(e)
			}
			p.State = "closed"
			p.Merged = merged
			s.Tracker = terminalTracker{Tracker: s.Tracker, pr: p}
			r, e := s.Status(ctx, n)
			if e != nil {
				t.Fatal(e)
			}
			expected := "closed-unmerged"
			if merged {
				expected = "merged"
			}
			if r.Status != expected || r.Worktree != "" {
				t.Fatalf("terminal status: %+v", r)
			}
			if _, e = s.Work(ctx, Options{Number: n}); e == nil {
				t.Fatal("terminal PR allowed work")
			}
			after, e := s.Manager.GetCurrentState(ctx, issue)
			if e != nil {
				t.Fatal(e)
			}
			a, _ := json.Marshal(before)
			b, _ := json.Marshal(after)
			if string(a) != string(b) {
				t.Fatal("status mutated state")
			}
		})
	}
}

func TestForkContributorResolvesRebaseConflictsAsReports(t *testing.T) {
	s, issue, number, base := fixture(t)
	ctx := context.Background()
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if _, err := s.Manager.ApproveTask(ctx, issue, "alice"); err != nil {
		t.Fatal(err)
	}
	st, err := s.Manager.GetCurrentState(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	forked, err := s.Manager.AcquireFork(ctx, issue, "alice", "mock", "all", "alice/swarm", time.Hour, 3)
	if err != nil {
		t.Fatal(err)
	}
	branch := workspace.ForkBranchName(issue, forked.Lease.ClaimID)
	forkBare := filepath.Join(t.TempDir(), "alice-swarm.git")
	git(s.Root, "init", "-q", "--bare", forkBare)
	git(s.Root, "remote", "add", "fork", forkBare)
	git(s.Root, "config", "remote.fork.swarmSlug", "alice/swarm")
	git(s.Root, "push", "-q", "fork", base+":refs/heads/"+branch)
	if _, err = s.Manager.RecordCheckpoint(ctx, issue, "alice", branch, base); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Manager.RecordSubmission(ctx, issue, "alice", number, st.PRURL); err != nil {
		t.Fatal(err)
	}
	forkHead := func() string { return strings.Fields(git(s.Root, "ls-remote", forkBare, "refs/heads/"+branch))[0] }
	contributorView := &forkPRTracker{Tracker: s.Tracker, branch: branch, head: forkHead}
	gateway := &loopbackGateway{t: t, upstream: s.Manager, actor: "alice"}
	contributor := lease.NewManager(contributorView, contract.SecurityConfig{RequireReadyLabel: true, ReadyLabel: "swarm:ready"})
	contributor.SetStore(s.Root, s.Manager.Store())
	contributor.SetWorkerGateway(gateway)
	svc := &Service{Root: s.Root, Tracker: contributorView, Manager: contributor, Fork: true, ForkRemote: "fork"}

	t.Setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'fork change\\n' > conflict.txt; printf 'fork other\\n' > other.txt")
	if _, err := svc.Work(ctx, Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock"}}); err != nil {
		t.Fatalf("contributor pr work: %v", err)
	}
	if _, err := svc.Submit(ctx, number); err != nil {
		t.Fatalf("contributor pr submit: %v", err)
	}

	// Upstream moves to a base that conflicts with the contribution and is re-approved.
	if err := os.WriteFile(filepath.Join(s.Root, "conflict.txt"), []byte("upstream change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Upstream absorbs other.txt verbatim, so the PR no longer changes it.
	if err := os.WriteFile(filepath.Join(s.Root, "other.txt"), []byte("fork other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(s.Root, "add", "conflict.txt", "other.txt")
	git(s.Root, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-q", "-m", "upstream conflict")
	if _, err := s.Manager.ApproveTask(ctx, issue, "alice"); err != nil {
		t.Fatal(err)
	}
	contributorView.baseTip = git(s.Root, "rev-parse", "HEAD")

	t.Setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'resolved\\n' > conflict.txt")
	gateway.operations = nil
	rebased, err := svc.Rebase(ctx, Options{Number: number, Resolve: true, Work: workflow.WorkOptions{AgentName: "mock", Headless: true}})
	if err != nil {
		t.Fatalf("contributor rebase --resolve: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(rebased.Worktree, "conflict.txt")); string(data) != "resolved\n" {
		t.Fatalf("resolution not imported: %q", data)
	}
	sent := false
	for _, c := range gateway.contexts {
		sent = sent || c.Resolution != nil
	}
	if !slices.Contains(gateway.operations, "invalidate-pr-evidence") || !sent {
		t.Fatalf("resolution never went through the coordinator: ops=%v contexts=%+v", gateway.operations, gateway.contexts)
	}
	upstream, err := s.Manager.GetCurrentState(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	r := upstream.LastRebase
	if r == nil || r.Status != "completed" || len(r.Resolutions) != 1 || !r.Resolutions[0].Reported || r.Verified {
		t.Fatalf("upstream rebase evidence = %+v", r)
	}
	if summary := RebaseSummary(upstream, number); !strings.Contains(summary, "Reported resolved by mock") {
		t.Fatalf("summary does not label the report:\n%s", summary)
	}
	// Upstream cannot inspect the unpublished head; the contributor's own
	// coordinator reports the comparison, labeled as local.
	if r.DroppedPathsVerified || len(r.DroppedPaths) != 0 || !strings.Contains(RebaseSummary(upstream, number), "Changed-path comparison: unavailable") {
		t.Fatalf("upstream claimed an unverifiable comparison: %+v", r)
	}
	if local := rebased.LocalDroppedPaths; local == nil || local.Source == "" || !slices.Equal(local.Paths, []string{"other.txt"}) {
		t.Fatalf("local dropped-path diagnostics = %+v", local)
	}
	status, err := svc.Status(ctx, number)
	if err != nil {
		t.Fatal(err)
	}
	if status.LocalDroppedPaths == nil || !slices.Equal(status.LocalDroppedPaths.Paths, []string{"other.txt"}) {
		t.Fatalf("status local diagnostics = %+v", status.LocalDroppedPaths)
	}
	if _, err = svc.Submit(ctx, number); err != nil {
		t.Fatalf("contributor submit after rebase: %v", err)
	}
	published, err := s.Manager.GetCurrentState(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	if r := published.LastRebase; r == nil || !r.DroppedPathsVerified || !slices.Equal(r.DroppedPaths, []string{"other.txt"}) {
		t.Fatalf("published upstream evidence = %+v", r)
	}
	if summary := RebaseSummary(published, number); !strings.Contains(summary, "Warning: no longer changed: other.txt\n") || !strings.Contains(summary, "swarm --fork pr work "+strconv.Itoa(number)) {
		t.Fatalf("published summary:\n%s", summary)
	}
}

func TestRebaseRoutineReapprovalRejectsChangedAuthority(t *testing.T) {
	for _, kind := range []string{"non-maintainer", "policy", "task", "active", "stale base", "stale head"} {
		t.Run(kind, func(t *testing.T) {
			s, issue, number, base := fixture(t)
			ctx := context.Background()
			path := "upstream.txt"
			if kind == "policy" {
				path = ".swarm/project.md"
			}
			if err := os.WriteFile(filepath.Join(s.Root, path), []byte("target change\n"), 0644); err != nil {
				t.Fatal(err)
			}
			command := exec.Command("git", "-C", s.Root, "add", path)
			if out, err := command.CombinedOutput(); err != nil {
				t.Fatalf("add: %v %s", err, out)
			}
			command = exec.Command("git", "-C", s.Root, "commit", "-qm", "target")
			if out, err := command.CombinedOutput(); err != nil {
				t.Fatalf("commit: %v %s", err, out)
			}
			pr, err := s.Tracker.GetPullRequest(ctx, number)
			if err != nil {
				t.Fatal(err)
			}
			m := s.Manager
			if kind == "non-maintainer" {
				tr := s.Tracker.(*tracker.LocalTracker).WithUser("bob", "CONTRIBUTOR")
				m = lease.NewManager(tr, contract.SecurityConfig{})
				m.SetStore(s.Root, s.Manager.Store())
			}
			if kind == "task" {
				i, e := s.Tracker.GetIssue(ctx, issue)
				if e != nil {
					t.Fatal(e)
				}
				if _, e = s.Tracker.UpdateIssueBody(ctx, issue, strings.Replace(i.Body, "Revise linked PR", "Different task", 1)); e != nil {
					t.Fatal(e)
				}
			}
			if kind == "active" {
				if _, err = m.AcquirePR(ctx, issue, number, "alice", "mock", "all", time.Hour, 3); err != nil {
					t.Fatal(err)
				}
			}
			expectedBase, expectedHead := base, pr.HeadSHA
			if kind == "stale base" {
				expectedBase = strings.Repeat("0", 40)
			}
			if kind == "stale head" {
				expectedHead = strings.Repeat("0", 40)
			}
			before, err := m.GetCurrentState(ctx, issue)
			if err != nil {
				t.Fatal(err)
			}
			_, err = m.ReapproveRebase(ctx, issue, number, expectedBase, pr.BaseSHA, expectedHead)
			if err == nil {
				t.Fatal("unsafe approval accepted")
			}
			after, e := m.GetCurrentState(ctx, issue)
			if e != nil {
				t.Fatal(e)
			}
			b, _ := json.Marshal(before)
			a, _ := json.Marshal(after)
			if string(b) != string(a) {
				t.Fatal("rejected approval changed state")
			}
		})
	}
}

type auditRetryTracker struct {
	tracker.Tracker
	failAudit bool
}

func (t *auditRetryTracker) CreateComment(ctx context.Context, n int, body string) (*tracker.Comment, error) {
	if t.failAudit && strings.Contains(body, "re-pinned") {
		return nil, fmt.Errorf("audit unavailable")
	}
	return t.Tracker.CreateComment(ctx, n, body)
}

func TestRebaseApprovalAuditRetriesWithoutReapproval(t *testing.T) {
	s, issue, number, base := fixture(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(s.Root, "upstream.txt"), []byte("target\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "upstream.txt"}, {"commit", "-qm", "target"}} {
		cmd := exec.Command("git", append([]string{"-C", s.Root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	tr := &auditRetryTracker{Tracker: s.Tracker, failAudit: true}
	m := lease.NewManager(tr, contract.SecurityConfig{})
	m.SetStore(s.Root, s.Manager.Store())
	pr, err := tr.GetPullRequest(ctx, number)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.ReapproveRebase(ctx, issue, number, base, pr.BaseSHA, pr.HeadSHA); err == nil || !strings.Contains(err.Error(), "audit") {
		t.Fatalf("missing audit failure: %v", err)
	}
	approved, err := m.GetCurrentState(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	if approved.BaseSHA != pr.BaseSHA || approved.RebaseApprovalAudit == "" {
		t.Fatal("approval/audit checkpoint missing")
	}
	tr.failAudit = false
	projected, err := m.ProjectRebaseApprovalAudit(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	if projected.RebaseApprovalAudit != "" || projected.BaseSHA != approved.BaseSHA || projected.ApprovedBy != approved.ApprovedBy || !projected.ApprovedAt.Equal(approved.ApprovedAt) {
		t.Fatal("audit retry changed approval")
	}
}

func TestPRSetupProgressBeforePreparation(t *testing.T) {
	s, issue, number, _ := fixture(t)
	var events []workflow.ProgressEvent
	reporter := workflow.NewProgressReporter(t.Context(), func(event workflow.ProgressEvent) {
		// Actual PR provisioning has not happened when workspace starts.
		if event.Stage == "workspace" && event.Status == "started" {
			if _, err := os.Stat(workspace.PRWorktreePath(s.Root, number)); !os.IsNotExist(err) {
				t.Errorf("workspace progress after preparation: %v", err)
			}
		}
		if event.IssueNumber != issue {
			t.Errorf("wrong issue: %+v", event)
		}
		events = append(events, event)
	})
	defer reporter.Close()
	_, err := s.Work(t.Context(), Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock", Phase: "plan", SetupProgress: reporter}})
	reporter.Close()
	if err != nil {
		t.Fatal(err)
	}
	var phases []string
	for _, event := range events {
		if event.Status == "started" {
			phases = append(phases, event.Stage)
		}
	}
	if strings.Join(phases, ",") != "claim,workspace,claim,bundle" {
		t.Fatalf("setup phases: %v", phases)
	}
}

func TestServiceCarriesLookupIntoEnginesAndRunners(t *testing.T) {
	t.Setenv("SWARM_EXECUTION_MODE", contract.ExecutionIsolated)
	trusted := func(name string) (string, bool) {
		if name == "SWARM_EXECUTION_MODE" {
			return contract.ExecutionTrustedLocal, true
		}
		return "", false
	}
	s := &Service{Root: t.TempDir(), Tracker: tracker.NewLocalTracker(t.TempDir()), LookupEnv: trusted, NodeProfilePath: filepath.Join(t.TempDir(), "node.yaml")}
	cfg := &contract.SwarmConfig{Agents: map[string]contract.AgentConfig{}}
	if engine := s.newEngine(cfg); engine.LookupEnv == nil {
		t.Fatal("engine lost the service lookup")
	} else if mode, _ := engine.LookupEnv("SWARM_EXECUTION_MODE"); mode != contract.ExecutionTrustedLocal {
		t.Fatalf("engine mode = %q", mode)
	}
	if _, err := agent.NewConfiguredRunnerWithOptions("mock", cfg, true, s.runnerOptions()); err != nil {
		t.Fatalf("service runner options ignored the injected mode: %v", err)
	}
	if _, err := agent.NewConfiguredRunnerWithOptions("mock", cfg, true, (&Service{}).runnerOptions()); err == nil {
		t.Fatal("default service runner options ignored the process execution mode")
	}
}
