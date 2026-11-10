package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/pr"
	"github.com/supernovae/swarmos/internal/validation"
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
	opts := &RootOptions{RepoRoot: "/repo"}
	base := []string{"swarm", "--root", "/repo"}
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
		{"issue work", "issue", 7, fmt.Errorf("issue #7: %w", workflow.ErrPreExistingFailures), want{ReasonPreExistingFailures, argv("issue", "status", "7"), true, ExitFailure}},
		{"pr work", "pr", 9, fmt.Errorf("%w: %w", workflow.ErrPreExistingFailures, errors.New("required validation check failed")), want{ReasonPreExistingFailures, argv("pr", "status", "9"), true, ExitFailure}},
		{"issue work", "issue", 7, fmt.Errorf("issue #7: %w after 1 of 1 repair rounds: %w", workflow.ErrValidationRepairExhausted, errors.New("required validation check failed")), want{ReasonValidationRepairExhausted, argv("issue", "work", "7", "--phase", "implement"), false, ExitChangesRequested}},
		{"issue work", "issue", 7, fmt.Errorf("issue #7: %w in repair round 1/1", workflow.ErrValidationRepairNoChanges), want{ReasonValidationRepairNoChanges, argv("issue", "work", "7", "--phase", "implement"), false, ExitChangesRequested}},
		{"pr work", "pr", 9, fmt.Errorf("issue #7: %w after 2 of 2 repair rounds: %w", workflow.ErrValidationRepairExhausted, errors.New("required validation check failed")), want{ReasonValidationRepairExhausted, argv("pr", "work", "9", "--phase", "implement"), false, ExitChangesRequested}},
		{"pr work", "pr", 9, fmt.Errorf("issue #7: %w in repair round 1/1", workflow.ErrValidationRepairNoChanges), want{ReasonValidationRepairNoChanges, argv("pr", "work", "9", "--phase", "implement"), false, ExitChangesRequested}},
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
			if strings.Count(wrapped.Error(), "Next step:") != 1 {
				t.Fatalf("missing or repeated action: %v", wrapped)
			}
			line := strings.Split(wrapped.Error(), "Next step: ")[1]
			parsed, parseErr := exec.Command("sh", "-c", "set -- "+line+"; printf '%s\\0' \"$@\"").Output()
			if parseErr != nil || !reflect.DeepEqual(strings.Split(strings.TrimSuffix(string(parsed), "\x00"), "\x00"), action.Argv) {
				t.Fatalf("shell argv: %q %v", parsed, parseErr)
			}
			if !errors.Is(wrapped, tc.err) || messageOnly(wrapped) != tc.err.Error() {
				t.Fatalf("stop changed the error: %v", wrapped)
			}
		})
	}
	for _, resource := range []string{"issue", "pr"} {
		for _, repo := range []string{"", "acme/widgets"} {
			o := &RootOptions{Repo: repo, Fork: true, RepoRoot: "/contributor", UpstreamRemote: "upstream", ForkRemote: "personal"}
			c := stopContext{opts: o, resource: resource, number: 9}
			plan := &lease.PlanApprovalError{IssueNumber: 7}
			original := fmt.Errorf("%w: %w", workflow.ErrPlanApprovalNeeded, plan)
			stopped := c.stop(original)
			a := nextActionOf(stopped)
			expected := []string{"swarm"}
			if repo != "" {
				expected = append(expected, "--repo", repo)
			}
			expected = append(expected, "issue", "review-plan", "7", "--approve")
			var typed *lease.PlanApprovalError
			if a == nil || !reflect.DeepEqual(a.Argv, expected) || a.Reason != ReasonNeedsApproval || !a.HumanRequired || ExitCode(stopped) != 4 || !errors.As(stopped, &typed) || !errors.Is(stopped, lease.ErrApprovalRequired) || !errors.Is(stopped, workflow.ErrPlanApprovalNeeded) {
				t.Fatalf("fork action: %v %+v", stopped, a)
			}
			if !strings.HasPrefix(stopped.Error(), original.Error()+"\n") || !strings.Contains(stopped.Error(), "upstream maintainer") || !strings.Contains(stopped.Error(), strings.Join(expected, " ")) || (repo == "" && !strings.Contains(stopped.Error(), "Run this command from the upstream repository checkout.")) {
				t.Fatal(stopped)
			}
			root := newRootCmdWithOptions(&RootOptions{})
			cmd, flags, err := root.Find(expected[1:])
			if err != nil || cmd.Name() != "review-plan" {
				t.Fatalf("command: %v %v", cmd, err)
			}
			if err := cmd.ParseFlags(flags); err != nil {
				t.Fatal(err)
			}
			if !cmd.Flags().Changed("approve") || !reflect.DeepEqual(cmd.Flags().Args(), []string{"7"}) {
				t.Fatalf("parsed args: %v", cmd.Flags().Args())
			}
			for _, higher := range []error{workflow.ErrPlanReviewRejected, &lease.TaskApprovalError{IssueNumber: 7}, &lease.ClaimBaseApprovalError{IssueNumber: 7}} {
				nested := errors.Join(higher, plan)
				if got := c.stop(nested); messageOnly(got) != nested.Error() {
					t.Fatalf("precedence changed text: %v", got)
				}
			}
			for _, issue := range []int{0, -1} {
				if a := c.classify(&lease.PlanApprovalError{IssueNumber: issue}); a != nil {
					t.Fatalf("invented issue: %+v", a)
				}
			}
			o.Fork = false
			if got := c.stop(original); got != original {
				t.Fatalf("non-fork changed: %v", got)
			}
		}
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
	fork := &RootOptions{Repo: "acme/widgets", RepoRoot: root, Fork: true, ForkRemote: "my fork"}
	upstream := &RootOptions{RepoRoot: root, ForkRemote: workspace.DefaultForkRemote}
	custom := &RootOptions{ForkRemote: "writable"}
	for _, tc := range []struct {
		name string
		got  *NextAction
		want []string
	}{
		{"fork worker keeps fork flags", stopContext{opts: fork, resource: "pr", number: 3}.classify(&pr.ConflictError{PRNumber: 3, Err: errors.New("x")}),
			[]string{"swarm", "--repo", "acme/widgets", "--root", root, "--fork", "--fork-remote", "my fork", "pr", "rebase", "3", "--resolve"}},
		{"fork approval targets upstream maintainer", stopContext{opts: fork, resource: "issue", number: 2}.classify(&lease.TaskApprovalError{IssueNumber: 2}),
			[]string{"swarm", "--repo", "acme/widgets", "issue", "approve", "2"}},
		{"fork issue revise", stopContext{opts: fork, resource: "issue", number: 2}.classify(workflow.ErrPlanReviewRejected),
			[]string{"swarm", "--repo", "acme/widgets", "--root", root, "--fork", "--fork-remote", "my fork", "issue", "work", "2", "--phase", "plan"}},
		{"default fork remote omitted", stopContext{opts: upstream, resource: "pr", number: 3}.classify(workflow.ErrPlanReviewRejected),
			[]string{"swarm", "--root", root, "pr", "work", "3", "--phase", "plan"}},
		{"upstream-authorized remote kept for pr", stopContext{opts: custom, resource: "pr", number: 3}.classify(workflow.ErrPlanReviewRejected),
			[]string{"swarm", "--fork-remote", "writable", "pr", "work", "3", "--phase", "plan"}},
		{"remote ignored for issue", stopContext{opts: custom, resource: "issue", number: 2}.classify(workflow.ErrPlanReviewRejected),
			[]string{"swarm", "issue", "work", "2", "--phase", "plan"}},
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
	return runJSON(t, f.options(nil), append([]string{"--root", f.root}, args...)...)
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
	if doc["error"] != messageOnly(err) || strings.Contains(fmt.Sprint(doc["error"]), "Next step:") {
		t.Fatalf("JSON presentation leaked: %v", doc)
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
	return append([]string{"swarm", "--root", f.root}, args...)
}

func TestIssueWorkNextAction(t *testing.T) {
	t.Parallel()
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
	f.setenv("SWARM_MOCK_VERDICT", "CHANGES_REQUESTED")
	doc, err = f.runJSON(t, "issue", "work", n, "--agent", "mock", "--progress=false", "--json")
	assertNextAction(t, doc, err, ReasonPlanNoProgress, false, ExitChangesRequested, localArgv(f, "issue", "work", n, "--phase", "plan")...)
	if recovery, _ := doc["recovery"].(string); !strings.Contains(recovery, "issue work "+n+" --phase plan") || !errors.Is(err, workflow.ErrPlanReviewRejected) {
		t.Fatalf("legacy recovery or cause lost: %v %v", err, doc)
	}
	for _, a := range f.state(t, issue).Artifacts {
		if a.Type == contract.ArtifactImplementation {
			t.Fatal("rejected plan was implemented")
		}
	}
	if _, err := f.run("issue", "work", n, "--agent", "mock", "--progress=false"); ExitCode(err) != ExitChangesRequested {
		t.Fatalf("unchanged re-plan must stop with no-progress recovery: %v", err)
	}

	other := f.create(t, "agent")
	o := strconv.Itoa(other)
	f.must(t, "issue", "approve", o)
	f.setenv("SWARM_MOCK_VERDICT", "APPROVED")
	doc, err = f.runJSON(t, "issue", "work", o, "--agent", "mock", "--progress=false", "--json")
	assertNoNextAction(t, doc, err)
	if doc["status"] != "completed" {
		t.Fatalf("success = %v", doc)
	}
	doc, err = f.runJSON(t, "issue", "work", o, "--dry-run")
	assertNoNextAction(t, doc, err)
}

func TestIssueWorkHumanGatePauseIsSuccess(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	f := newCLIFixture(t)
	issue := f.create(t, "agent")
	n := strconv.Itoa(issue)
	f.must(t, "issue", "approve", n)
	f.must(t, "issue", "work", n, "--agent", "mock", "--progress=false")
	seedCompleteIssueAcceptance(t, f, issue)
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
	t.Parallel()
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
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'PR update\\n' >> next-action.txt")
	for _, args := range [][]string{{"work", p, "--agent", "mock", "--progress=false"}, {"rebase", p}, {"validate", p}, {"submit", p, "--progress=false"}} {
		doc, err = f.runJSON(t, append(append([]string{"pr"}, args...), "--json")...)
		assertNoNextAction(t, doc, err)
	}

	f.setenv("SWARM_MOCK_VERDICT", "CHANGES_REQUESTED")
	doc, err = f.runJSON(t, "pr", "work", p, "--agent", "mock", "--progress=false", "--json")
	assertNextAction(t, doc, err, ReasonChangesRequested, false, ExitChangesRequested, localArgv(f, "pr", "work", p, "--phase", "plan")...)
	if doc["kind"] != "pr-work" || doc["result"] == nil || doc["recovery"] == nil || !errors.Is(err, workflow.ErrPlanReviewRejected) {
		t.Fatalf("PR envelope lost fields: %v", doc)
	}
}

func TestPRRebaseConflictNextAction(t *testing.T) {
	t.Parallel()
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

	f.resolverRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
		return timeoutRunner{&agent.MockRunner{}}, nil
	}
	doc, err = f.runJSON(t, "pr", "rebase", p, "--continue", "--resolve", "--agent", "mock", "--progress=false", "--json")
	assertNextAction(t, doc, err, ReasonConflict, true, ExitConflict, localArgv(f, "pr", "rebase", p, "--continue")...)
	if _, _, rebasing, _ := workspace.PRHead(context.Background(), workspace.PRWorktreePath(f.root, number)); !rebasing {
		t.Fatal("manual conflict ended the rebase")
	}
}

func TestPRWorkGitHubBlockersNextAction(t *testing.T) {
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
	if _, err = trustedLedgerStore(t, f.root, "").CompareAndSwap(ctx, issue, "", raw); err != nil {
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
		{"conflict", false, ReasonConflict, false, ExitConflict, []string{"swarm", "--root", f.root, "pr", "rebase", p, "--resolve"}},
		// A moved base needs re-approval before any rebase, even when conflicting.
		{"moved", true, ReasonNeedsApproval, true, ExitNeedsApproval, []string{"swarm", "--root", f.root, "issue", "approve", strconv.Itoa(issue)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pull := *original
			pull.Mergeable, pull.MergeableState = &conflicting, "dirty"
			if tc.moved {
				pull.BaseSHA = "advanced"
			}
			opts := &RootOptions{RepoRoot: f.root, trackerOverride: workCLITracker{Tracker: f.tracker, pr: pull}}
			doc, err := runJSON(t, f.options(opts), "pr", "work", p, "--agent", "mock", "--progress=false", "--json")
			assertNextAction(t, doc, err, tc.reason, tc.human, tc.code, tc.argv...)
			if _, statErr := os.Stat(workspace.PRWorktreePath(f.root, number)); !os.IsNotExist(statErr) {
				t.Fatal("blocked work prepared a worktree")
			}
		})
	}
}

func TestTesterRetryNextActionPreservesSelections(t *testing.T) {
	for _, resource := range []string{"issue", "pr"} {
		opts := &RootOptions{Repo: "org/repo", RepoRoot: "/checkout", UpstreamRemote: "upstream"}
		c := stopContext{opts: opts, resource: resource, number: 42}
		err := &workflow.TestRetryAvailableError{Err: errors.New("tester refused output")}
		action := c.classify(err)
		want := []string{"swarm", "--repo", "org/repo", "--root", "/checkout", "--upstream-remote", "upstream", resource, "work", "42", "--phase", "test"}
		if action == nil || action.Reason != "tester_retry_available" || action.HumanRequired || !reflect.DeepEqual(action.Argv, want) {
			t.Fatalf("action %+v", action)
		}
		if c.classify(errors.New(err.Error())) != nil {
			t.Fatal("error prose conferred eligibility")
		}
		opts.Fork = true
		if c.classify(err) != nil {
			t.Fatal("fork advertised unsupported retry")
		}
	}
}

func TestNextActionHumanMessageBoundary(t *testing.T) {
	c := stopContext{opts: &RootOptions{RepoRoot: "/repo with 'quotes'"}, resource: "issue", number: 7}
	cause := &lease.HumanPlanApprovalError{IssueNumber: 7}
	err := c.stop(cause)
	if strings.Count(err.Error(), "Next step:") != 1 || !strings.Contains(err.Error(), "independent maintainer") {
		t.Fatalf("missing human action: %v", err)
	}
	line := strings.Split(err.Error(), "Next step: ")[1]
	parsed, parseErr := exec.Command("sh", "-c", "set -- "+line+"; printf '%s\\0' \"$@\"").Output()
	if parseErr != nil || !reflect.DeepEqual(strings.Split(strings.TrimSuffix(string(parsed), "\x00"), "\x00"), nextActionOf(err).Argv) {
		t.Fatalf("quoted argv: %q %v", parsed, parseErr)
	}
	if messageOnly(fmt.Errorf("context: %w", err)) != "context: "+cause.Error() {
		t.Fatal("JSON diagnostic context lost")
	}
	if c.stop(fmt.Errorf("context: %w", err)).Error() != "context: "+err.Error() {
		t.Fatal("repeated rendering")
	}
}

func TestNextActionUnknownIdentity(t *testing.T) {
	for _, cause := range []error{workflow.ErrValidationRepairExhausted, workflow.ErrPlanReviewRejected, workflow.ErrPreExistingFailures} {
		if got := (stopContext{resource: "issue"}).classify(cause); got != nil {
			t.Fatalf("unknown issue: %+v", got)
		}
	}
}

func TestNextActionNoReadyCarrier(t *testing.T) {
	cause := workflow.ErrValidationRepairExhausted
	c := stopContext{opts: &RootOptions{Repo: "acme/repo", RepoRoot: "/a 'repo'"}, resource: "issue", number: 19}
	e := &noReadyError{cause: cause, action: c.classify(cause), text: "Issue #19: automatic allowance exhausted"}
	if !errors.Is(e, errNoReadyWork) || !errors.Is(e, cause) || ExitCode(e) != ExitFailure {
		t.Fatal(e)
	}
	var stop *stopError
	if errors.As(e, &stop) {
		t.Fatal("selection returned a stop")
	}
	wrapped := (stopContext{resource: "issue"}).stop(e)
	if ExitCode(wrapped) != ExitChangesRequested || !reflect.DeepEqual(nextActionOf(wrapped).Argv, e.action.Argv) || strings.Count(wrapped.Error(), "Next step:") != 1 {
		t.Fatal(wrapped)
	}
	if messageOnly(fmt.Errorf("context: %w", wrapped)) != "context: "+e.text {
		t.Fatal("message context lost")
	}
}

func TestIssueWorkHumanPlanDecisionSurfaces(t *testing.T) {
	f, n, _, actor, run := recoveryCommandFixture(t, "local", "human", 1)
	number := strconv.Itoa(n)
	out, err := run("issue", "work", number, "--phase", "plan", "--agent", "mock", "--progress=false")
	if err != nil || strings.Count(out, "Next step:") != 1 || !strings.Contains(out, "issue review-plan "+number+" --approve") {
		t.Fatalf("plan: %v %s", err, out)
	}
	out, err = run("issue", "status", number)
	if err != nil || strings.Count(out, "Next step:") != 1 {
		t.Fatalf("status: %v %s", err, out)
	}
	out, err = run("issue", "approve", number)
	if err == nil || !strings.Contains(err.Error(), "independent maintainer") || strings.Contains(err.Error(), "release") {
		t.Fatalf("own claim: %v %s", err, out)
	}
	jsonOut, refusalErr := run("issue", "approve", number, "--json")
	var refusalDoc map[string]any
	if decodeErr := json.Unmarshal([]byte(jsonOut[strings.Index(jsonOut, "{"):]), &refusalDoc); decodeErr != nil {
		t.Fatal(decodeErr, jsonOut)
	}
	if ExitCode(refusalErr) != ExitFailure || refusalDoc["error"] != messageOnly(refusalErr) || strings.Contains(fmt.Sprint(refusalDoc["error"]), "Next step:") || refusalDoc["next_action"] == nil {
		t.Fatal(refusalDoc, refusalErr)
	}

	st, err := f.manager.GetCurrentState(t.Context(), n)
	if err != nil {
		t.Fatal(err)
	}
	issue, err := f.tracker.GetIssue(t.Context(), n)
	if err != nil {
		t.Fatal(err)
	}
	wu, err := contract.ParseWorkUnitFromIssue(n, issue.Title, issue.Body)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := harness.LoadConfigAt(f.root, st.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*lease.IssueState)
		want   bool
	}{
		{"first plan", func(*lease.IssueState) {}, true},
		{"not human", func(s *lease.IssueState) { s.EffectiveGate = contract.GateAgent }, false},
		{"stale task", func(s *lease.IssueState) { s.TaskDigest = "stale" }, false},
		{"stale policy", func(s *lease.IssueState) { s.PolicyDigest = "stale" }, false},
		{"human rejection", func(s *lease.IssueState) { s.HumanPlanReview = &lease.PlanRejection{} }, false},
		{"pending rejection", func(s *lease.IssueState) { s.PendingPlanReview = &lease.PlanRejection{} }, false},
		{"missing pair", func(s *lease.IssueState) { s.Artifacts = nil }, false},
		{"completed", func(s *lease.IssueState) { s.Lease.Status = "completed" }, false},
		{"linked PR", func(s *lease.IssueState) { s.Lease.Status = "completed"; s.PRNumber = 99 }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := *st
			tc.change(&copy)
			if got := needsHumanPlanDecision(&copy, wu, cfg); got != tc.want {
				t.Fatalf("got %t", got)
			}
		})
	}
	*actor = "independent-maintainer"
	if out, err = run("issue", "review-plan", number, "--approve"); err != nil {
		t.Fatal(err, out)
	}
	*actor = "alice"
	out, err = run("issue", "status", number)
	wantArgv := []string{"swarm", "--repo", "octo/swarm", "--root", f.root, "issue", "work", number}
	if err != nil || strings.Count(out, "Next step:") != 1 {
		t.Fatalf("approved status: %v %s", err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Next step:") && line != "Next step: "+formatCommand(wantArgv) {
			t.Fatalf("approved next step: %s", line)
		}
	}
	for _, guidance := range []string{"Ask an", "issue release", "review-plan", "issue approve"} {
		if strings.Contains(out, guidance) {
			t.Fatalf("approved status suggests human intervention (%q): %s", guidance, out)
		}
	}
	jsonOut, err = run("issue", "status", number, "--json")
	if err != nil {
		t.Fatal(err, jsonOut)
	}
	var approvedDoc struct {
		Issues []struct {
			Issue struct {
				Number int `json:"number"`
			} `json:"issue"`
			NextAction *struct {
				Argv          []string `json:"argv"`
				Reason        string   `json:"reason"`
				HumanRequired *bool    `json:"human_required"`
			} `json:"next_action"`
		} `json:"issues"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &approvedDoc); err != nil {
		t.Fatal(err, jsonOut)
	}
	if len(approvedDoc.Issues) != 1 || approvedDoc.Issues[0].Issue.Number != n {
		t.Fatalf("approved status issue: %s", jsonOut)
	}
	action := approvedDoc.Issues[0].NextAction
	if action == nil || !slices.Equal(action.Argv, wantArgv) || action.Reason != "resumable_work" || action.HumanRequired == nil || *action.HumanRequired {
		t.Fatalf("approved status action: %s", jsonOut)
	}
	st, err = f.manager.GetCurrentState(t.Context(), n)
	if err != nil {
		t.Fatal(err)
	}
	if needsHumanPlanDecision(st, wu, cfg) {
		t.Fatal("approved plan still needs a human decision")
	}
	out, err = run("issue", "approve", number)
	if err == nil || !strings.Contains(err.Error(), "task approval is current") || strings.Contains(err.Error(), "Next step:") || strings.Contains(err.Error(), "release") {
		t.Fatalf("current: %v %s", err, out)
	}
	issue.Body = strings.Replace(issue.Body, wu.Objective, wu.Objective+" changed", 1)
	if _, err = f.tracker.UpdateIssueBody(t.Context(), n, issue.Body); err != nil {
		t.Fatal(err)
	}
	out, err = run("issue", "approve", number)
	if err == nil || !strings.Contains(err.Error(), "task reapproval required") || !strings.Contains(err.Error(), "issue release "+number) || strings.Contains(err.Error(), "review-plan") {
		t.Fatalf("stale task: %v %s", err, out)
	}

}

func TestIssueWorkPhaseDiagnostics(t *testing.T) {
	for _, resource := range []string{"issue", "pr"} {
		cmd := newRootCmdWithOptions(&RootOptions{})
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{resource, "work", "7", "--phase", "invalid"})
		err := cmd.ExecuteContext(t.Context())
		if err == nil || !strings.Contains(err.Error(), "valid: all, plan, review, implement, test") || strings.Contains(err.Error(), "Next step:") {
			t.Fatal(resource, err)
		}
	}
}

func TestNextActionOperationGuidance(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
		no   string
	}{
		{[]string{"issue", "approve", "7"}, "authorized maintainer", "independent"},
		{[]string{"issue", "review-plan", "7", "--approve"}, "independent maintainer", "authorized maintainer"},
		{[]string{"issue", "review-plan", "7"}, "authorized maintainer", "independent"},
		{[]string{"issue", "release", "7"}, "active claim owner", "maintainer"},
		{[]string{"issue", "status", "7"}, "Next step:", "maintainer"},
		{[]string{"pr", "rebase", "9", "--continue"}, "Next step:", "maintainer"},
	} {
		got := renderNextAction(&NextAction{Argv: append([]string{"swarm"}, tc.args...), HumanRequired: true})
		if !strings.Contains(got, tc.want) || strings.Contains(got, tc.no) {
			t.Fatal(got)
		}
	}
}

func TestIssue466StatusSelectors(t *testing.T) {
	f, n := issue466Fixture(t)
	st := f.state(t, n)
	iss, err := f.tracker.GetIssue(t.Context(), n)
	if err != nil {
		t.Fatal(err)
	}
	wu, err := contract.ParseWorkUnitFromIssue(n, iss.Title, iss.Body)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := harness.LoadConfigAt(f.root, st.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	opts := &RootOptions{Repo: "owner/repo with spaces", RepoRoot: "/checkout with spaces", UpstreamRemote: "upstream space"}
	a := (stopContext{opts: opts, number: n, resource: "issue"}).statusAction(st, wu, cfg, nil, nil, "alice", st.Lease.ClaimID, time.Now())
	if a == nil {
		t.Fatal("no action")
	}
	for _, value := range []string{opts.Repo, opts.RepoRoot, opts.UpstreamRemote} {
		if !slices.Contains(a.Argv, value) || !strings.Contains(renderNextAction(a), "'"+value+"'") {
			t.Fatalf("literal selector lost: %v", a)
		}
	}
}

func TestIssuePartialImplementationStatusAndNextAction(t *testing.T) {
	f := newCLIFixture(t)
	n := strconv.Itoa(f.create(t, "agent"))
	f.must(t, "issue", "approve", n)
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf work > real.txt; printf 'Status: PARTIAL\nUnmet criterion: SDK integration\n' > .swarm/active/IMPLEMENTATION.md")
	doc, err := f.runJSON(t, "issue", "work", n, "--agent", "mock", "--progress=false", "--json")
	assertNextAction(t, doc, err, ReasonPartialImplementation, false, ExitFailure, localArgv(f, "issue", "work", n, "--phase", "plan")...)
	if !strings.Contains(err.Error(), "swarm issue edit "+n) {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"issue", "status", n}, {"issue", "status", n, "--json"}} {
		out, e := f.run(args...)
		if e != nil || !strings.Contains(out, "partial") || !strings.Contains(out, "Unmet criterion: SDK integration") {
			t.Fatalf("%s %v", out, e)
		}
	}
	wrapped := fmt.Errorf("%w: %w", &workflow.PartialImplementationError{IssueNumber: 1, Details: "unmet"}, workflow.ErrValidationRepairExhausted)
	c := stopContext{resource: "issue", number: 1}
	if a := c.classify(wrapped); a == nil || a.Reason != ReasonPartialImplementation {
		t.Fatalf("%+v", a)
	}
	if a := (stopContext{resource: "pr", number: 1}).classify(&workflow.PartialImplementationError{IssueNumber: 1}); a != nil {
		t.Fatal(a)
	}
}

func TestIssuePartialImplementationCurrentOutcome(t *testing.T) {
	st := &lease.IssueState{HeadSHA: "head", BaseSHA: "base", TaskDigest: "task", PolicyDigest: "policy", Artifacts: []contract.Artifact{{Type: contract.ArtifactDesign, Body: "plan"}, {Type: contract.ArtifactTestPlan, Body: "tests"}}}
	md := lease.ReviewBindings(st)
	md["head_sha"] = "head"
	md["implementation_outcome"] = "partial"
	st.Artifacts = append(st.Artifacts, contract.Artifact{Type: contract.ArtifactImplementation, Verdict: "FAIL", Metadata: md})
	if !currentPartialImplementation(st) {
		t.Fatal("current partial not selected")
	}
	for k := range md {
		if k == "review_claim_id" {
			continue
		}
		old := md[k]
		md[k] = "stale"
		if currentPartialImplementation(st) {
			t.Fatalf("accepted stale %s", k)
		}
		md[k] = old
	}
	for _, verdict := range []string{"PASS", "FAIL"} {
		st.Artifacts = append(st.Artifacts, contract.Artifact{Type: contract.ArtifactImplementation, Verdict: verdict})
		if currentPartialImplementation(st) {
			t.Fatal("superseded partial")
		}
		st.Artifacts = st.Artifacts[:len(st.Artifacts)-1]
	}
}

func TestRequiredCheckLimitNextAction(t *testing.T) {
	t.Run("printed work stops and complete retries", requiredCheckLimitNextActionCompleteRetry)
	f, n := issue466Fixture(t)
	st := f.state(t, n)
	iss, err := f.tracker.GetIssue(t.Context(), n)
	if err != nil {
		t.Fatal(err)
	}
	wu, err := contract.ParseWorkUnitFromIssue(n, iss.Title, iss.Body)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := harness.LoadConfigAt(f.root, st.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range []string{"issue", "pr"} {
		for _, outcome := range []string{validation.OutcomeTimeLimit, validation.OutcomeStalled} {
			t.Run(resource+"/"+outcome, func(t *testing.T) {
				number := n
				if resource == "pr" {
					number = n + 100
				}
				opts := &RootOptions{Repo: "owner/project", RepoRoot: "/checkout with spaces", UpstreamRemote: "upstream"}
				c := stopContext{opts: opts, resource: resource, number: number}
				typed := fmt.Errorf("wrapped: %w", errors.Join(workflow.ErrValidationRepairExhausted, &validation.LimitError{Outcome: outcome}))
				want := c.argv(false, resource, "validate", strconv.Itoa(number))
				assert := func(a *NextAction) {
					t.Helper()
					if a == nil || a.Reason != ReasonRequiredCheckLimit || !reflect.DeepEqual(a.Argv, want) || strings.Contains(renderNextAction(a), "--phase") {
						t.Fatalf("retry action: %+v want %v", a, want)
					}
				}
				assert(c.classify(typed))
				if stopped := c.stop(typed); !strings.Contains(stopped.Error(), "Next step:") || !strings.Contains(stopped.Error(), resource+" validate") {
					t.Fatalf("work-stop output: %v", stopped)
				}
				cmd, _, findErr := NewRootCmd().Find([]string{resource, "validate", strconv.Itoa(number)})
				if findErr != nil || cmd.Name() != "validate" {
					t.Fatalf("unknown emitted command: %v %v", cmd, findErr)
				}
				current := *st
				current.Artifacts = append(append([]contract.Artifact{}, st.Artifacts...), contract.Artifact{Type: contract.ArtifactValidation, Verdict: "FAIL", Metadata: map[string]string{"validation_trusted_origin": "coordinator", "head_sha": st.HeadSHA, "base_sha": st.BaseSHA, validation.LimitMetadataKey: outcome}})
				status := func() *NextAction {
					return c.statusAction(&current, wu, cfg, nil, nil, "alice", st.Lease.ClaimID, time.Now())
				}
				repairData, _ := json.Marshal(validation.RepairSummary{HeadSHA: st.HeadSHA})
				current.Artifacts[len(current.Artifacts)-1].Metadata[validation.RepairMetadataKey] = string(repairData)
				assert(status())
				if resource == "issue" {
					issue466State(t, f, n, func(saved *lease.IssueState) { saved.Artifacts = current.Artifacts })
					printed, e := f.run("issue", "status", strconv.Itoa(n))
					if e != nil || !strings.Contains(printed, "issue validate "+strconv.Itoa(n)) || strings.Contains(printed, "--phase implement") {
						t.Fatalf("status output: %v\n%s", e, printed)
					}
				}
				for _, key := range []string{"validation_trusted_origin", "head_sha", "base_sha", validation.LimitMetadataKey} {
					md := current.Artifacts[len(current.Artifacts)-1].Metadata
					saved := md[key]
					md[key] = "stale-or-malformed"
					if a := status(); a != nil && a.Reason == ReasonRequiredCheckLimit {
						t.Fatalf("accepted %s", key)
					}
					md[key] = saved
				}
				current.Artifacts = append(current.Artifacts, contract.Artifact{Type: contract.ArtifactValidation, Verdict: "PASS"})
				if a := status(); a != nil && a.Reason == ReasonRequiredCheckLimit {
					t.Fatal("old limit outranks latest pass")
				}
				if a := c.classify(errors.Join(typed, &lease.TaskApprovalError{IssueNumber: n})); a == nil || a.Reason != ReasonNeedsApproval {
					t.Fatal("limit outranks approval")
				}
				if a := c.classify(errors.Join(typed, lease.ErrClaimFence)); a != nil {
					t.Fatal("limit outranks fencing")
				}
				if a := c.classify(workflow.ErrValidationRepairExhausted); a == nil || a.Reason != ReasonValidationRepairExhausted {
					t.Fatal("ordinary failure changed")
				}
			})
		}
	}
}

func requiredCheckLimitNextActionCompleteRetry(t *testing.T) {
	for _, resource := range []string{"issue", "pr"} {
		t.Run(resource, func(t *testing.T) {
			counters := t.TempDir()
			setup := func(f *cliFixture) {
				policy := "required_checks:\n  - name: first\n    command: echo ran >> '" + filepath.Join(counters, "first") + "'\n    timeout: 1m\n  - name: second\n    command: echo ran >> '" + filepath.Join(counters, "second") + "'; if test -f trigger; then sleep 2; fi\n    timeout: 50ms\nprotected_paths: ['.swarm/**']\n"
				if err := os.WriteFile(filepath.Join(f.root, ".swarm/validation/required-checks.yaml"), []byte(policy), 0644); err != nil {
					t.Fatal(err)
				}
				prGit(t, f.root, "commit", "-am", "required retry checks")
				f.setenv("SWARM_MOCK_DEVELOPER_CMD", "echo implemented > feature.txt")
			}
			var f *cliFixture
			var issue, number int
			if resource == "pr" {
				f, issue, number = submittedPRFixture(t, "agent", setup)
			} else {
				f = newCLIFixture(t)
				setup(f)
				issue = f.create(t, "agent")
				number = issue
				f.must(t, "issue", "approve", strconv.Itoa(issue))
			}
			f.setenv("SWARM_MOCK_DEVELOPER_CMD", "echo timeout > trigger; echo changed >> feature.txt")
			n := strconv.Itoa(number)
			doc, err := f.runJSON(t, resource, "work", n, "--agent", "mock", "--headless", "--progress=false", "--json")
			assertNextAction(t, doc, err, ReasonRequiredCheckLimit, false, ExitFailure, localArgv(f, resource, "validate", n)...)
			var limit *validation.LimitError
			if !errors.As(err, &limit) {
				t.Fatalf("typed limit lost in actual work: %v", err)
			}
			initial := f.state(t, issue)
			if initial.ValidationRepairCount != 0 {
				t.Fatal("work reserved repair")
			}
			if resource == "issue" {
				out, e := f.run("issue", "status", n)
				if e != nil || !strings.Contains(out, "issue validate "+n) || strings.Contains(out, "--phase implement") {
					t.Fatalf("status: %v %s", e, out)
				}
			}
			// Change the external runtime condition while preserving candidate and policy.
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "sleep"), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			for _, name := range []string{"first", "second"} {
				if err := os.WriteFile(filepath.Join(counters, name), nil, 0644); err != nil {
					t.Fatal(err)
				}
			}
			// Execute the exact emitted resource, operation and number. f.run supplies
			// the same repository selection already present in localArgv.
			action := nextActionOf(err)
			if action == nil {
				t.Fatal("missing next action")
			}
			args := append([]string{}, action.Argv[3:]...)
			args = append(args, "--progress=false")
			if out, err := f.run(args...); err != nil {
				t.Fatalf("retry %v: %v %s", args, err, out)
			}
			for _, name := range []string{"first", "second"} {
				data, _ := os.ReadFile(filepath.Join(counters, name))
				if string(data) != "ran\n" {
					t.Fatalf("check %s not rerun once: %q", name, data)
				}
			}
			final := f.state(t, issue)
			if final.ValidationRepairCount != initial.ValidationRepairCount || final.HeadSHA != initial.HeadSHA {
				t.Fatal("check retry changed implementation or repair count")
			}
		})
	}
}
