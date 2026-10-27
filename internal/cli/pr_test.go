package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/pr"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
)

func prGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// submittedPRFixture submits a PR for a new issue; setup runs on the fresh
// fixture before any command.
func submittedPRFixture(t *testing.T, gate string, setup ...func(*cliFixture)) (*cliFixture, int, int) {
	t.Helper()
	f := newCLIFixture(t)
	for _, fn := range setup {
		fn(f)
	}
	f.create(t, "agent") // PR and issue IDs deliberately differ.
	n := f.create(t, gate)
	issue := strconv.Itoa(n)
	f.must(t, "issue", "approve", issue)
	f.must(t, "issue", "work", issue, "--agent", "mock", "--progress=false")
	if gate == "human" {
		f.setenv("SWARM_USER", "independent-maintainer")
		f.must(t, "issue", "review-plan", issue, "--approve", "--notes", "Checked initial plan")
		f.setenv("SWARM_USER", "alice")
		f.must(t, "issue", "work", issue, "--agent", "mock", "--progress=false")
	}
	f.must(t, "issue", "submit", issue)
	number := f.state(t, n).PRNumber
	if number == n {
		t.Fatal("fixture must distinguish PR and issue identifiers")
	}
	return f, n, number
}

func readPROutcome(t *testing.T, raw string) *pr.Result {
	t.Helper()
	var outcome struct {
		Result *pr.Result `json:"result"`
	}
	if err := json.Unmarshal([]byte(raw), &outcome); err != nil {
		t.Fatalf("invalid PR JSON: %v\n%s", err, raw)
	}
	if outcome.Result == nil {
		t.Fatalf("missing PR result: %s", raw)
	}
	return outcome.Result
}

func TestCLIPRWorkValidateSubmitPreservesIdentity(t *testing.T) {
	t.Parallel()
	f, issue, number := submittedPRFixture(t, "agent")
	p := strconv.Itoa(number)
	before := f.state(t, issue)
	original, err := f.tracker.GetPullRequest(context.Background(), number)
	if err != nil {
		t.Fatal(err)
	}
	status := readPROutcome(t, f.must(t, "pr", "status", p, "--json"))
	if status.IssueNumber != issue || status.PR.Number != number {
		t.Fatalf("PR/issue identity confused: %+v", status)
	}
	if len(status.RoleRuns) != 4 || !reflect.DeepEqual(status.RoleRuns, lease.RoleRuns(before)) {
		t.Fatalf("PR status role runs = %+v", status.RoleRuns)
	}
	if !strings.Contains(original.Body, "## ⏱️ Role Run Telemetry") || !strings.Contains(original.Body, lease.RoleRunTable(status.RoleRuns)) || !strings.Contains(original.Body, `"run_id": "`+before.RoleRuns[0].RunID+`"`) {
		t.Fatalf("PR evidence lacks role run telemetry: %s", original.Body)
	}
	f.must(t, "pr", "work", p, "--agent", "mock", "--dry-run", "--json")
	if after := f.state(t, issue); !reflect.DeepEqual(before, after) {
		t.Fatal("dry-run changed authoritative state")
	}
	issueTree := workspace.WorktreePath(f.root, issue)
	issueHead := prGit(t, issueTree, "rev-parse", "HEAD")
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'PR update\\n' > pr-update.txt")
	worked := readPROutcome(t, f.must(t, "pr", "work", p, "--agent", "mock", "--instructions", "Fix the implementation within the approved acceptance criteria", "--progress=false", "--json"))
	if worked.State.RevisionPlan == nil {
		t.Fatal("approved original missing from revision")
	}
	revised := lease.RoleRunTable(lease.RoleRuns(f.state(t, issue)))
	for _, args := range [][]string{{"issue", "status", strconv.Itoa(issue)}, {"pr", "status", p}} {
		output := f.must(t, args...)
		if !strings.Contains(output, "Original approved plan") || !strings.Contains(output, "Current revision plan") {
			t.Fatal(output)
		}
		// Issue and PR status show the same rows, including the revision's runs.
		if !strings.Contains(output, "Role runs:\n"+revised) || strings.Count(revised, "\n") <= 6 {
			t.Fatalf("%v role runs:\n%s", args, output)
		}
	}
	if worked.Worktree == issueTree || worked.HeadSHA == before.HeadSHA {
		t.Fatalf("no isolated PR checkpoint: %+v", worked)
	}
	if got := prGit(t, issueTree, "rev-parse", "HEAD"); got != issueHead {
		t.Fatal("work changed original issue branch")
	}
	if got := prGit(t, f.root, "rev-parse", before.Branch); got != before.HeadSHA {
		t.Fatal("work published candidate before submit")
	}
	for _, operation := range []string{"claim", "work", "stage", "validate", "submit"} {
		args := []string{"issue", operation, strconv.Itoa(issue)}
		if operation == "work" || operation == "claim" {
			args = append(args, "--agent", "mock")
		}
		if operation == "stage" {
			args = append(args, "--phase", "implement")
		}
		if _, err := f.run(args...); err == nil {
			t.Fatalf("issue %s bypassed PR revision", operation)
		}
	}
	validated := readPROutcome(t, f.must(t, "pr", "validate", p, "--json"))
	if validated.Validation == nil || !validated.Validation.Passed {
		t.Fatalf("validation failed: %+v", validated)
	}
	f.must(t, "pr", "submit", p, "--json")
	after := f.state(t, issue)
	updated, err := f.tracker.GetPullRequest(context.Background(), number)
	if err != nil {
		t.Fatal(err)
	}
	if after.RevisionPlan == nil || !strings.Contains(updated.Body, "Original approved plan") || !strings.Contains(updated.Body, "Current revision plan") {
		t.Fatal("submitted original/revision context missing")
	}
	status = readPROutcome(t, f.must(t, "pr", "status", p, "--json"))
	if status.State.RevisionPlan == nil || status.State.RevisionPlan.Original.Plan != worked.State.RevisionPlan.Original.Plan {
		t.Fatal("completed JSON lost original")
	}
	if after.PRNumber != number || after.HeadSHA != worked.HeadSHA || after.Lease.Status != "completed" {
		t.Fatalf("incorrect submission state: %+v", after)
	}
	if updated.Title != original.Title || !strings.HasPrefix(updated.Body, original.Body) || updated.Draft != original.Draft || updated.HTMLURL != original.HTMLURL {
		t.Fatal("PR submission replaced identity or review content")
	}
	f.must(t, "pr", "submit", p, "--json")
	if retry := f.state(t, issue); retry.HeadSHA != after.HeadSHA || retry.PRNumber != number {
		t.Fatal("submission retry changed identity")
	}
}

func TestCLIPRRebaseCarriesApprovedPlanAndRequiresFreshValidation(t *testing.T) {
	t.Parallel()
	for _, gate := range []string{"agent", "human"} {
		t.Run(gate, func(t *testing.T) {
			f, issue, number := submittedPRFixture(t, gate)
			p := strconv.Itoa(number)
			before := f.state(t, issue)
			if err := os.WriteFile(filepath.Join(f.root, "upstream.txt"), []byte("new upstream base\n"), 0644); err != nil {
				t.Fatal(err)
			}
			prGit(t, f.root, "add", "upstream.txt")
			prGit(t, f.root, "commit", "-m", "Advance approved base")
			base := prGit(t, f.root, "rev-parse", "HEAD")
			if _, err := f.run("pr", "rebase", p); err == nil {
				t.Fatal("rebase accepted an unapproved target base")
			}
			if after := f.state(t, issue); !reflect.DeepEqual(before, after) {
				t.Fatal("rejected rebase mutated state")
			}
			f.must(t, "issue", "approve", strconv.Itoa(issue))
			rebased := readPROutcome(t, f.must(t, "pr", "rebase", p, "--json"))
			if rebased.BaseSHA != base || rebased.HeadSHA == before.HeadSHA {
				t.Fatalf("rebase did not move onto new base: %+v", rebased)
			}
			prGit(t, rebased.Worktree, "merge-base", "--is-ancestor", base, "HEAD")
			if rebased.State.ApprovalTransition == nil {
				t.Fatalf("plan was not carried: reason=%s", rebased.State.CarryReason)
			}
			validated := readPROutcome(t, f.must(t, "pr", "validate", p, "--json"))
			if validated.Validation == nil || !validated.Validation.Passed || validated.Validation.BaseSHA != base {
				t.Fatalf("fresh validation missing: %+v", validated.Validation)
			}
			f.must(t, "pr", "submit", p)
			if state := f.state(t, issue); state.PRNumber != number || state.BaseSHA != base {
				t.Fatalf("rebase changed linked contract identity: %+v", state)
			}
		})
	}
}

func TestCLIPRHumanGateRequiresNewIndependentApproval(t *testing.T) {
	t.Parallel()
	f, issue, number := submittedPRFixture(t, "human")
	p := strconv.Itoa(number)
	result := readPROutcome(t, f.must(t, "pr", "work", p, "--agent", "mock", "--progress=false", "--json"))
	if result.Work == nil || !result.Work.PausedForHuman {
		t.Fatal("PR work reused pre-submission human approval")
	}
	if _, err := f.run("pr", "submit", p); err == nil {
		t.Fatal("submission bypassed human plan gate")
	}
	f.setenv("SWARM_USER", "independent-maintainer")
	f.must(t, "issue", "review-plan", strconv.Itoa(issue), "--approve", "--notes", "Checked PR revision plan")
	f.setenv("SWARM_USER", "alice")
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'revision\n' > human-approved-revision.txt")
	f.must(t, "pr", "work", p, "--agent", "mock", "--progress=false")
	f.must(t, "pr", "submit", p)
}

func TestCLIPRSelectorsAndUnlinkedPRCannotAcquire(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	issue := f.create(t, "agent")
	f.must(t, "issue", "approve", strconv.Itoa(issue))
	before := f.state(t, issue)
	orphan, err := f.tracker.CreateDraftPR(context.Background(), tracker.DraftPRRequest{IssueNumber: issue, Title: "Not authoritatively submitted", Head: "main", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"pr", "work", "0"}, {"pr", "rebase", "1", "--continue", "--abort"},
		{"pr", "work", "1", "--phase", "repair"}, {"pr", "repair", "1"},
		{"pr", "work", strconv.Itoa(orphan.Number), "--agent", "mock"},
		{"pr", "stage", strconv.Itoa(orphan.Number), "--phase", "plan"},
	} {
		if _, err := f.run(args...); err == nil {
			t.Fatalf("invalid or unapproved PR accepted: %v", args)
		}
	}
	if after := f.state(t, issue); !reflect.DeepEqual(before, after) {
		t.Fatal("invalid PR request changed approval or claim")
	}
}

func submittedInteractivePRFixture(t *testing.T, gates ...string) (*cliFixture, int, int) {
	t.Helper()
	f := newCLIFixture(t)
	cfgPath := filepath.Join(f.root, ".swarm", "config.yaml")
	cfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	interactive := strings.Replace(string(cfg), "plan_mode: auto", "plan_mode: interactive", 1)
	if interactive == string(cfg) {
		t.Fatal("fixture config does not contain default plan mode")
	}
	if err := os.WriteFile(cfgPath, []byte(interactive), 0600); err != nil {
		t.Fatal(err)
	}
	prGit(t, f.root, "add", ".swarm/config.yaml")
	prGit(t, f.root, "commit", "-m", "Approve interactive plan policy")

	f.create(t, "agent") // PR and issue numbers must remain distinct.
	gate := "human"
	if len(gates) > 0 {
		gate = gates[0]
	}
	issue := f.create(t, gate)
	n := strconv.Itoa(issue)
	f.must(t, "issue", "approve", n)
	f.must(t, "issue", "claim", n, "--agent", "mock")
	issueTree := workspace.WorktreePath(f.root, issue)
	planDir := filepath.Join(issueTree, ".swarm", "active")
	if err := os.WriteFile(filepath.Join(planDir, "PLAN.md"), []byte("# Initial implementation plan\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planDir, "TEST_PLAN.md"), []byte("# Initial test plan\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.must(t, "issue", "stage", n, "--phase", "plan")
	f.setenv("SWARM_USER", "independent-maintainer")
	f.must(t, "issue", "review-plan", n, "--approve", "--notes", "Checked initial interactive plan")
	f.setenv("SWARM_USER", "alice")
	if err := os.WriteFile(filepath.Join(issueTree, "initial.txt"), []byte("initial implementation\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.must(t, "issue", "stage", n, "--phase", "implement")
	f.must(t, "issue", "submit", n)
	number := f.state(t, issue).PRNumber
	if number == issue {
		t.Fatal("fixture must distinguish PR and issue identifiers")
	}
	return f, issue, number
}

func TestCLIPRSubmitKeepsApprovedPlanAfterFailedImplementation(t *testing.T) {
	t.Parallel()
	f, issue, number := submittedPRFixture(t, "agent")
	p := strconv.Itoa(number)
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "exit 1")
	if _, err := f.run("pr", "work", p, "--agent", "mock", "--progress=false"); err == nil {
		t.Fatal("failing developer reported success")
	}
	before := f.state(t, issue)
	out, err := f.run("pr", "submit", p, "--progress=false")
	if err == nil || !strings.Contains(err.Error(), "swarm pr work "+p+" --phase implement") || strings.Contains(out, "submitted") {
		t.Fatalf("submit without implementation = %q, %v", out, err)
	}
	if after := f.state(t, issue); !reflect.DeepEqual(before, after) {
		t.Fatal("refused submit changed authoritative state")
	}
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'PR update\\n' > pr-update.txt")
	worked := readPROutcome(t, f.must(t, "pr", "work", p, "--agent", "mock", "--phase", "implement", "--progress=false", "--json"))
	if worked.HeadSHA == before.HeadSHA {
		t.Fatalf("resumed implementation produced no checkpoint: %+v", worked)
	}
	f.must(t, "pr", "submit", p)
	if state := f.state(t, issue); state.PRRevision != nil || state.HeadSHA != worked.HeadSHA {
		t.Fatalf("submit after implementation: %+v", state)
	}
}

func TestCLIPRInteractivePlanStageAndResume(t *testing.T) {
	t.Parallel()
	f, issue, number := submittedInteractivePRFixture(t)
	p := strconv.Itoa(number)
	if _, err := f.run("pr", "stage", p, "--phase", "plan"); err == nil {
		t.Fatal("PR staging accepted a revision before PR work created a pending checkpoint")
	}
	initial := f.state(t, issue)
	issueTree := workspace.WorktreePath(f.root, issue)
	issueHead := prGit(t, issueTree, "rev-parse", "HEAD")
	paused := readPROutcome(t, f.must(t, "pr", "work", p, "--agent", "mock", "--progress=false", "--json"))
	if paused.Work == nil || !paused.Work.PausedForHuman || paused.Work.PhaseCompleted != "awaiting-plan" || paused.Worktree == issueTree {
		t.Fatalf("interactive PR work did not pause in isolated worktree: %+v", paused)
	}
	if state := f.state(t, issue); state.PRRevision == nil {
		t.Fatal("paused PR has no pending revision")
	}
	if _, err := f.run("--fork", "pr", "stage", p, "--phase", "plan"); err == nil {
		t.Fatal("PR stage accepted contributor-only fork mode")
	}
	if _, err := f.run("issue", "stage", strconv.Itoa(issue), "--phase", "plan"); err == nil {
		t.Fatal("issue stage accepted a pending PR revision")
	}
	if _, err := f.run("pr", "stage", p, "--phase", "other"); err == nil {
		t.Fatal("PR stage accepted an invalid phase")
	}
	planDir := filepath.Join(paused.Worktree, ".swarm", "active")
	if err := os.WriteFile(filepath.Join(planDir, "PLAN.md"), []byte("# PR interactive implementation plan\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planDir, "TEST_PLAN.md"), []byte("# PR interactive test plan\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var stage struct {
		PRNumber    int    `json:"pr_number"`
		IssueNumber int    `json:"issue_number"`
		Phase       string `json:"phase"`
	}
	if err := json.Unmarshal([]byte(f.must(t, "pr", "stage", p, "--phase", "plan", "--harness", "codex", "--model", "interactive-model", "--json")), &stage); err != nil {
		t.Fatal(err)
	}
	if stage.PRNumber != number || stage.IssueNumber != issue || stage.Phase != "plan" {
		t.Fatalf("PR plan stage confused identities: %+v", stage)
	}
	if _, err := f.run("pr", "submit", p); err == nil {
		t.Fatal("PR submit bypassed independent plan approval")
	}
	f.setenv("SWARM_USER", "independent-maintainer")
	f.must(t, "issue", "review-plan", strconv.Itoa(issue), "--approve", "--notes", "Reviewed PR interactive plan")
	f.setenv("SWARM_USER", "alice")
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'PR implementation\\n' > pr-interactive.txt")
	worked := readPROutcome(t, f.must(t, "pr", "work", p, "--agent", "mock", "--progress=false", "--json"))
	if worked.HeadSHA == initial.HeadSHA || worked.State.PRRevision == nil {
		t.Fatalf("PR implementation did not checkpoint locally: %+v", worked)
	}
	if prGit(t, issueTree, "rev-parse", "HEAD") != issueHead || prGit(t, f.root, "rev-parse", initial.Branch) != initial.HeadSHA {
		t.Fatal("PR revision mutated original issue checkout or published branch")
	}
	f.must(t, "pr", "submit", p)
	if state := f.state(t, issue); state.HeadSHA != worked.HeadSHA || state.PRNumber != number {
		t.Fatalf("PR submit failed to publish recorded revision: %+v", state)
	}
}

func TestCLIPRManualImplementationStageRemainsUnpublished(t *testing.T) {
	t.Parallel()
	f, issue, number := submittedInteractivePRFixture(t)
	p := strconv.Itoa(number)
	before := f.state(t, issue)
	paused := readPROutcome(t, f.must(t, "pr", "work", p, "--agent", "mock", "--progress=false", "--json"))
	planDir := filepath.Join(paused.Worktree, ".swarm", "active")
	for name, body := range map[string]string{"PLAN.md": "# Manual revision plan\n", "TEST_PLAN.md": "# Manual revision tests\n"} {
		if err := os.WriteFile(filepath.Join(planDir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	f.must(t, "pr", "stage", p, "--phase", "plan")
	f.setenv("SWARM_USER", "independent-maintainer")
	f.must(t, "issue", "review-plan", strconv.Itoa(issue), "--approve", "--notes", "Checked manual PR plan")
	f.setenv("SWARM_USER", "alice")
	if err := os.WriteFile(filepath.Join(paused.Worktree, "manual-pr.txt"), []byte("unpublished PR revision\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var stage struct {
		PRNumber    int    `json:"pr_number"`
		IssueNumber int    `json:"issue_number"`
		HeadSHA     string `json:"head_sha"`
	}
	if err := json.Unmarshal([]byte(f.must(t, "pr", "stage", p, "--phase", "implement", "--json")), &stage); err != nil {
		t.Fatal(err)
	}
	if stage.PRNumber != number || stage.IssueNumber != issue || stage.HeadSHA == before.HeadSHA {
		t.Fatalf("manual checkpoint lost PR identity: %+v", stage)
	}
	staged := f.state(t, issue)
	if staged.HeadSHA != before.HeadSHA || staged.PRRevision == nil || staged.PRRevision.LocalHeadSHA != stage.HeadSHA || prGit(t, f.root, "rev-parse", before.Branch) != before.HeadSHA {
		t.Fatal("manual checkpoint published original issue branch before PR submit")
	}
	f.must(t, "pr", "submit", p)
	if state := f.state(t, issue); state.HeadSHA != stage.HeadSHA || state.PRRevision != nil {
		t.Fatalf("PR submit did not publish manual checkpoint: %+v", state)
	}
}

func TestCLIIssueInteractivePlanPauseKeepsClaimActive(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	cfgPath := filepath.Join(f.root, ".swarm", "config.yaml")
	cfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(strings.Replace(string(cfg), "plan_mode: auto", "plan_mode: interactive", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	prGit(t, f.root, "add", ".swarm/config.yaml")
	prGit(t, f.root, "commit", "-m", "Approve interactive plan policy")
	issue := f.create(t, "human")
	n := strconv.Itoa(issue)
	f.must(t, "issue", "approve", n)
	out := f.must(t, "issue", "work", n, "--agent", "mock", "--progress=false")
	if !strings.Contains(out, "Interactive plan required") {
		t.Fatalf("issue work did not pause for an interactive plan:\n%s", out)
	}
	state := f.state(t, issue)
	if state.Lease.Status != "active" || state.Lease.Phase != "plan" || state.Lease.Owner != "alice" {
		t.Fatalf("interactive pause must keep the planner's claim active: %+v", state.Lease)
	}
	planDir := filepath.Join(workspace.WorktreePath(f.root, issue), ".swarm", "active")
	for name, body := range map[string]string{"PLAN.md": "# Plan\n", "TEST_PLAN.md": "# Tests\n"} {
		if err := os.WriteFile(filepath.Join(planDir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	f.must(t, "issue", "stage", n, "--phase", "plan")
}

func TestCLIPRRebaseConflictAbortAndContinue(t *testing.T) {
	t.Parallel()
	f, issue, number := submittedPRFixture(t, "agent")
	p := strconv.Itoa(number)
	before := f.state(t, issue)
	changed := strings.Fields(prGit(t, f.root, "diff", "--name-only", before.BaseSHA, before.HeadSHA))
	if len(changed) == 0 {
		t.Fatal("fixture PR changed no files")
	}
	if _, err := f.run("pr", "rebase", p, "--continue"); err == nil || !strings.Contains(err.Error(), "no managed PR rebase in progress") {
		t.Fatalf("continue without a rebase = %v", err)
	}
	conflicting := changed[0]
	if err := os.WriteFile(filepath.Join(f.root, conflicting), []byte("conflicting upstream content\n"), 0644); err != nil {
		t.Fatal(err)
	}
	prGit(t, f.root, "add", conflicting)
	prGit(t, f.root, "commit", "-m", "Conflicting upstream change")
	base := prGit(t, f.root, "rev-parse", "HEAD")
	f.must(t, "issue", "approve", strconv.Itoa(issue))

	if _, err := f.run("pr", "rebase", p); err == nil {
		t.Fatal("conflicting rebase reported success")
	}
	worktree := workspace.PRWorktreePath(f.root, number)
	if _, err := f.run("pr", "rebase", p); err == nil || !strings.Contains(err.Error(), "--continue or --abort") {
		t.Fatalf("missing recovery guidance: %v", err)
	}
	f.must(t, "pr", "rebase", p, "--abort")
	if r := f.state(t, issue).LastRebase; r == nil || r.Status != "aborted" || r.Verified {
		t.Fatalf("abort recorded success: %+v", r)
	}
	if head := prGit(t, worktree, "rev-parse", "HEAD"); head != before.HeadSHA {
		t.Fatalf("abort left PR head at %s, want %s", head, before.HeadSHA)
	}

	if _, err := f.run("pr", "rebase", p); err == nil {
		t.Fatal("conflicting rebase reported success")
	}
	if err := os.WriteFile(filepath.Join(worktree, conflicting), []byte("resolved content\n"), 0644); err != nil {
		t.Fatal(err)
	}
	prGit(t, worktree, "add", conflicting)
	rebased := readPROutcome(t, f.must(t, "pr", "rebase", p, "--continue", "--json"))
	if rebased.Status != "pending" || rebased.HeadSHA == before.HeadSHA {
		t.Fatalf("continued rebase = %+v", rebased)
	}
	prGit(t, worktree, "merge-base", "--is-ancestor", base, "HEAD")
	if state := f.state(t, issue); state.PRRevision == nil || state.PRRevision.LocalHeadSHA != rebased.HeadSHA {
		t.Fatalf("rebased head not recorded: %+v", state.PRRevision)
	}
	evidence := rebased.State.LastRebase
	if evidence == nil || evidence.Status != "completed" || evidence.PatchIdentity != "changed" || !reflect.DeepEqual(evidence.ConflictedPaths, []string{conflicting}) {
		t.Fatalf("missing conflict context: %+v", evidence)
	}
	f.must(t, "pr", "validate", p)
	f.must(t, "pr", "submit", p)
	updated, err := f.tracker.GetPullRequest(context.Background(), number)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(updated.Body, "Patch identity: changed") || !strings.Contains(updated.Body, conflicting) {
		t.Fatal("PR lacks rebase context")
	}
	f.must(t, "pr", "submit", p)
	retried, err := f.tracker.GetPullRequest(context.Background(), number)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Body != updated.Body {
		t.Fatal("submit retry duplicated context")
	}
}

func TestCLIApprovalClaimRecovery(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	number := f.create(t, "agent")
	n := strconv.Itoa(number)
	f.must(t, "issue", "approve", n)
	f.must(t, "issue", "claim", n, "--agent", "mock")
	if _, err := f.run("issue", "approve", n); err == nil || !strings.Contains(err.Error(), "alice") || !strings.Contains(err.Error(), "until") || !strings.Contains(err.Error(), "swarm issue release "+n) {
		t.Fatalf("own claim recovery: %v", err)
	}
	f.setenv("SWARM_USER", "other-maintainer")
	if _, err := f.run("issue", "approve", n); err == nil || !strings.Contains(err.Error(), "alice") || strings.Contains(err.Error(), "swarm issue release") {
		t.Fatalf("other claim recovery: %v", err)
	}
}

func TestCLIPRRebaseExplainsReplan(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{"WorkUnit changed", "policy files changed"} {
		t.Run(reason, func(t *testing.T) {
			f, issue, number := submittedPRFixture(t, "agent")
			n := strconv.Itoa(issue)
			p := strconv.Itoa(number)
			name := "upstream.txt"
			if reason == "policy files changed" {
				name = ".swarm/rules/rebase-test.md"
				if err := os.MkdirAll(filepath.Dir(filepath.Join(f.root, name)), 0755); err != nil {
					t.Fatal(err)
				}
			} else {
				current, err := f.tracker.GetIssue(context.Background(), issue)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.tracker.UpdateIssueBody(context.Background(), issue, strings.ReplaceAll(current.Body, "Exercise the lifecycle", "Exercise the revised lifecycle")); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(f.root, name), []byte("new upstream content\n"), 0644); err != nil {
				t.Fatal(err)
			}
			prGit(t, f.root, "add", name)
			prGit(t, f.root, "commit", "-m", "Advance base")
			f.must(t, "issue", "approve", n)
			output := f.must(t, "pr", "rebase", p)
			if !strings.Contains(output, reason) || !strings.Contains(output, "swarm pr work "+p) {
				t.Fatalf("missing replan reason: %s", output)
			}
			status := f.must(t, "pr", "status", p)
			if !strings.Contains(status, reason) || (reason == "policy files changed" && !strings.Contains(status, name)) {
				t.Fatalf("missing status detail: %s", status)
			}
			if _, err := f.run("pr", "validate", p); err == nil {
				t.Fatal("validation accepted invalidated plan")
			}
			if _, err := f.run("pr", "submit", p); err == nil {
				t.Fatal("submit accepted invalidated plan")
			}
		})
	}
}

func TestCLIPRWorkDryRunReportsLocalSkip(t *testing.T) {
	t.Parallel()
	f, issue, number := submittedPRFixture(t, "agent")
	before := f.state(t, issue)
	result := readPROutcome(t, f.must(t, "pr", "work", strconv.Itoa(number), "--dry-run"))
	if result.WorkPreflight == nil || result.WorkPreflight.Status != "skipped" || result.WorkPreflight.BaseResult != "skipped" || result.Preflight == nil {
		t.Fatalf("missing dry-run checks: %+v", result)
	}
	if !reflect.DeepEqual(before, f.state(t, issue)) {
		t.Fatal("dry-run changed state")
	}
	if _, err := os.Stat(workspace.PRWorktreePath(f.root, number)); !os.IsNotExist(err) {
		t.Fatalf("dry-run created worktree: %v", err)
	}
}

type workCLITracker struct {
	tracker.Tracker
	pr tracker.PullRequest
}

func (w workCLITracker) BackendName() string { return "github" }
func (w workCLITracker) GetPullRequest(context.Context, int) (*tracker.PullRequest, error) {
	p := w.pr
	return &p, nil
}

func TestCLIPRWorkGitHubDryRun(t *testing.T) {
	t.Parallel()
	f, issue, number := submittedPRFixture(t, "agent")
	ctx := context.Background()
	remote := filepath.Join(t.TempDir(), "remote.git")
	prGit(t, f.root, "init", "--bare", "--initial-branch=main", remote)
	prGit(t, f.root, "remote", "add", "origin", remote)
	prGit(t, f.root, "push", "origin", "main")
	raw, _, err := f.manager.Store().Load(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	store := trustedLedgerStore(t, f.root, "")
	if _, err = store.CompareAndSwap(ctx, issue, "", raw); err != nil {
		t.Fatal(err)
	}
	original, err := f.tracker.GetPullRequest(ctx, number)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		mergeable bool
		moved     bool
		blocked   bool
	}{
		{name: "clean", mergeable: true}, {name: "conflict", blocked: true}, {name: "moved", mergeable: true, moved: true, blocked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := *original
			p.Mergeable = &tc.mergeable
			p.MergeableState = "clean"
			if !tc.mergeable {
				p.MergeableState = "dirty"
			}
			if tc.moved {
				p.BaseSHA = "advanced"
			}
			opts := &RootOptions{RepoRoot: f.root, Backend: "github", trackerOverride: workCLITracker{Tracker: f.tracker, pr: p}}
			cmd := f.command(opts)
			var out, diagnostics bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&diagnostics)
			cmd.SetArgs([]string{"pr", "work", strconv.Itoa(number), "--dry-run"})
			if err := cmd.ExecuteContext(ctx); err != nil {
				t.Fatalf("%v %s", err, diagnostics.String())
			}
			result := readPROutcome(t, out.String())
			w := result.WorkPreflight
			if w == nil || (w.Status == "blocked") != tc.blocked || w.TargetBase != p.BaseSHA || w.ApprovedBase != original.BaseSHA || w.Mergeable == nil || *w.Mergeable != tc.mergeable {
				t.Fatalf("%+v", w)
			}
			if tc.blocked && !strings.Contains(strings.Join(w.Commands, " "), fmt.Sprintf("rebase %d", number)) {
				t.Fatal("missing recovery")
			}
			after, _, err := store.Load(ctx, issue)
			if err != nil || !bytes.Equal(raw, after) {
				t.Fatal("dry-run mutated Git state", err)
			}
			if _, err := os.Stat(workspace.PRWorktreePath(f.root, number)); !os.IsNotExist(err) {
				t.Fatal("worktree created", err)
			}
		})
	}
}

// Reproduce the recovery sequence after a policy change prevents plan carry-forward.
func TestCLIPRRebaseStagePlanSurvivesReview(t *testing.T) {
	t.Parallel()
	f, issue, number := submittedInteractivePRFixture(t, "agent")
	p := strconv.Itoa(number)
	name := ".swarm/rules/rebase-stage.md"
	if err := os.WriteFile(filepath.Join(f.root, name), []byte("new policy\n"), 0600); err != nil {
		t.Fatal(err)
	}
	prGit(t, f.root, "add", name)
	prGit(t, f.root, "commit", "-m", "Change approved policy")
	f.must(t, "issue", "approve", strconv.Itoa(issue))
	f.must(t, "pr", "rebase", p)
	before := f.state(t, issue)
	if before.PRRevision == nil || before.PRRevision.Intent != "rebase" || len(before.Artifacts) != 0 {
		t.Fatalf("expected ineligible rebase: %+v", before)
	}
	if err := os.MkdirAll(filepath.Join(workspace.PRWorktreePath(f.root, number), ".swarm", "active"), 0755); err != nil {
		t.Fatal(err)
	}
	bodies := map[string]string{"PLAN.md": "# Replacement plan\n", "TEST_PLAN.md": "# Replacement tests\n"}
	for name, body := range bodies {
		if err := os.WriteFile(filepath.Join(workspace.PRWorktreePath(f.root, number), ".swarm", "active", name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	f.must(t, "pr", "stage", p, "--phase", "plan")
	staged := f.state(t, issue)
	identity := *before.PRRevision
	identity.Intent = "work"
	if staged.PRRevision == nil || *staged.PRRevision != identity {
		t.Fatalf("staging did not transition revision: %+v", staged.PRRevision)
	}
	f.must(t, "pr", "work", p, "--phase", "review", "--agent", "mock", "--progress=false")
	after := f.state(t, issue)
	for _, art := range staged.Artifacts {
		found := false
		for _, current := range after.Artifacts {
			if current.Type == art.Type && current.Body == art.Body {
				found = true
			}
		}
		if !found {
			t.Fatalf("staged %s lost during review", art.Type)
		}
	}
	bindings := lease.ReviewBindings(after)
	reviewed := false
	for _, art := range after.Artifacts {
		if art.Type == contract.ArtifactReview {
			reviewed = true
			for key, value := range bindings {
				if value == "" || art.Metadata[key] != value {
					t.Fatalf("review binding %s mismatch", key)
				}
			}
		}
	}
	if !reviewed {
		t.Fatal("no fresh review")
	}
}

func TestCLIPRRebaseWithoutStageRequiresNewPlan(t *testing.T) {
	t.Parallel()
	f, issue, number := submittedInteractivePRFixture(t)
	p := strconv.Itoa(number)
	f.must(t, "pr", "rebase", p)
	result := readPROutcome(t, f.must(t, "pr", "work", p, "--agent", "mock", "--progress=false", "--json"))
	if result.Work == nil || result.Work.PhaseCompleted != "awaiting-plan" {
		t.Fatalf("expected new plan cycle: %+v", result.Work)
	}
	st := f.state(t, issue)
	if st.PRRevision.Intent != "work" || len(st.Artifacts) != 0 || st.ApprovalTransition != nil {
		t.Fatalf("unstaged resume retained evidence: %+v", st)
	}
}

func TestCLIPRWorkRestorationEvidence(t *testing.T) {
	t.Parallel()
	f, issue, number := submittedPRFixture(t, "agent", func(f *cliFixture) {
		if err := os.MkdirAll(filepath.Join(f.root, "lib"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.root, "lib", "keep.txt"), []byte("keep\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		prGit(t, f.root, "add", "lib/keep.txt")
		prGit(t, f.root, "commit", "-m", "base file")
	})
	p := strconv.Itoa(number)
	// A blocked PR round checkpoints the deletion, as in #207.
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "rm lib/keep.txt && printf 'Status: blocked - removed lib/keep.txt\\n' > .swarm/active/IMPLEMENTATION.md")
	if _, err := f.run("pr", "work", p, "--agent", "mock", "--progress=false"); err == nil || !strings.Contains(err.Error(), "developer blocked") {
		t.Fatalf("blocked round: %v", err)
	}
	captured := filepath.Join(t.TempDir(), "REPAIR.md")
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "cp .swarm/active/REPAIR.md '"+captured+"' && printf 'keep\\n' > lib/keep.txt")
	worked := readPROutcome(t, f.must(t, "pr", "work", p, "--agent", "mock", "--phase", "implement", "--progress=false", "--json"))
	brief, err := os.ReadFile(captured)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(brief), "# Approved-base restoration evidence") || !strings.Contains(string(brief), "## File \"lib/keep.txt\"") || !strings.Contains(string(brief), "```text\nkeep\n```\n") {
		t.Fatalf("PR round lacks restoration evidence:\n%s", brief)
	}
	if got := prGit(t, worked.Worktree, "show", worked.HeadSHA+":lib/keep.txt"); got != "keep" || f.state(t, issue).PRRevision == nil {
		t.Fatalf("restored file at PR head = %q", got)
	}
}
