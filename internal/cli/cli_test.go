package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workflow"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type cliFixture struct {
	root    string
	tracker *tracker.LocalTracker
	manager *lease.Manager
}

func newCLIFixture(t *testing.T) *cliFixture {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())
	t.Setenv("SWARM_USER", "alice")
	t.Setenv("SWARM_REGISTRY_PATH", filepath.Join(t.TempDir(), "repos.yaml"))
	t.Setenv("SWARM_NODE_CONFIG", filepath.Join(t.TempDir(), "node.yaml"))
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-b", "main")
	git("config", "user.name", "CLI Test")
	git("config", "user.email", "test@example.invalid")
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	policy := "required_checks:\n  - name: git-available\n    command: git --version\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**']\n"
	if err := os.WriteFile(filepath.Join(root, ".swarm", "validation", "required-checks.yaml"), []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "approved harness")
	tr := tracker.NewLocalTracker(root)
	cfg, err := harness.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	manager := lease.NewManager(tr, cfg.Security)
	if err := manager.ConfigureStore(root); err != nil {
		t.Fatal(err)
	}
	return &cliFixture{root: root, tracker: tr, manager: manager}
}

func (f *cliFixture) run(args ...string) (string, error) {
	cmd := NewRootCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs(append([]string{"--root", f.root, "--backend", "local"}, args...))
	err := cmd.Execute()
	return output.String(), err
}
func (f *cliFixture) must(t *testing.T, args ...string) string {
	t.Helper()
	out, err := f.run(args...)
	if err != nil {
		t.Fatalf("swarm %v: %v\n%s", args, err, out)
	}
	return out
}
func (f *cliFixture) create(t *testing.T, gate string) int {
	t.Helper()
	f.must(t, "issue", "create", "--title", "Approved task", "--objective", "Exercise the lifecycle", "--constraint", "Keep policy unchanged", "--acceptance", "Produce a checked checkpoint", "--gate", gate)
	issues, err := f.tracker.ListIssues(context.Background(), tracker.IssueFilter{State: "open"})
	if err != nil {
		t.Fatal(err)
	}
	number := 0
	for _, issue := range issues {
		if issue.Number > number {
			number = issue.Number
		}
	}
	return number
}
func (f *cliFixture) state(t *testing.T, issue int) *lease.IssueState {
	t.Helper()
	state, err := f.manager.GetCurrentState(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestCLIApprovalWorkAndIdempotentSubmission(t *testing.T) {
	f := newCLIFixture(t)
	number := f.create(t, "agent")
	n := strconv.Itoa(number)
	issue, err := f.tracker.GetIssue(context.Background(), number)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(issue.Labels, "swarm:ready") {
		t.Fatal("new task was automatically ready")
	}
	if _, err := f.run("work", n, "--agent", "mock", "--headless"); err == nil {
		t.Fatal("unapproved work executed")
	}
	f.must(t, "issue", "approve", n)
	result := f.must(t, "work", n, "--agent", "mock", "--headless", "--json")
	var outcome struct {
		Status string              `json:"status"`
		Result workflow.WorkResult `json:"result"`
	}
	if err := json.Unmarshal([]byte(result), &outcome); err != nil {
		t.Fatalf("invalid JSON outcome: %v: %s", err, result)
	}
	if outcome.Status != "completed" || !outcome.Result.ValidationPassed {
		t.Fatalf("unexpected workflow outcome: %+v", outcome)
	}
	f.must(t, "submit", n)
	first := f.state(t, number)
	if first.PRNumber == 0 || first.Lease.Status != "completed" {
		t.Fatalf("submission not durably recorded: %+v", first)
	}
	f.must(t, "submit", n)
	second := f.state(t, number)
	if second.PRNumber != first.PRNumber || second.HeadSHA != first.HeadSHA {
		t.Fatal("retry changed the submission identity")
	}
	pr, err := f.tracker.FindDraftPR(context.Background(), first.Branch, "main")
	if err != nil || pr == nil || pr.Number != first.PRNumber {
		t.Fatalf("missing recorded draft PR: %v, %v", pr, err)
	}
}

func TestCLIHumanGateCannotBeBypassedByReviewOrSubmit(t *testing.T) {
	f := newCLIFixture(t)
	number := f.create(t, "human")
	n := strconv.Itoa(number)
	f.must(t, "issue", "approve", n)
	f.must(t, "work", n, "--agent", "mock", "--headless")
	var paused struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(f.must(t, "work", n, "--agent", "mock", "--json")), &paused); err != nil {
		t.Fatal(err)
	}
	if paused.Status != "paused" {
		t.Fatalf("retry before human approval must remain paused, got %q", paused.Status)
	}
	if _, err := f.run("submit", n); err == nil {
		t.Fatal("submission bypassed human approval")
	}
	f.must(t, "review", n, "--agent", "mock")
	if _, err := f.run("work", n, "--phase", "implement", "--gate", "none", "--agent", "mock"); err == nil {
		t.Fatal("agent review satisfied human gate")
	}
	t.Setenv("SWARM_USER", "independent-maintainer")
	f.must(t, "review", n, "--approve", "--notes", "Checked exact plan and test plan")
	t.Setenv("SWARM_USER", "alice")
	f.must(t, "work", n, "--agent", "mock", "--headless")
	f.must(t, "submit", n)
}

func TestCLISelectorsFailBeforeClaiming(t *testing.T) {
	f := newCLIFixture(t)
	number := f.create(t, "agent")
	for _, args := range [][]string{
		{"work", "1", "--next"}, {"work", "1", "--phase", "typo"}, {"work", "1", "--phase", "full"},
		{"work", "1", "--phase", "submitted"}, {"work", "1", "--gate", "typo"}, {"review", "1", "--approve", "--reject"},
		{"node", "run", "--concurrency", "0"}, {"node", "run", "--max-jobs", "-1"}, {"node", "run", "--poll-interval", "0s"},
	} {
		if _, err := f.run(args...); err == nil {
			t.Errorf("invalid selector accepted: %v", args)
		}
	}
	if _, err := f.manager.GetCurrentState(context.Background(), number); !errors.Is(err, coordination.ErrNotFound) {
		t.Fatalf("invalid selection changed state: %v", err)
	}
}

func TestNodeAdmissionLimitAndFailedExecutionStatus(t *testing.T) {
	f := newCLIFixture(t)
	for i := 0; i < 6; i++ {
		n := f.create(t, "agent")
		f.must(t, "issue", "approve", strconv.Itoa(n))
	}
	f.must(t, "node", "run", "--concurrency", "4", "--max-jobs", "1", "--agent", "mock")
	claimed := 0
	for i := 1; i <= 6; i++ {
		if f.state(t, i).Lease.ClaimID != "" {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("max-jobs=1 admitted %d tasks", claimed)
	}
	t.Setenv("SWARM_MOCK_DEVELOPER_CMD", "exit 17")
	if _, err := f.run("node", "run", "--max-jobs", "1", "--agent", "mock"); err == nil {
		t.Fatal("failed developer produced successful node exit")
	}
}

func TestEmptyNodeQueueIsSuccessfulDrain(t *testing.T) {
	f := newCLIFixture(t)
	f.must(t, "node", "run", "--agent", "mock")
}

func TestNodeHumanGateWaitsThenResumesImplementation(t *testing.T) {
	f := newCLIFixture(t)
	issue := f.create(t, "human")
	n := strconv.Itoa(issue)
	f.must(t, "issue", "approve", n)
	f.must(t, "node", "run", "--agent", "mock", "--max-jobs", "1")
	waiting := f.state(t, issue)
	if waiting.Lease.Status != "handoff" {
		t.Fatalf("human wait did not release worker claim: %+v", waiting.Lease)
	}
	f.must(t, "node", "run", "--agent", "mock", "--max-jobs", "1")
	if after := f.state(t, issue); after.Lease.ClaimID != waiting.Lease.ClaimID {
		t.Fatal("worker reclaimed a task blocked on human approval")
	}
	f.must(t, "review", n, "--approve")
	f.must(t, "node", "run", "--agent", "mock", "--max-jobs", "1", "--auto-submit")
	if completed := f.state(t, issue); completed.PRNumber == 0 || completed.Lease.Status != "completed" {
		t.Fatalf("approved human handoff did not reach submission: %+v", completed)
	}
}

func TestCLIRenewalLimitReturnsErrorWithoutChangingLease(t *testing.T) {
	f := newCLIFixture(t)
	number := f.create(t, "agent")
	n := strconv.Itoa(number)
	f.must(t, "issue", "approve", n)
	f.must(t, "claim", n, "--agent", "mock")
	for range f.state(t, number).Lease.MaxRenewals {
		f.must(t, "renew", n, "--json")
	}
	before := f.state(t, number).Lease
	if _, err := f.run("renew", n, "--json"); !errors.Is(err, lease.ErrMaxRenewals) {
		t.Fatalf("renewal limit must reach the caller: %v", err)
	}
	after := f.state(t, number).Lease
	if after.Renewals != before.Renewals || !after.ExpiresAt.Equal(before.ExpiresAt) {
		t.Fatalf("failed renewal changed the lease: before=%+v after=%+v", before, after)
	}
}

func TestCLIInteractiveStageProvenanceAndPlanCommand(t *testing.T) {
	f := newCLIFixture(t)
	number := f.create(t, "agent")
	n := strconv.Itoa(number)

	createdIss, err := f.tracker.GetIssue(context.Background(), number)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(createdIss.Labels, "swarm:draft") {
		t.Fatalf("expected newly created draft issue to have swarm:draft label, got %v", createdIss.Labels)
	}

	f.must(t, "issue", "approve", n)
	f.must(t, "claim", n, "--agent", "mock")

	wtPath := filepath.Join(f.root, ".swarm", "worktrees", "issue-"+n)
	activeDir := filepath.Join(wtPath, ".swarm", "active")
	if err := os.WriteFile(filepath.Join(activeDir, "PLAN.md"), []byte("# Interactive Plan\n\n1. Add feature.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(activeDir, "TEST_PLAN.md"), []byte("# Interactive Test Plan\n\n- Run git --version\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	f.must(t, "stage", n, "--phase", "plan", "--harness", "claude", "--model", "claude-sonnet-4-5")
	f.must(t, "review", n, "--agent", "mock")

	if err := os.WriteFile(filepath.Join(wtPath, "feature.txt"), []byte("shipped\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.must(t, "stage", n, "--phase", "implement", "--harness", "codex", "--model", "gpt-5-codex")
	f.must(t, "submit", n)

	state := f.state(t, number)
	pr, err := f.tracker.FindDraftPR(context.Background(), state.Branch, "main")
	if err != nil || pr == nil {
		t.Fatalf("FindDraftPR: %v, %v", pr, err)
	}
	for _, want := range []string{
		"## 🔒 Harness & Model Provenance Attestation",
		"Machine-Readable Provenance Attestation (JSON)",
		"claude-sonnet-4-5",
		"gpt-5-codex",
		"## 📦 Attached SwarmOS Artifacts",
		"# Interactive Plan",
		"# Interactive Test Plan",
		"VERDICT: APPROVED",
	} {
		if !strings.Contains(pr.Body, want) {
			t.Fatalf("Draft PR body missing %q:\n%s", want, pr.Body)
		}
	}
	if strings.Contains(pr.Body, "alice") {
		t.Fatalf("Draft PR body leaked local username 'alice':\n%s", pr.Body)
	}

	comments, err := f.tracker.ListComments(context.Background(), number)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 {
		t.Fatalf("expected 1 living projection comment on issue #%d, got %d", number, len(comments))
	}

	// Verify swarm plan --from preview vs --apply --json and --scan CLI commands
	roadmapFile := filepath.Join(f.root, "ROADMAP.md")
	roadmapContent := "# CLI Roadmap\n\n## First Roadmap Item\n### Objective\nBuild part one.\n### Acceptance Criteria\n- [ ] Check one\n"
	if err := os.WriteFile(roadmapFile, []byte(roadmapContent), 0o644); err != nil {
		t.Fatal(err)
	}
	previewOut := f.must(t, "plan", "--from", roadmapFile)
	if !strings.Contains(previewOut, "`Step 1`") || strings.Contains(previewOut, "`#1`") {
		t.Fatalf("expected preview output to use Step 1 and not fake #1:\n%s", previewOut)
	}
	planJSON := f.must(t, "plan", "--from", roadmapFile, "--apply", "--json")
	var parsedPlan struct {
		Kind   string                     `json:"kind"`
		Report workflow.ProjectPlanReport `json:"report"`
	}
	if err := json.Unmarshal([]byte(planJSON), &parsedPlan); err != nil {
		t.Fatalf("unmarshal plan JSON: %v\n%s", err, planJSON)
	}
	if parsedPlan.Report.EpicIssue == nil || !parsedPlan.Report.EpicCreated || !parsedPlan.Report.EpicIssue.IsEpic() {
		t.Fatalf("expected epic_issue (EpicCreated=true), got %+v", parsedPlan.Report)
	}
	if len(parsedPlan.Report.CreatedIssues) != 1 || len(parsedPlan.Report.ExistingIssues) != 0 {
		t.Fatalf("expected 1 created child and 0 existing, got %+v", parsedPlan.Report)
	}
	// Repeat apply --json: EpicCreated=false, CreatedIssues=0, ExistingIssues=1
	repeatJSON := f.must(t, "plan", "--from", roadmapFile, "--apply", "--json")
	var repeatPlan struct {
		Kind   string                     `json:"kind"`
		Report workflow.ProjectPlanReport `json:"report"`
	}
	if err := json.Unmarshal([]byte(repeatJSON), &repeatPlan); err != nil {
		t.Fatal(err)
	}
	if repeatPlan.Report.EpicCreated || len(repeatPlan.Report.CreatedIssues) != 0 || len(repeatPlan.Report.ExistingIssues) != 1 {
		t.Fatalf("expected repeat apply JSON to show reused epic and child, got %+v", repeatPlan.Report)
	}
	scanOut := f.must(t, "plan", "--scan", "--apply")
	if !strings.Contains(scanOut, "SwarmOS Backlog Sense-Making") {
		t.Fatalf("unexpected plan --scan output:\n%s", scanOut)
	}
}

type mockDoctorTracker struct {
	tracker.Tracker
	epicTypeErr error
	authErr     error
	created     int
}

func (m *mockDoctorTracker) CheckIssueType(_ context.Context, issueType string) error {
	return m.epicTypeErr
}

func (m *mockDoctorTracker) Authorize(_ context.Context, permission string) error {
	if permission == "create-epic" {
		return m.authErr
	}
	return nil
}

func (m *mockDoctorTracker) CreateIssue(ctx context.Context, title, body string, labels []string) (*tracker.Issue, error) {
	m.created++
	return m.Tracker.CreateIssue(ctx, title, body, labels)
}

func (m *mockDoctorTracker) CreateTypedIssue(ctx context.Context, title, body string, labels []string, issueType string) (*tracker.Issue, error) {
	m.created++
	return m.Tracker.CreateTypedIssue(ctx, title, body, labels, issueType)
}

func TestCLIEpicCommandsParentLinkingPullSkipAndDoctor(t *testing.T) {
	f := newCLIFixture(t)
	ctx := context.Background()

	bodyFile := filepath.Join(f.root, "EPIC.md")
	if err := os.WriteFile(bodyFile, []byte("# Standalone Epic\n\nHigh-level design.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	epicOut := f.must(t, "issue", "epic", "create", "--title", "Standalone Epic", "--body-file", bodyFile, "--json")
	var epicRes struct {
		Kind  string         `json:"kind"`
		Issue *tracker.Issue `json:"issue"`
	}
	if err := json.Unmarshal([]byte(epicOut), &epicRes); err != nil {
		t.Fatalf("unmarshal epic JSON: %v\n%s", err, epicOut)
	}
	if epicRes.Issue == nil || !epicRes.Issue.IsEpic() {
		t.Fatalf("expected created Epic issue, got %+v", epicRes)
	}
	epicNumStr := strconv.Itoa(epicRes.Issue.Number)

	// Epic cannot be approved or claimed
	if _, err := f.run("issue", "approve", epicNumStr); err == nil {
		t.Fatal("expected issue approve on Epic to fail")
	}
	if _, err := f.run("claim", epicNumStr); err == nil {
		t.Fatal("expected claim on Epic to fail")
	}

	// Create a child issue with --parent <epic>
	childOut := f.must(t, "issue", "create", "--parent", epicNumStr,
		"--title", "Child under Epic",
		"--objective", "Implement child work",
		"--constraint", "Stay within scope",
		"--acceptance", "Child tests pass",
		"--json")
	var childRes struct {
		Issue *tracker.Issue `json:"issue"`
	}
	if err := json.Unmarshal([]byte(childOut), &childRes); err != nil {
		t.Fatal(err)
	}
	if parentNum, err := f.tracker.ParentIssueNumber(ctx, childRes.Issue.Number); err != nil || parentNum != epicRes.Issue.Number {
		t.Fatalf("expected child #%d native parent #%d, got (%d, %v)", childRes.Issue.Number, epicRes.Issue.Number, parentNum, err)
	}

	// Even if an Epic is mislabeled swarm:ready, PullNextReadyIssue skips it and claims the approved child
	_ = f.tracker.AddLabels(ctx, epicRes.Issue.Number, []string{"swarm:ready"})
	f.must(t, "issue", "approve", strconv.Itoa(childRes.Issue.Number))
	f.must(t, "work", "--next", "--agent", "mock", "--headless")
	if st := f.state(t, childRes.Issue.Number); st.Lease.ClaimID == "" {
		t.Fatalf("expected child #%d to be pulled and worked past mislabeled Epic #%d", childRes.Issue.Number, epicRes.Issue.Number)
	}

	// Read-only doctor with github backend reports WARN for missing Epic type and non-maintainer without creating issues
	mockTr := &mockDoctorTracker{
		Tracker:     f.tracker,
		epicTypeErr: errors.New("issue type \"Epic\" is not enabled"),
		authErr:     errors.New("actor lacks maintainer permission"),
	}
	docCmd := newRootCmdWithOptions(&RootOptions{
		RepoRoot:        f.root,
		Backend:         "github",
		trackerOverride: mockTr,
	})
	var docBuf bytes.Buffer
	docCmd.SetOut(&docBuf)
	docCmd.SetErr(&docBuf)
	docCmd.SetArgs([]string{"--root", f.root, "doctor", "--backend", "github"})
	if err := docCmd.Execute(); err != nil {
		t.Fatalf("doctor should report WARN without failing: %v\n%s", err, docBuf.String())
	}
	docText := docBuf.String()
	if !strings.Contains(docText, "github-epic-type") || !strings.Contains(docText, "github-epic-auth") || !strings.Contains(docText, "WARN") {
		t.Fatalf("expected doctor output to include WARN for github-epic-type and github-epic-auth:\n%s", docText)
	}
	if mockTr.created != 0 {
		t.Fatalf("doctor must be read-only, created %d issues", mockTr.created)
	}
}
