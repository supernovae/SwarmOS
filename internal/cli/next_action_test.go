package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/pr"
	"github.com/supernovae/swarmos/internal/workflow"
	"github.com/supernovae/swarmos/internal/workspace"
)

// TestNextActionMatrix classifies each reason for each command that reports
// one, using the errors those commands' gates return. Pairs a command cannot
// reach naturally are still classified consistently:
//   - issue work and issue submit have no PR, so conflict is unreachable.
//   - issue submit and pr submit run no agent review, so changes_requested is
//     unreachable.
func TestNextActionMatrix(t *testing.T) {
	opts := &RootOptions{Backend: "local", RepoRoot: "/repo"}
	base := []string{"swarm", "--backend", "local", "--root", "/repo"}
	argv := func(args ...string) []string { return append(append([]string(nil), base...), args...) }
	rejected := fmt.Errorf("agent review: %w: reviewer %q returned CHANGES_REQUESTED", workflow.ErrPlanReviewRejected, "mock")
	task := &lease.TaskApprovalError{IssueNumber: 7}
	plan := &lease.HumanPlanApprovalError{IssueNumber: 7}
	fresh := &pr.ConflictError{PRNumber: 9, Err: errors.New("PR #9 is conflicting")}
	paused := &pr.ConflictError{PRNumber: 9, Paused: true, Err: errors.New("PR #9 has a paused rebase")}
	manual := &pr.ConflictError{PRNumber: 9, Paused: true, Manual: true, Err: errors.New("resolver stopped")}
	approve := argv("issue", "approve", "7")
	reviewPlan := argv("issue", "review-plan", "7", "--approve")
	type want struct {
		reason string
		argv   []string
		human  bool
		code   int
	}
	for _, tc := range []struct {
		command  string
		resource string
		number   int
		err      error
		want     want
	}{
		{"issue work", "issue", 7, rejected, want{ReasonChangesRequested, argv("issue", "work", "7", "--phase", "plan"), false, ExitChangesRequested}},
		{"issue work", "issue", 7, task, want{ReasonNeedsApproval, approve, true, ExitNeedsApproval}},
		{"issue work", "issue", 7, &lease.ClaimBaseApprovalError{IssueNumber: 7, Reason: "WorkUnit digest changed"}, want{ReasonNeedsApproval, approve, true, ExitNeedsApproval}},
		{"issue work", "issue", 7, fmt.Errorf("%w: %w", workflow.ErrPlanApprovalNeeded, plan), want{ReasonNeedsApproval, reviewPlan, true, ExitNeedsApproval}},
		{"issue work (unreachable)", "issue", 7, fresh, want{ReasonConflict, argv("pr", "rebase", "9", "--resolve"), false, ExitConflict}},
		{"issue submit (unreachable)", "issue", 7, rejected, want{ReasonChangesRequested, argv("issue", "work", "7", "--phase", "plan"), false, ExitChangesRequested}},
		{"issue submit", "issue", 7, fmt.Errorf("submission gate: %w", task), want{ReasonNeedsApproval, approve, true, ExitNeedsApproval}},
		{"issue submit", "issue", 7, fmt.Errorf("submission gate: %w", plan), want{ReasonNeedsApproval, reviewPlan, true, ExitNeedsApproval}},
		{"issue submit (unreachable)", "issue", 7, paused, want{ReasonConflict, argv("pr", "rebase", "9", "--continue", "--resolve"), false, ExitConflict}},
		{"pr work", "pr", 9, rejected, want{ReasonChangesRequested, argv("pr", "work", "9", "--phase", "plan"), false, ExitChangesRequested}},
		{"pr work", "pr", 9, task, want{ReasonNeedsApproval, approve, true, ExitNeedsApproval}},
		{"pr work", "pr", 9, fresh, want{ReasonConflict, argv("pr", "rebase", "9", "--resolve"), false, ExitConflict}},
		{"pr work", "pr", 9, paused, want{ReasonConflict, argv("pr", "rebase", "9", "--continue", "--resolve"), false, ExitConflict}},
		{"pr rebase", "pr", 9, fmt.Errorf("rebase plan review: %w", rejected), want{ReasonChangesRequested, argv("pr", "work", "9", "--phase", "plan"), false, ExitChangesRequested}},
		{"pr rebase", "pr", 9, task, want{ReasonNeedsApproval, approve, true, ExitNeedsApproval}},
		{"pr rebase", "pr", 9, plan, want{ReasonNeedsApproval, reviewPlan, true, ExitNeedsApproval}},
		{"pr rebase", "pr", 9, paused, want{ReasonConflict, argv("pr", "rebase", "9", "--continue", "--resolve"), false, ExitConflict}},
		{"pr rebase", "pr", 9, manual, want{ReasonConflict, argv("pr", "rebase", "9", "--continue"), true, ExitConflict}},
		{"pr submit (unreachable)", "pr", 9, rejected, want{ReasonChangesRequested, argv("pr", "work", "9", "--phase", "plan"), false, ExitChangesRequested}},
		{"pr submit", "pr", 9, fmt.Errorf("PR plan approval required: %w", task), want{ReasonNeedsApproval, approve, true, ExitNeedsApproval}},
		{"pr submit", "pr", 9, paused, want{ReasonConflict, argv("pr", "rebase", "9", "--continue", "--resolve"), false, ExitConflict}},
		// Precedence: rejection over approval, task approval over plan approval.
		{"pr work", "pr", 9, errors.Join(rejected, task), want{ReasonChangesRequested, argv("pr", "work", "9", "--phase", "plan"), false, ExitChangesRequested}},
		{"pr work", "pr", 9, &lease.HumanPlanApprovalError{IssueNumber: 7, Err: task}, want{ReasonNeedsApproval, approve, true, ExitNeedsApproval}},
	} {
		t.Run(tc.command+"/"+tc.want.reason, func(t *testing.T) {
			wrapped := stopContext{opts: opts, resource: tc.resource, number: tc.number}.stop(tc.err)
			action := nextActionOf(wrapped)
			if action == nil || action.Reason != tc.want.reason || !reflect.DeepEqual(action.Argv, tc.want.argv) || action.HumanRequired != tc.want.human {
				t.Fatalf("action = %+v, want %+v", action, tc.want)
			}
			if code := ExitCode(fmt.Errorf("outer: %w", wrapped)); code != tc.want.code {
				t.Fatalf("exit = %d, want %d", code, tc.want.code)
			}
			if !errors.Is(wrapped, tc.err) || wrapped.Error() != tc.err.Error() {
				t.Fatalf("stop changed the error: %v", wrapped)
			}
		})
	}
}

func TestNextActionLeavesOtherErrorsUnclassified(t *testing.T) {
	c := stopContext{opts: &RootOptions{}, resource: "pr", number: 9}
	for name, err := range map[string]error{
		"nil":               nil,
		"generic":           errors.New("boom"),
		"claim fence":       fmt.Errorf("wrapped: %w", lease.ErrClaimFence),
		"ambiguous":         lease.ErrApprovalRequired,
		"reviewer policy":   fmt.Errorf("%w: reviewer agent %q is not allowed", lease.ErrApprovalRequired, "x"),
		"cas conflict":      coordination.ErrConflict,
		"cancelled":         context.Canceled,
		"plan not approved": fmt.Errorf("%w: agent gate", workflow.ErrPlanApprovalNeeded),
	} {
		t.Run(name, func(t *testing.T) {
			got := c.stop(err)
			if got != err || nextActionOf(got) != nil {
				t.Fatalf("classified %v", err)
			}
			want := ExitFailure
			if err == nil {
				want = ExitOK
			}
			if code := ExitCode(got); code != want {
				t.Fatalf("exit = %d, want %d", code, want)
			}
		})
	}
}

func TestNextActionArgvKeepsRepositorySelection(t *testing.T) {
	root := "/work/it's a repo"
	fork := &RootOptions{Backend: "github", Repo: "acme/widgets", RepoRoot: root, Fork: true, ForkRemote: "my fork"}
	upstream := &RootOptions{Backend: "github", RepoRoot: root, ForkRemote: workspace.DefaultForkRemote}
	custom := &RootOptions{Backend: "github", ForkRemote: "writable"}
	for _, tc := range []struct {
		name string
		got  *NextAction
		want []string
	}{
		{"fork worker keeps fork flags", stopContext{opts: fork, resource: "pr", number: 3}.classify(&pr.ConflictError{PRNumber: 3, Err: errors.New("x")}),
			[]string{"swarm", "--backend", "github", "--repo", "acme/widgets", "--root", root, "--fork", "--fork-remote", "my fork", "pr", "rebase", "3", "--resolve"}},
		{"fork approval targets upstream maintainer", stopContext{opts: fork, resource: "issue", number: 2}.classify(&lease.TaskApprovalError{IssueNumber: 2}),
			[]string{"swarm", "--backend", "github", "--repo", "acme/widgets", "issue", "approve", "2"}},
		{"fork issue revise", stopContext{opts: fork, resource: "issue", number: 2}.classify(workflow.ErrPlanReviewRejected),
			[]string{"swarm", "--backend", "github", "--repo", "acme/widgets", "--root", root, "--fork", "--fork-remote", "my fork", "issue", "work", "2", "--phase", "plan"}},
		{"default fork remote omitted", stopContext{opts: upstream, resource: "pr", number: 3}.classify(workflow.ErrPlanReviewRejected),
			[]string{"swarm", "--backend", "github", "--root", root, "pr", "work", "3", "--phase", "plan"}},
		{"upstream-authorized remote kept for pr", stopContext{opts: custom, resource: "pr", number: 3}.classify(workflow.ErrPlanReviewRejected),
			[]string{"swarm", "--backend", "github", "--fork-remote", "writable", "pr", "work", "3", "--phase", "plan"}},
		{"remote ignored for issue", stopContext{opts: custom, resource: "issue", number: 2}.classify(workflow.ErrPlanReviewRejected),
			[]string{"swarm", "--backend", "github", "issue", "work", "2", "--phase", "plan"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got == nil || !reflect.DeepEqual(tc.got.Argv, tc.want) {
				t.Fatalf("action = %+v, want argv %q", tc.got, tc.want)
			}
		})
	}
	if a := (stopContext{opts: fork}).classify(&lease.TaskApprovalError{}); a != nil {
		t.Fatalf("approval without an issue number guessed %+v", a)
	}
}

// runJSON runs one command and decodes its stdout as exactly one JSON document.
func runJSON(t *testing.T, opts *RootOptions, args ...string) (map[string]any, error) {
	t.Helper()
	cmd := newRootCmdWithOptions(opts)
	var out, diagnostics bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&diagnostics)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	dec := json.NewDecoder(&out)
	var doc map[string]any
	if decErr := dec.Decode(&doc); decErr != nil {
		t.Fatalf("%v: invalid JSON: %v\nstdout: %s\nstderr: %s", args, decErr, out.String(), diagnostics.String())
	}
	if dec.More() {
		t.Fatalf("%v: more than one JSON document: %s", args, out.String())
	}
	return doc, err
}

func (f *cliFixture) runJSON(t *testing.T, args ...string) (map[string]any, error) {
	t.Helper()
	return runJSON(t, &RootOptions{}, append([]string{"--root", f.root, "--backend", "local"}, args...)...)
}

func assertNextAction(t *testing.T, doc map[string]any, err error, reason string, human bool, code int, argv ...string) {
	t.Helper()
	raw, ok := doc["next_action"].(map[string]any)
	if !ok {
		t.Fatalf("missing next_action (err %v): %v", err, doc)
	}
	var got []string
	for _, v := range raw["argv"].([]any) {
		got = append(got, v.(string))
	}
	if raw["reason"] != reason || raw["human_required"] != human || !reflect.DeepEqual(got, argv) {
		t.Fatalf("next_action = %v, want %s %v %q", raw, reason, human, argv)
	}
	if doc["status"] != "failed" || doc["error"] == "" {
		t.Fatalf("stop not reported as failed: %v", doc)
	}
	if c := ExitCode(err); c != code {
		t.Fatalf("exit = %d, want %d (%v)", c, code, err)
	}
}

func assertNoNextAction(t *testing.T, doc map[string]any, err error) {
	t.Helper()
	if err != nil || ExitCode(err) != ExitOK {
		t.Fatalf("unexpected failure: %v %v", err, doc)
	}
	if _, ok := doc["next_action"]; ok {
		t.Fatalf("success carries next_action: %v", doc)
	}
}

// reviseTask changes the WorkUnit under an active claim, staling its
// approval, and returns a function restoring the approved body.
func reviseTask(t *testing.T, f *cliFixture, issue int) func() {
	t.Helper()
	ctx := context.Background()
	iss, err := f.tracker.GetIssue(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(iss.Body, "Exercise the lifecycle", "Exercise a changed lifecycle", 1)
	if body == iss.Body {
		t.Fatal("fixture objective not found")
	}
	if _, err := f.tracker.UpdateIssueBody(ctx, issue, body); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if _, err := f.tracker.UpdateIssueBody(ctx, issue, iss.Body); err != nil {
			t.Fatal(err)
		}
	}
}

func localArgv(f *cliFixture, args ...string) []string {
	return append([]string{"swarm", "--backend", "local", "--root", f.root}, args...)
}

func TestIssueWorkNextAction(t *testing.T) {
	f := newCLIFixture(t)
	issue := f.create(t, "agent")
	n := strconv.Itoa(issue)
	doc, err := f.runJSON(t, "issue", "work", n, "--agent", "mock", "--progress=false", "--json")
	assertNextAction(t, doc, err, ReasonNeedsApproval, true, ExitNeedsApproval, localArgv(f, "issue", "approve", n)...)
	var approval *lease.TaskApprovalError
	if !errors.As(err, &approval) || !errors.Is(err, coordination.ErrNotFound) || doc["kind"] != "work" {
		t.Fatalf("approval stop lost its cause or kind: %v %v", err, doc)
	}

	f.must(t, "issue", "approve", n)
	t.Setenv("SWARM_MOCK_VERDICT", "CHANGES_REQUESTED")
	doc, err = f.runJSON(t, "issue", "work", n, "--agent", "mock", "--progress=false", "--json")
	assertNextAction(t, doc, err, ReasonChangesRequested, false, ExitChangesRequested, localArgv(f, "issue", "work", n, "--phase", "plan")...)
	if recovery, _ := doc["recovery"].(string); !strings.Contains(recovery, "issue work "+n+" --phase plan") || !errors.Is(err, workflow.ErrPlanReviewRejected) {
		t.Fatalf("legacy recovery or cause lost: %v %v", err, doc)
	}
	for _, a := range f.state(t, issue).Artifacts {
		if a.Type == contract.ArtifactImplementation {
			t.Fatal("rejected plan was implemented")
		}
	}
	if _, err := f.run("issue", "work", n, "--agent", "mock", "--progress=false"); ExitCode(err) != ExitFailure {
		t.Fatalf("unchanged re-plan must stay an ordinary failure: %v", err)
	}

	other := f.create(t, "agent")
	o := strconv.Itoa(other)
	f.must(t, "issue", "approve", o)
	t.Setenv("SWARM_MOCK_VERDICT", "APPROVED")
	doc, err = f.runJSON(t, "issue", "work", o, "--agent", "mock", "--progress=false", "--json")
	assertNoNextAction(t, doc, err)
	if doc["status"] != "completed" {
		t.Fatalf("success = %v", doc)
	}
	doc, err = f.runJSON(t, "issue", "work", o, "--dry-run")
	assertNoNextAction(t, doc, err)
}

func TestIssueWorkHumanGatePauseIsSuccess(t *testing.T) {
	f := newCLIFixture(t)
	n := strconv.Itoa(f.create(t, "human"))
	f.must(t, "issue", "approve", n)
	doc, err := f.runJSON(t, "issue", "work", n, "--agent", "mock", "--progress=false", "--json")
	assertNoNextAction(t, doc, err)
	if doc["status"] != "paused" {
		t.Fatalf("human gate = %v", doc)
	}
	if _, err := f.run("issue", "work", n, "--phase", "implement", "--agent", "mock", "--progress=false"); ExitCode(err) != ExitNeedsApproval || !strings.Contains(err.Error(), "plan approval required") {
		t.Fatalf("implement without human approval: exit %d %v", ExitCode(err), err)
	}
}

func TestIssueSubmitNextAction(t *testing.T) {
	f := newCLIFixture(t)
	issue := f.create(t, "agent")
	n := strconv.Itoa(issue)
	f.must(t, "issue", "approve", n)
	f.must(t, "issue", "work", n, "--agent", "mock", "--progress=false")
	restore := reviseTask(t, f, issue)
	before := f.state(t, issue)
	doc, err := f.runJSON(t, "issue", "submit", n, "--json", "--progress=false")
	assertNextAction(t, doc, err, ReasonNeedsApproval, true, ExitNeedsApproval, localArgv(f, "issue", "approve", n)...)
	if after := f.state(t, issue); after.PRNumber != 0 || after.HeadSHA != before.HeadSHA {
		t.Fatalf("stopped submission changed state: %+v", after)
	}
	restore()
	doc, err = f.runJSON(t, "issue", "submit", n, "--json", "--progress=false")
	assertNoNextAction(t, doc, err)
	if doc["status"] != "submitted" {
		t.Fatalf("submission = %v", doc)
	}
}

func TestPRWorkNextAction(t *testing.T) {
	f, issue, number := submittedPRFixture(t, "agent")
	p := strconv.Itoa(number)
	restore := reviseTask(t, f, issue)
	for _, args := range [][]string{{"work", p, "--agent", "mock", "--progress=false"}, {"rebase", p}, {"submit", p, "--progress=false"}} {
		doc, err := f.runJSON(t, append(append([]string{"pr"}, args...), "--json")...)
		// The linked issue, not the PR, is what a maintainer approves.
		assertNextAction(t, doc, err, ReasonNeedsApproval, true, ExitNeedsApproval, localArgv(f, "issue", "approve", strconv.Itoa(issue))...)
	}
	doc, err := f.runJSON(t, "pr", "work", p, "--dry-run")
	if _, ok := doc["next_action"]; ok || ExitCode(err) != ExitFailure {
		t.Fatalf("dry run reported a next action: %v %v", err, doc)
	}

	restore()
	t.Setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'PR update\\n' >> next-action.txt")
	for _, args := range [][]string{{"work", p, "--agent", "mock", "--progress=false"}, {"rebase", p}, {"validate", p}, {"submit", p, "--progress=false"}} {
		doc, err = f.runJSON(t, append(append([]string{"pr"}, args...), "--json")...)
		assertNoNextAction(t, doc, err)
	}

	t.Setenv("SWARM_MOCK_VERDICT", "CHANGES_REQUESTED")
	doc, err = f.runJSON(t, "pr", "work", p, "--agent", "mock", "--progress=false", "--json")
	assertNextAction(t, doc, err, ReasonChangesRequested, false, ExitChangesRequested, localArgv(f, "pr", "work", p, "--phase", "plan")...)
	if doc["kind"] != "pr-work" || doc["result"] == nil || doc["recovery"] == nil || !errors.Is(err, workflow.ErrPlanReviewRejected) {
		t.Fatalf("PR envelope lost fields: %v", doc)
	}
}

func TestPRRebaseConflictNextAction(t *testing.T) {
	f, issue, number, _ := twoStopPRFixture(t)
	p := strconv.Itoa(number)
	doc, err := f.runJSON(t, "pr", "rebase", p, "--json")
	resume := localArgv(f, "pr", "rebase", p, "--continue", "--resolve")
	assertNextAction(t, doc, err, ReasonConflict, false, ExitConflict, resume...)
	paused := f.state(t, issue)
	for _, args := range [][]string{{"rebase", p}, {"work", p, "--agent", "mock", "--progress=false"}, {"submit", p, "--progress=false"}} {
		doc, err = f.runJSON(t, append(append([]string{"pr"}, args...), "--json")...)
		assertNextAction(t, doc, err, ReasonConflict, false, ExitConflict, resume...)
	}
	if after := f.state(t, issue); after.PRRevision.LocalHeadSHA != paused.PRRevision.LocalHeadSHA || after.PRNumber != paused.PRNumber {
		t.Fatal("conflict stop changed the revision")
	}

	testResolverRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
		return timeoutRunner{&agent.MockRunner{}}, nil
	}
	t.Cleanup(func() { testResolverRunner = nil })
	doc, err = f.runJSON(t, "pr", "rebase", p, "--continue", "--resolve", "--agent", "mock", "--progress=false", "--json")
	assertNextAction(t, doc, err, ReasonConflict, true, ExitConflict, localArgv(f, "pr", "rebase", p, "--continue")...)
	if _, _, rebasing, _ := workspace.PRHead(context.Background(), workspace.PRWorktreePath(f.root, number)); !rebasing {
		t.Fatal("manual conflict ended the rebase")
	}
}

func TestPRWorkGitHubBlockersNextAction(t *testing.T) {
	f, issue, number := submittedPRFixture(t, "agent")
	ctx := context.Background()
	remote := filepath.Join(t.TempDir(), "remote.git")
	prGit(t, f.root, "init", "--bare", remote)
	prGit(t, f.root, "remote", "add", "origin", remote)
	prGit(t, f.root, "push", "origin", "main")
	raw, _, err := f.manager.Store().Load(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = coordination.NewGitStore(f.root).CompareAndSwap(ctx, issue, "", raw); err != nil {
		t.Fatal(err)
	}
	original, err := f.tracker.GetPullRequest(ctx, number)
	if err != nil {
		t.Fatal(err)
	}
	p := strconv.Itoa(number)
	conflicting := false
	for _, tc := range []struct {
		name   string
		moved  bool
		reason string
		human  bool
		code   int
		argv   []string
	}{
		{"conflict", false, ReasonConflict, false, ExitConflict, []string{"swarm", "--backend", "github", "--root", f.root, "pr", "rebase", p, "--resolve"}},
		// A moved base needs re-approval before any rebase, even when conflicting.
		{"moved", true, ReasonNeedsApproval, true, ExitNeedsApproval, []string{"swarm", "--backend", "github", "--root", f.root, "issue", "approve", strconv.Itoa(issue)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pull := *original
			pull.Mergeable, pull.MergeableState = &conflicting, "dirty"
			if tc.moved {
				pull.BaseSHA = "advanced"
			}
			opts := &RootOptions{RepoRoot: f.root, Backend: "github", trackerOverride: workCLITracker{Tracker: f.tracker, pr: pull}}
			doc, err := runJSON(t, opts, "pr", "work", p, "--agent", "mock", "--progress=false", "--json")
			assertNextAction(t, doc, err, tc.reason, tc.human, tc.code, tc.argv...)
			if _, statErr := os.Stat(workspace.PRWorktreePath(f.root, number)); !os.IsNotExist(statErr) {
				t.Fatal("blocked work prepared a worktree")
			}
		})
	}
}
