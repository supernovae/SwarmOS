package cli

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/fork"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/testutil"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workflow"
	"github.com/supernovae/swarmos/internal/workspace"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type cliFixture struct {
	root    string
	tracker *tracker.LocalTracker
	manager *lease.Manager
	env     *fixtureEnv
	// resolverRunner replaces PR work and conflict resolver runners for this
	// fixture's later commands.
	resolverRunner func(name string, cfg *contract.SwarmConfig, local bool) (agent.Runner, error)
}

// fixtureEnv is the environment one fixture's commands see in place of the
// process environment. Names it does not hold read the process environment,
// whose user directories TestMain points at a temporary root.
type fixtureEnv struct {
	mu   sync.Mutex
	vars map[string]*string
}

// newFixtureEnv owns execution mode, user directories and profile paths, and
// clears the SWARM_* selectors and mock controls a host might have set.
func newFixtureEnv(t *testing.T) *fixtureEnv {
	t.Helper()
	e := &fixtureEnv{vars: map[string]*string{}}
	// Fixtures intentionally run local mock agents and validation.
	e.set("SWARM_EXECUTION_MODE", "trusted-local")
	for _, name := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "APPDATA"} {
		e.set(name, t.TempDir())
	}
	e.set("SWARM_REGISTRY_PATH", filepath.Join(t.TempDir(), "repos.yaml"))
	e.set("SWARM_NODE_CONFIG", filepath.Join(t.TempDir(), "node.yaml"))
	for _, name := range []string{"SWARM_BACKEND", "SWARM_REPO", "SWARM_REPOS_DIR", "SWARM_MOCK_VERDICT", "SWARM_MOCK_DEVELOPER_CMD"} {
		e.unset(name)
	}
	return e
}

func (e *fixtureEnv) set(name, value string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.vars[name] = &value
}

func (e *fixtureEnv) unset(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.vars[name] = nil
}

// lookup returns a snapshot, so later set calls never change a command that
// is already built.
func (e *fixtureEnv) lookup() func(string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	vars := make(map[string]*string, len(e.vars))
	for name, value := range e.vars {
		vars[name] = value
	}
	return func(name string) (string, bool) {
		if value, held := vars[name]; held {
			if value == nil {
				return "", false
			}
			return *value, true
		}
		return os.LookupEnv(name)
	}
}

func newCLIFixture(t *testing.T) *cliFixture {
	t.Helper()
	env := newFixtureEnv(t)
	env.set("SWARM_USER", "alice")
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := gitfixture.Command(t, "", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
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
	tr := tracker.NewLocalTracker(root).WithUser("alice", "")
	cfg, err := harness.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	manager := lease.NewManager(tr, cfg.Security)
	if err := manager.ConfigureStore(root); err != nil {
		t.Fatal(err)
	}
	return &cliFixture{root: root, tracker: tr, manager: manager, env: env}
}

// setenv changes a variable for this fixture's later commands only.
func (f *cliFixture) setenv(name, value string) { f.env.set(name, value) }

// options gives opts (nil for defaults) this fixture's environment and
// resolver runner in place of the process environment.
func (f *cliFixture) options(opts *RootOptions) *RootOptions {
	if opts == nil {
		opts = &RootOptions{}
	}
	opts.lookupEnv = f.env.lookup()
	opts.resolverRunner = f.resolverRunner
	return opts
}

// command builds a root command from opts (nil for defaults) that reads this
// fixture's environment.
func (f *cliFixture) command(opts *RootOptions) *cobra.Command {
	return newRootCmdWithOptions(f.options(opts))
}

func (f *cliFixture) run(args ...string) (string, error) {
	cmd := f.command(nil)
	var output, diagnostics bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&diagnostics)
	cmd.SetArgs(append([]string{"--root", f.root, "--backend", "local"}, args...))
	err := cmd.Execute()
	if err != nil {
		output.Write(diagnostics.Bytes())
	}
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
	t.Parallel()
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
	if _, err := f.run("issue", "work", n, "--agent", "mock", "--headless"); err == nil {
		t.Fatal("unapproved work executed")
	}
	f.must(t, "issue", "approve", n)
	result := f.must(t, "issue", "work", n, "--agent", "mock", "--headless", "--json")
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
	f.must(t, "issue", "submit", n)
	first := f.state(t, number)
	if first.PRNumber == 0 || first.Lease.Status != "completed" {
		t.Fatalf("submission not durably recorded: %+v", first)
	}
	f.must(t, "issue", "submit", n)
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
	t.Parallel()
	f := newCLIFixture(t)
	number := f.create(t, "human")
	n := strconv.Itoa(number)
	f.must(t, "issue", "approve", n)
	f.must(t, "issue", "work", n, "--agent", "mock", "--headless")
	var paused struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(f.must(t, "issue", "work", n, "--agent", "mock", "--json")), &paused); err != nil {
		t.Fatal(err)
	}
	if paused.Status != "paused" {
		t.Fatalf("retry before human approval must remain paused, got %q", paused.Status)
	}
	if _, err := f.run("issue", "submit", n); err == nil {
		t.Fatal("submission bypassed human approval")
	}
	f.must(t, "issue", "review-plan", n, "--agent", "mock")
	if _, err := f.run("issue", "work", n, "--phase", "implement", "--gate", "none", "--agent", "mock"); err == nil {
		t.Fatal("agent review satisfied human gate")
	}
	f.setenv("SWARM_USER", "independent-maintainer")
	f.must(t, "issue", "review-plan", n, "--approve", "--notes", "Checked exact plan and test plan")
	f.setenv("SWARM_USER", "alice")
	f.must(t, "issue", "work", n, "--agent", "mock", "--headless")
	f.must(t, "issue", "submit", n)
}

func TestCLISelectorsFailBeforeClaiming(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	number := f.create(t, "agent")
	for _, args := range [][]string{
		{"issue", "work", "1", "--next"}, {"issue", "work", "1", "--phase", "typo"}, {"issue", "work", "1", "--phase", "full"},
		{"issue", "work", "1", "--phase", "submitted"}, {"issue", "work", "1", "--gate", "typo"}, {"issue", "review-plan", "1", "--approve", "--reject"},
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

func TestCLIClaimNextSelectorsAndPreparedWork(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	number := f.create(t, "agent")
	n := strconv.Itoa(number)
	for _, args := range [][]string{
		{"issue", "claim", "1", "--next"}, {"issue", "claim"},
		{"issue", "claim", "1", "--all"}, {"issue", "claim", "1", "--duration", "0s"},
		{"issue", "claim", "--next", "--duration", "-1m"},
	} {
		if _, err := f.run(args...); err == nil {
			t.Fatalf("invalid claim accepted: %v", args)
		}
	}
	if _, err := f.manager.GetCurrentState(context.Background(), number); !errors.Is(err, coordination.ErrNotFound) {
		t.Fatalf("invalid claim changed state: %v", err)
	}
	if _, err := f.run("issue", "claim", "--next"); !errors.Is(err, errNoReadyWork) {
		t.Fatalf("empty queue: %v", err)
	}
	f.must(t, "issue", "approve", n)
	f.must(t, "repo", "add", "octo/repo", f.root)
	raw := f.must(t, "issue", "claim", "--next", "--all", "--agent", "mock", "--json")
	var outcome struct {
		Kind      string         `json:"kind"`
		Status    string         `json:"status"`
		Selection pulledWorkUnit `json:"selection"`
	}
	if err := json.Unmarshal([]byte(raw), &outcome); err != nil {
		t.Fatal(err)
	}
	if outcome.Kind != "claim" || outcome.Status != "claimed" || outcome.Selection.Repo != "octo/repo" ||
		outcome.Selection.Issue == nil || outcome.Selection.Issue.Number != number ||
		outcome.Selection.TargetPhase != "all" || outcome.Selection.WorktreePath == "" {
		t.Fatalf("incorrect selected repository or task: %+v", outcome)
	}
	// Git can report a canonical path while TempDir retains a symlinked prefix.
	selectedRoot, err := os.Stat(outcome.Selection.RepoRoot)
	if err != nil {
		t.Fatal(err)
	}
	fixtureRoot, err := os.Stat(f.root)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(selectedRoot, fixtureRoot) {
		t.Fatalf("selected repository %q is not fixture %q", outcome.Selection.RepoRoot, f.root)
	}
	before := f.state(t, number)
	if before.Lease.Status != "active" || len(before.Artifacts) != 0 {
		t.Fatalf("selection ran work: %+v", before)
	}
	f.must(t, "issue", "work", n, "--agent", "mock", "--progress=false")
	if after := f.state(t, number); after.Lease.ClaimID != before.Lease.ClaimID {
		t.Fatal("explicit work did not resume the prepared claim")
	}
}

func TestCLIClaimNextResumesHandoffPhase(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	n := strconv.Itoa(f.create(t, "agent"))
	f.must(t, "issue", "approve", n)
	f.must(t, "issue", "work", n, "--agent", "mock", "--phase", "plan", "--handoff", "--progress=false")
	before := f.state(t, 1)
	worktree := workspace.WorktreePath(f.root, 1)
	checkpoint, err := gitfixture.Command(t, "", "-C", worktree, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	raw := f.must(t, "issue", "claim", "--next", "--agent", "mock", "--json")
	var outcome struct {
		Selection pulledWorkUnit `json:"selection"`
	}
	if err := json.Unmarshal([]byte(raw), &outcome); err != nil {
		t.Fatal(err)
	}
	after := f.state(t, 1)
	selectedHead, err := gitfixture.Command(t, "", "-C", worktree, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Selection.TargetPhase != "review" || !bytes.Equal(selectedHead, checkpoint) || !reflect.DeepEqual(after.Artifacts, before.Artifacts) {
		t.Fatalf("claim restarted handed-off plan: before=%+v after=%+v selection=%+v", before, after, outcome.Selection)
	}
}

func TestCLIValidationNamespacesAndRemovedRoutes(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	issue := f.create(t, "agent")
	n := strconv.Itoa(issue)
	for _, args := range [][]string{
		{"init"}, {"doctor"}, {"plan", "--scan"}, {"claim", n}, {"pull"}, {"work", n},
		{"stage", n}, {"review", n}, {"validate", n}, {"submit", n}, {"status", n},
		{"renew", n}, {"release", n}, {"issue", "pull"}, {"issue", "review", n},
		{"repo", "bogus"}, {"node", "bogus"}, {"issue", "epic", "bogus"}, {"repo", "list", "extra"},
		{"repo", "doctor", "extra"}, {"node", "status", "extra"}, {"version", "extra"},
		{"issue", "stage", n, "--phase", "implement", "--handoff"}, {"pr", "stage", "1", "--phase", "implement", "--handoff"},
	} {
		if _, err := f.run(args...); err == nil {
			t.Fatalf("removed route accepted: %v", args)
		}
	}
	if _, err := f.manager.GetCurrentState(context.Background(), issue); !errors.Is(err, coordination.ErrNotFound) {
		t.Fatalf("removed routes mutated task state: %v", err)
	}
	for _, args := range [][]string{
		{"repo", "validate"}, {"repo", "validate", n, "--base", "HEAD"},
		{"repo", "validate", "--base", "HEAD"}, {"repo", "validate", "--ci", "--base", "HEAD"},
		{"issue", "validate", n, "--ci"},
	} {
		if _, err := f.run(args...); err == nil {
			t.Fatalf("invalid validation accepted: %v", args)
		}
	}
	base, err := gitfixture.Command(t, "", "-C", f.root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	raw := f.must(t, "repo", "validate", "--base", strings.TrimSpace(string(base)), "--json")
	var outcome struct {
		Kind   string `json:"kind"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(raw), &outcome); err != nil || outcome.Kind != "validation" || outcome.Status != "passed" {
		t.Fatalf("repo validation: %s (%v)", raw, err)
	}
	if _, err := f.manager.GetCurrentState(context.Background(), issue); !errors.Is(err, coordination.ErrNotFound) {
		t.Fatalf("repo validation wrote task artifacts: %v", err)
	}
	f.must(t, "issue", "approve", n)
	f.must(t, "issue", "claim", n, "--agent", "mock")
	if _, err := f.run("issue", "validate", n, "--base", strings.Repeat("a", 40)); err == nil || !strings.Contains(err.Error(), "approved immutable base") {
		t.Fatalf("mismatched issue base accepted: %v", err)
	}
	if _, err := f.run("issue", "validate", n); err == nil {
		t.Fatal("issue validation accepted missing task artifacts")
	}
}

func TestCLIRepoValidationFailurePreservesJSONAndExitStatus(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	policy := "required_checks:\n  - name: deliberate-failure\n    command: 'false'\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**']\n"
	if err := os.WriteFile(filepath.Join(f.root, ".swarm", "validation", "required-checks.yaml"), []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "failing trusted policy"}} {
		if out, err := gitfixture.Command(t, "", append([]string{"-C", f.root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	base, err := gitfixture.Command(t, "", "-C", f.root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := f.run("repo", "validate", "--base", strings.TrimSpace(string(base)), "--json")
	if err == nil {
		t.Fatal("failed trusted check returned success")
	}
	var outcome struct {
		Kind   string `json:"kind"`
		Status string `json:"status"`
	}
	if jsonErr := json.Unmarshal([]byte(raw), &outcome); jsonErr != nil || outcome.Kind != "validation" || outcome.Status != "failed" {
		t.Fatalf("invalid failed validation JSON: %s (%v)", raw, jsonErr)
	}
}

func TestCLIRepoValidateReportsProtectedChangesForMaintainers(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	checks := filepath.Join(f.root, ".swarm", "validation", "required-checks.yaml")
	policy := "required_checks:\n  - name: ok\n    command: 'true'\n    timeout: 1m\nprotected_paths: ['.swarm/**']\n"
	if err := os.WriteFile(checks, []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "trusted policy"}} {
		if out, err := gitfixture.Command(t, "", append([]string{"-C", f.root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	out, err := gitfixture.Command(t, "", "-C", f.root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	base := strings.TrimSpace(string(out))
	if err := os.WriteFile(checks, []byte(policy+"# maintainer edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := f.run("repo", "validate", "--base", base); err == nil {
		t.Fatal("protected change passed the default enforce mode")
	}
	if _, err := f.run("repo", "validate", "--base", base, "--protected-paths", "skip"); err == nil {
		t.Fatal("unknown --protected-paths mode accepted")
	}
	got := f.must(t, "repo", "validate", "--base", base, "--protected-paths", "report")
	for _, want := range []string{"PROTECTED .swarm/validation/required-checks.yaml", "NOTE .swarm/ policy changed", "PASS ok"} {
		if !strings.Contains(got, want) {
			t.Fatalf("report output missing %q:\n%s", want, got)
		}
	}
}

func TestCLIIssueLeafJSONAndReviewDelegation(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	raw := f.must(t, "issue", "create", "--json", "--title", "JSON WorkUnit", "--objective", "Check leaf output",
		"--constraint", "Keep policy unchanged", "--acceptance", "Pass checks", "--gate", "agent")
	var created struct {
		Kind  string         `json:"kind"`
		Issue *tracker.Issue `json:"issue"`
	}
	if err := json.Unmarshal([]byte(raw), &created); err != nil || created.Kind != "issue" || created.Issue == nil {
		t.Fatalf("create JSON: %s (%v)", raw, err)
	}
	n := strconv.Itoa(created.Issue.Number)
	for _, tc := range []struct {
		args []string
		kind string
	}{
		{[]string{"issue", "lint", n, "--json"}, "issue-lint"},
		{[]string{"issue", "approve", n, "--json"}, "task-approval"},
	} {
		raw := f.must(t, tc.args...)
		var result struct {
			Kind                string `json:"kind"`
			SuggestedDimensions *struct {
				Size string `json:"size"`
			} `json:"suggested_dimensions"`
		}
		if err := json.Unmarshal([]byte(raw), &result); err != nil || result.Kind != tc.kind {
			t.Fatalf("%v: %s (%v)", tc.args, raw, err)
		}
		if tc.kind == "issue-lint" && (result.SuggestedDimensions == nil || result.SuggestedDimensions.Size == "") {
			t.Fatalf("lint JSON should include an advisory shape: %s", raw)
		}
	}
	if out := f.must(t, "issue", "lint", n); !strings.Contains(out, "is a valid WorkUnit") || !strings.Contains(out, "Suggested dimensions:") || !strings.Contains(out, "hint:") {
		t.Fatalf("lint text should keep its verdict and add a shape and hints:\n%s", out)
	}
	f.must(t, "issue", "work", n, "--agent", "mock", "--phase", "plan", "--progress=false")
	raw = f.must(t, "issue", "review-plan", n, "--agent", "mock", "--json")
	var review struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(raw), &review); err != nil || review.Kind != "work" {
		t.Fatalf("review delegation JSON: %s (%v)", raw, err)
	}
}

func TestNodeAdmissionLimitAndFailedExecutionStatus(t *testing.T) {
	t.Parallel()
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
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "exit 17")
	if _, err := f.run("node", "run", "--max-jobs", "1", "--agent", "mock"); err == nil {
		t.Fatal("failed developer produced successful node exit")
	}
}

func TestEmptyNodeQueueIsSuccessfulDrain(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	f.must(t, "node", "run", "--agent", "mock")
}

func TestNodeHumanGateWaitsThenResumesImplementation(t *testing.T) {
	t.Parallel()
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
	f.must(t, "issue", "review-plan", n, "--approve")
	f.must(t, "node", "run", "--agent", "mock", "--max-jobs", "1", "--auto-submit")
	if completed := f.state(t, issue); completed.PRNumber == 0 || completed.Lease.Status != "completed" {
		t.Fatalf("approved human handoff did not reach submission: %+v", completed)
	}
}

func TestCLIRenewalLimitReturnsErrorWithoutChangingLease(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	number := f.create(t, "agent")
	n := strconv.Itoa(number)
	f.must(t, "issue", "approve", n)
	f.must(t, "issue", "claim", n, "--agent", "mock")
	for range f.state(t, number).Lease.MaxRenewals {
		f.must(t, "issue", "renew", n, "--json")
	}
	before := f.state(t, number).Lease
	if _, err := f.run("issue", "renew", n, "--json"); !errors.Is(err, lease.ErrMaxRenewals) {
		t.Fatalf("renewal limit must reach the caller: %v", err)
	}
	after := f.state(t, number).Lease
	if after.Renewals != before.Renewals || !after.ExpiresAt.Equal(before.ExpiresAt) {
		t.Fatalf("failed renewal changed the lease: before=%+v after=%+v", before, after)
	}
}

func TestCLIInteractiveStageProvenanceAndPlanCommand(t *testing.T) {
	t.Parallel()
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
	f.must(t, "issue", "claim", n, "--agent", "mock")

	wtPath := filepath.Join(f.root, ".swarm", "worktrees", "issue-"+n)
	activeDir := filepath.Join(wtPath, ".swarm", "active")
	if err := os.WriteFile(filepath.Join(activeDir, "PLAN.md"), []byte("# Interactive Plan\n\n1. Add feature.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(activeDir, "TEST_PLAN.md"), []byte("# Interactive Test Plan\n\n- Run git --version\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	f.must(t, "issue", "stage", n, "--phase", "plan", "--harness", "claude", "--model", "claude-sonnet-4-5")
	f.must(t, "issue", "review-plan", n, "--agent", "mock")

	if err := os.WriteFile(filepath.Join(wtPath, "feature.txt"), []byte("shipped\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.must(t, "issue", "stage", n, "--phase", "implement", "--harness", "codex", "--model", "gpt-5-codex")
	f.must(t, "issue", "submit", n)

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

	// Verify swarm repo plan --from preview vs --apply --json and --scan CLI commands
	roadmapFile := filepath.Join(f.root, "ROADMAP.md")
	roadmapContent := "# CLI Roadmap\n\n## First Roadmap Item\n### Objective\nBuild part one.\n### Acceptance Criteria\n- [ ] Check one\n"
	if err := os.WriteFile(roadmapFile, []byte(roadmapContent), 0o644); err != nil {
		t.Fatal(err)
	}
	previewOut := f.must(t, "repo", "plan", "--from", roadmapFile)
	if !strings.Contains(previewOut, "`Step 1`") || strings.Contains(previewOut, "`#1`") {
		t.Fatalf("expected preview output to use Step 1 and not fake #1:\n%s", previewOut)
	}
	planJSON := f.must(t, "repo", "plan", "--from", roadmapFile, "--apply", "--json")
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
	repeatJSON := f.must(t, "repo", "plan", "--from", roadmapFile, "--apply", "--json")
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
	scanOut := f.must(t, "repo", "plan", "--scan", "--apply")
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
	t.Parallel()
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
	if _, err := f.run("issue", "claim", epicNumStr); err == nil {
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

	// Even if an Epic is mislabeled swarm:ready, pullNextReadyIssue skips it and claims the approved child
	_ = f.tracker.AddLabels(ctx, epicRes.Issue.Number, []string{"swarm:ready"})
	f.must(t, "issue", "approve", strconv.Itoa(childRes.Issue.Number))
	f.must(t, "issue", "work", "--next", "--agent", "mock", "--headless")
	if st := f.state(t, childRes.Issue.Number); st.Lease.ClaimID == "" {
		t.Fatalf("expected child #%d to be pulled and worked past mislabeled Epic #%d", childRes.Issue.Number, epicRes.Issue.Number)
	}

	// Read-only doctor with github backend reports WARN for missing Epic type and non-maintainer without creating issues
	cfgPath := filepath.Join(f.root, ".swarm", "config.yaml")
	cfgBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfgText := strings.Replace(string(cfgBytes), "default_agent: codex", "default_agent: mock", 1)
	cfgText += "\n  mock:\n    binary: git\n    harness: mock\n"
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0600); err != nil {
		t.Fatal(err)
	}
	mockTr := &mockDoctorTracker{
		Tracker:     f.tracker,
		epicTypeErr: errors.New("issue type \"Epic\" is not enabled"),
		authErr:     errors.New("actor lacks maintainer permission"),
	}
	docCmd := f.command(&RootOptions{
		RepoRoot:        f.root,
		Backend:         "github",
		trackerOverride: mockTr,
	})
	var docBuf bytes.Buffer
	docCmd.SetOut(&docBuf)
	docCmd.SetErr(&docBuf)
	docCmd.SetArgs([]string{"--root", f.root, "repo", "doctor", "--backend", "github"})
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

func TestForkCLISmoke(t *testing.T) {
	t.Parallel()
	env := newFixtureEnv(t)
	ctx := context.Background()
	tmp := t.TempDir()

	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := gitfixture.Command(t, "", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v in %s: %v (%s)", args, dir, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	originBare := filepath.Join(tmp, "octo", "swarm.git")
	aliceForkBare := filepath.Join(tmp, "alice", "swarm.git")
	bobForkBare := filepath.Join(tmp, "bob", "swarm.git")
	for _, dir := range []string{originBare, aliceForkBare, bobForkBare} {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	git(tmp, "init", "--bare", "--initial-branch=main", originBare)

	upstreamRoot := filepath.Join(tmp, "upstream")
	git(tmp, "clone", originBare, upstreamRoot)
	git(upstreamRoot, "config", "user.name", "Maintainer")
	git(upstreamRoot, "config", "user.email", "maintainer@example.invalid")
	pin := "a1b2c3d4e5f60123456789abcdef0123456789ab"
	if _, err := harness.InitRepository(harness.InitOptions{
		RepoRoot:        upstreamRoot,
		SwarmVersion:    pin,
		ForkCoordinator: true,
	}); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(upstreamRoot, ".swarm", "config.yaml")
	cfgBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfgWithMock := string(cfgBytes) + "\n  mock:\n    binary: git\n    harness: mock\n"
	if err := os.WriteFile(cfgPath, []byte(cfgWithMock), 0o644); err != nil {
		t.Fatal(err)
	}
	checksPolicy := "required_checks:\n  - name: git-available\n    command: git --version\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**']\n"
	if err := os.WriteFile(filepath.Join(upstreamRoot, ".swarm", "validation", "required-checks.yaml"), []byte(checksPolicy), 0o644); err != nil {
		t.Fatal(err)
	}
	git(upstreamRoot, "add", ".")
	git(upstreamRoot, "commit", "-m", "approved upstream baseline")
	git(upstreamRoot, "push", "origin", "main")
	git(originBare, "symbolic-ref", "HEAD", "refs/heads/main")
	shaA := git(upstreamRoot, "rev-parse", "HEAD")

	git(tmp, "clone", "--bare", originBare, aliceForkBare)
	git(tmp, "clone", "--bare", originBare, bobForkBare)

	var mu sync.Mutex
	now := time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC)
	nextCommentID := int64(1000)
	nextPRNum := 0
	wuBody := "```yaml swarm-workunit\nid: ISSUE-42\nobjective: Exercise fork contributor flow\nconstraints: [No upstream push access]\nacceptance: [Cross-repo Draft PR opened]\nworkflow: feature\ngate: agent\nallow_harness_changes: true\n```"
	issues := map[int]*tracker.Issue{
		42: {
			Number:  42,
			Title:   "Fork mode smoke task",
			Body:    wuBody,
			State:   "open",
			Author:  "maintainer",
			HTMLURL: "https://github.com/octo/swarm/issues/42",
		},
	}
	comments := make(map[int64]*tracker.Comment)
	issueComments := make(map[int][]int64)
	prs := make(map[int]*tracker.PullRequest)

	resolveHeadSHA := func(head string) string {
		parts := strings.SplitN(head, ":", 2)
		branch := head
		bare := originBare
		if len(parts) == 2 {
			branch = parts[1]
			switch parts[0] {
			case "alice":
				bare = aliceForkBare
			case "bob":
				bare = bobForkBare
			}
		}
		out, err := gitfixture.Command(t, "", "-C", bare, "rev-parse", "refs/heads/"+branch).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}

	newGHTracker := func(login, userType string) *tracker.GitHubTracker {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			writeResp := func(v any) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(v)
			}
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/user":
				writeResp(map[string]any{"login": login, "type": userType})
			case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/octo/swarm/collaborators/"):
				perm := "read"
				if login == "maintainer" {
					perm = "admin"
				}
				writeResp(map[string]any{"permission": perm})
			case r.Method == http.MethodGet && (r.URL.Path == "/repos/alice/swarm" || r.URL.Path == "/repos/bob/swarm"):
				owner := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")[0]
				writeResp(map[string]any{
					"full_name":   owner + "/swarm",
					"name":        "swarm",
					"fork":        true,
					"owner":       map[string]any{"login": owner},
					"source":      map[string]any{"full_name": "octo/swarm"},
					"permissions": map[string]any{"push": login == owner},
				})
			case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/octo/swarm/issues/comments/"):
				id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/repos/octo/swarm/issues/comments/"), 10, 64)
				c, ok := comments[id]
				if !ok {
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				writeResp(map[string]any{
					"id":         c.ID,
					"body":       c.Body,
					"created_at": c.CreatedAt,
					"updated_at": c.UpdatedAt,
					"user":       map[string]any{"login": c.Author, "type": c.AuthorType},
				})
			case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/repos/octo/swarm/issues/comments/"):
				id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/repos/octo/swarm/issues/comments/"), 10, 64)
				c, ok := comments[id]
				if !ok {
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				var payload struct {
					Body string `json:"body"`
				}
				_ = json.NewDecoder(r.Body).Decode(&payload)
				c.Body = payload.Body
				writeResp(map[string]any{
					"id":         c.ID,
					"body":       c.Body,
					"created_at": c.CreatedAt,
					"updated_at": c.UpdatedAt,
					"user":       map[string]any{"login": c.Author, "type": c.AuthorType},
				})
			case strings.HasPrefix(r.URL.Path, "/repos/octo/swarm/issues/"):
				rest := strings.TrimPrefix(r.URL.Path, "/repos/octo/swarm/issues/")
				parts := strings.Split(rest, "/")
				issueNum, _ := strconv.Atoi(parts[0])
				iss, ok := issues[issueNum]
				if !ok {
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				if len(parts) == 1 && r.Method == http.MethodGet {
					var lbs []map[string]string
					for _, l := range iss.Labels {
						lbs = append(lbs, map[string]string{"name": l})
					}
					writeResp(map[string]any{
						"id":     issueNum,
						"number": iss.Number,
						"title":  iss.Title,
						"body":   iss.Body,
						"state":  iss.State,
						"user":   map[string]any{"login": iss.Author},
						"labels": lbs,
					})
					return
				}
				if len(parts) == 2 && parts[1] == "comments" {
					if r.Method == http.MethodGet {
						var list []map[string]any
						for _, cid := range issueComments[issueNum] {
							c := comments[cid]
							list = append(list, map[string]any{
								"id":         c.ID,
								"body":       c.Body,
								"created_at": c.CreatedAt,
								"updated_at": c.UpdatedAt,
								"user":       map[string]any{"login": c.Author, "type": c.AuthorType},
							})
						}
						writeResp(list)
						return
					}
					if r.Method == http.MethodPost {
						var payload struct {
							Body string `json:"body"`
						}
						_ = json.NewDecoder(r.Body).Decode(&payload)
						nextCommentID++
						c := &tracker.Comment{
							ID:         nextCommentID,
							IssueNum:   issueNum,
							Author:     login,
							AuthorType: userType,
							Body:       payload.Body,
							CreatedAt:  now,
							UpdatedAt:  now,
						}
						comments[c.ID] = c
						issueComments[issueNum] = append(issueComments[issueNum], c.ID)
						writeResp(map[string]any{
							"id":         c.ID,
							"body":       c.Body,
							"created_at": c.CreatedAt,
							"updated_at": c.UpdatedAt,
							"user":       map[string]any{"login": c.Author, "type": c.AuthorType},
						})
						return
					}
				}
				if len(parts) == 2 && parts[1] == "labels" && r.Method == http.MethodPost {
					if login != "maintainer" && login != fork.ActionsBotLogin {
						http.Error(w, "contributor cannot mutate upstream labels", http.StatusForbidden)
						return
					}
					var payload struct {
						Labels []string `json:"labels"`
					}
					_ = json.NewDecoder(r.Body).Decode(&payload)
					for _, lb := range payload.Labels {
						if !slices.Contains(iss.Labels, lb) {
							iss.Labels = append(iss.Labels, lb)
						}
					}
					writeResp(map[string]any{})
					return
				}
				if len(parts) == 3 && parts[1] == "labels" && r.Method == http.MethodDelete {
					if login != "maintainer" && login != fork.ActionsBotLogin {
						http.Error(w, "contributor cannot mutate upstream labels", http.StatusForbidden)
						return
					}
					target := parts[2]
					var kept []string
					for _, lb := range iss.Labels {
						if lb != target {
							kept = append(kept, lb)
						}
					}
					iss.Labels = kept
					w.WriteHeader(http.StatusNoContent)
					return
				}
			case r.URL.Path == "/repos/octo/swarm/git/ref/heads/main" && r.Method == http.MethodGet:
				writeResp(map[string]any{"object": map[string]string{"sha": resolveHeadSHA("main")}})
				return
			case r.URL.Path == "/repos/octo/swarm/pulls" && r.Method == http.MethodGet:
				wantHead := r.URL.Query().Get("head")
				wantBase := r.URL.Query().Get("base")
				var list []map[string]any
				for _, pr := range prs {
					qualified := pr.Head
					if pr.HeadRepo != "" {
						owner := strings.Split(pr.HeadRepo, "/")[0]
						qualified = owner + ":" + pr.Head
					}
					if pr.Draft && qualified == wantHead && pr.Base == wantBase {
						list = append(list, map[string]any{
							"number":   pr.Number,
							"title":    pr.Title,
							"body":     pr.Body,
							"draft":    pr.Draft,
							"html_url": pr.HTMLURL,
						})
					}
				}
				writeResp(list)
				return
			case r.URL.Path == "/repos/octo/swarm/pulls" && r.Method == http.MethodPost:
				var payload struct {
					Title string `json:"title"`
					Body  string `json:"body"`
					Head  string `json:"head"`
					Base  string `json:"base"`
					Draft bool   `json:"draft"`
				}
				_ = json.NewDecoder(r.Body).Decode(&payload)
				nextPRNum++
				headBranch := payload.Head
				headRepo := "octo/swarm"
				if parts := strings.SplitN(payload.Head, ":", 2); len(parts) == 2 {
					headRepo = parts[0] + "/swarm"
					headBranch = parts[1]
				}
				pr := &tracker.PullRequest{
					Number:   nextPRNum,
					Title:    payload.Title,
					Body:     payload.Body,
					Head:     headBranch,
					HeadRepo: headRepo,
					HeadSHA:  resolveHeadSHA(payload.Head),
					Base:     payload.Base,
					BaseRepo: "octo/swarm",
					Draft:    payload.Draft,
					HTMLURL:  fmt.Sprintf("https://github.com/octo/swarm/pull/%d", nextPRNum),
				}
				prs[pr.Number] = pr
				writeResp(map[string]any{
					"number":   pr.Number,
					"title":    pr.Title,
					"body":     pr.Body,
					"draft":    pr.Draft,
					"html_url": pr.HTMLURL,
				})
				return
			case strings.HasPrefix(r.URL.Path, "/repos/octo/swarm/pulls/") && r.Method == http.MethodPatch:
				prNum, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/repos/octo/swarm/pulls/"))
				pr, ok := prs[prNum]
				if !ok {
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				var payload struct {
					Title string `json:"title"`
					Body  string `json:"body"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				pr.Title, pr.Body = payload.Title, payload.Body

				writeResp(map[string]any{
					"number":   pr.Number,
					"title":    pr.Title,
					"body":     pr.Body,
					"draft":    pr.Draft,
					"html_url": pr.HTMLURL,
					"head":     map[string]any{"ref": pr.Head},
					"base":     map[string]any{"ref": pr.Base},
				})
				return
			case strings.HasPrefix(r.URL.Path, "/repos/octo/swarm/pulls/") && r.Method == http.MethodGet:
				prNum, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/repos/octo/swarm/pulls/"))
				pr, ok := prs[prNum]
				if !ok {
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				writeResp(map[string]any{
					"number":   pr.Number,
					"title":    pr.Title,
					"body":     pr.Body,
					"state":    "open",
					"draft":    pr.Draft,
					"html_url": pr.HTMLURL,
					"head": map[string]any{
						"ref":  pr.Head,
						"sha":  pr.HeadSHA,
						"repo": map[string]any{"full_name": pr.HeadRepo},
					},
					"base": map[string]any{
						"ref":  pr.Base,
						"repo": map[string]any{"full_name": pr.BaseRepo},
					},
				})
				return
			default:
				http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			}
		})
		client, err := api.NewRESTClient(api.ClientOptions{
			Host:      "github.com",
			AuthToken: "test-token",
			Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				cloned := req.Clone(req.Context())
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, cloned)
				resp := rec.Result()
				resp.Request = cloned
				return resp, nil
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		return tracker.NewGitHubTrackerWithClient("octo", "swarm", client)
	}

	maintTracker := newGHTracker("maintainer", "User")
	botTracker := newGHTracker(fork.ActionsBotLogin, "Bot")
	aliceTracker := newGHTracker("alice", "User")
	bobTracker := newGHTracker("bob", "User")

	coord := &fork.Coordinator{
		RepoRoot:     upstreamRoot,
		UpstreamRepo: "octo/swarm",
		Tracker:      botTracker,
		NowFn:        func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
		ForkGitURLFunc: func(forkRepo string) string {
			switch forkRepo {
			case "alice/swarm":
				return aliceForkBare
			case "bob/swarm":
				return bobForkBare
			default:
				return filepath.Join(tmp, "missing.git")
			}
		},
	}
	dispatch := func(ctx context.Context, issueNumber int, c *tracker.Comment) error {
		var ev fork.IssueCommentEvent
		ev.Action = "created"
		ev.Repository.FullName = "octo/swarm"
		ev.Issue.Number = issueNumber
		ev.Issue.State = "open"
		ev.Comment.ID = c.ID
		ev.Comment.Body = c.Body
		ev.Comment.CreatedAt = c.CreatedAt
		ev.Comment.UpdatedAt = c.UpdatedAt
		ev.Comment.User.Login = c.Author
		ev.Comment.User.Type = c.AuthorType
		_, err := coord.HandleEvent(ctx, ev)
		return err
	}

	var lastDiagnostics string
	runCmd := func(opts RootOptions, args ...string) (string, error) {
		opts.forkPollInterval = 5 * time.Millisecond
		opts.forkPollTimeout = 30 * time.Second
		opts.forkDispatcher = dispatch
		opts.nowFn = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
		opts.lookupEnv = env.lookup()
		cmd := newRootCmdWithOptions(&opts)
		var out, diagnostics bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&diagnostics)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(ctx)
		if strings.Contains(out.String(), "upstream fork coordinator Action") {
			t.Fatal("fork progress on stdout")
		}
		if slices.Contains(args, "--json") {
			decoder := json.NewDecoder(strings.NewReader(out.String()))
			var result map[string]any
			if e := decoder.Decode(&result); e != nil {
				t.Fatalf("invalid JSON stdout: %v: %s", e, out.String())
			}
			if result["kind"] == nil {
				t.Fatalf("missing JSON kind: %v", result)
			}
			if e := decoder.Decode(new(any)); e != io.EOF {
				t.Fatalf("extra stdout: %v", e)
			}
		}
		lastDiagnostics = diagnostics.String()
		return out.String(), err
	}

	// Require reporting at explicit mutation calls, including rejected requests.
	runMutation := func(operation, outcome string, opts RootOptions, args ...string) (string, error) {
		t.Helper()
		out, err := runCmd(opts, args...)
		posted := regexp.MustCompile(`(?m)^Fork ` + operation + ` request ([1-9][0-9]*): waiting on the upstream fork coordinator Action$`).FindStringSubmatchIndex(lastDiagnostics)
		if posted == nil {
			t.Fatalf("missing posted %s progress: %s", operation, lastDiagnostics)
		}
		id := lastDiagnostics[posted[2]:posted[3]]
		terminal := regexp.MustCompile(`(?m)^Fork ` + operation + ` request ` + id + `: ` + outcome + ` \(elapsed [0-9]+(?:h|m|s)[0-9hms.]*\)$`).FindStringIndex(lastDiagnostics)
		if terminal == nil || terminal[0] <= posted[1] {
			t.Fatalf("missing ordered %s outcome: %s", outcome, lastDiagnostics)
		}
		return out, err
	}

	// 0. Flag & backend validation checks
	if _, err := runCmd(RootOptions{RepoRoot: upstreamRoot}, "--backend", "local", "--fork", "issue", "status"); err == nil {
		t.Fatal("expected --backend local --fork to fail explicitly")
	}
	if _, err := runCmd(RootOptions{RepoRoot: upstreamRoot}, "--fork-remote", "fork", "issue", "status"); err == nil {
		t.Fatal("expected --fork-remote without --fork to fail explicitly")
	}
	if _, err := runCmd(RootOptions{RepoRoot: upstreamRoot, trackerOverride: aliceTracker}, "--fork", "issue", "approve", "42"); err == nil {
		t.Fatal("expected maintainer issue command to fail with --fork")
	}
	if _, err := runCmd(RootOptions{RepoRoot: upstreamRoot, trackerOverride: aliceTracker}, "--fork", "issue", "review-plan", "42", "--approve"); err == nil {
		t.Fatal("expected human review --approve to fail with --fork")
	}
	for _, args := range [][]string{
		{"issue", "create", "--title", "forbidden"},
		{"issue", "lint", "42"},
		{"issue", "decompose", "42"},
		{"issue", "epic", "create"},
	} {
		call := append([]string{"--fork", "--repo", "octo/swarm"}, args...)
		if _, err := runCmd(RootOptions{RepoRoot: upstreamRoot, trackerOverride: aliceTracker}, call...); err == nil || !strings.Contains(err.Error(), "maintainer-only") {
			t.Fatalf("--repo bypassed fork restriction on %v: %v", args, err)
		}
	}
	if _, err := runCmd(RootOptions{RepoRoot: upstreamRoot, trackerOverride: aliceTracker},
		"--fork", "--repo", "octo/swarm", "issue", "review-plan", "42", "--approve"); err == nil || !strings.Contains(err.Error(), "maintainer-only") {
		t.Fatalf("--repo bypassed human review restriction: %v", err)
	}
	if _, err := runCmd(RootOptions{RepoRoot: upstreamRoot, trackerOverride: aliceTracker},
		"--fork", "--repo", "octo/swarm", "repo", "plan", "--scan", "--apply"); err == nil || !strings.Contains(err.Error(), "maintainer-only") {
		t.Fatalf("--repo bypassed project plan restriction: %v", err)
	}

	// 1. Maintainer approves issue #42 at SHA A on upstream
	if out, err := runCmd(RootOptions{RepoRoot: upstreamRoot, Backend: "github", trackerOverride: maintTracker}, "issue", "approve", "42"); err != nil {
		t.Fatalf("maintainer issue approve 42 failed: %v\n%s", err, out)
	}

	// 2. Prepare alice's clone where origin push is denied and fork remote points to aliceForkBare
	aliceRoot := filepath.Join(tmp, "alice-clone")
	git(tmp, "clone", originBare, aliceRoot)
	git(aliceRoot, "config", "user.name", "Alice")
	git(aliceRoot, "config", "user.email", "alice@example.invalid")
	git(aliceRoot, "remote", "add", "fork", aliceForkBare)
	git(aliceRoot, "remote", "set-url", "--push", "origin", filepath.Join(tmp, "no-upstream-push.git"))

	// 3. Alice runs `swarm --fork issue claim 42 --agent mock`
	claimOut, err := runMutation("acquire", "accepted", RootOptions{RepoRoot: aliceRoot, Backend: "github", trackerOverride: aliceTracker}, "--fork", "issue", "claim", "42", "--agent", "mock")
	if err != nil {
		t.Fatalf("alice --fork claim 42 failed: %v\n%s", err, claimOut)
	}
	t.Logf("claim output:\n%s", claimOut)
	if out, err := runMutation("renew", "accepted", RootOptions{RepoRoot: aliceRoot, Backend: "github", trackerOverride: aliceTracker}, "--fork", "issue", "renew", "42", "--json"); err != nil {
		t.Fatalf("JSON renew: %v %s", err, out)
	} else {
		var result struct {
			Kind        string           `json:"kind"`
			IssueNumber int              `json:"issue_number"`
			State       lease.IssueState `json:"state"`
		}
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Fatal(err)
		}
		if result.Kind != "renew" || result.IssueNumber != 42 || result.State.Lease.Owner != "alice" || result.State.Lease.Status != "active" || result.State.BaseSHA != shaA || result.State.ForkRepo != "alice/swarm" {
			t.Fatalf("unexpected renew JSON: %s", out)
		}
	}

	upstreamStore := trustedLedgerStore(t, upstreamRoot, "")
	readUpstreamState := func() *lease.IssueState {
		t.Helper()
		raw, _, err := upstreamStore.Load(ctx, 42)
		if err != nil {
			t.Fatal(err)
		}
		var st lease.IssueState
		if err := json.Unmarshal(raw, &st); err != nil {
			t.Fatal(err)
		}
		return &st
	}
	stClaimed := readUpstreamState()
	aliceClaimID := stClaimed.Lease.ClaimID
	expectedAliceBranch := fmt.Sprintf("swarm/issue-42-%s", aliceClaimID)
	if stClaimed.BaseSHA != shaA || stClaimed.Lease.Owner != "alice" || stClaimed.ForkRepo != "alice/swarm" || aliceClaimID == "" {
		t.Fatalf("unexpected upstream state after alice claim: %+v", stClaimed)
	}
	if !strings.Contains(claimOut, expectedAliceBranch) {
		t.Fatalf("claim output did not report fork branch %s:\n%s", expectedAliceBranch, claimOut)
	}

	// 4. Contributor review is advisory: the authenticated maintainer must
	// approve the current plan before the contributor can resume implementation.
	workOut, err := runCmd(RootOptions{RepoRoot: aliceRoot, Backend: "github", trackerOverride: aliceTracker}, "--fork", "issue", "work", "42", "--agent", "mock", "--headless")
	if !errors.Is(err, workflow.ErrPlanApprovalNeeded) {
		t.Fatalf("expected contributor review to stop at the trusted approval gate, got %v\n%s", err, workOut)
	}
	if out, err := runCmd(RootOptions{RepoRoot: upstreamRoot, Backend: "github", trackerOverride: maintTracker}, "issue", "review-plan", "42", "--approve", "--notes", "Reviewed the contributor plan and test plan"); err != nil {
		t.Fatalf("maintainer review failed: %v\n%s", err, out)
	}
	workOut, err = runCmd(RootOptions{RepoRoot: aliceRoot, Backend: "github", trackerOverride: aliceTracker}, "--fork", "issue", "work", "42", "--agent", "mock", "--headless", "--phase", "implement")
	if err != nil {
		t.Fatalf("alice --fork work 42 --phase implement after trusted approval failed: %v\n%s", err, workOut)
	}
	t.Logf("work output:\n%s", workOut)

	wt42 := workspace.WorktreePath(aliceRoot, 42)
	legitimateCheckpoint := readUpstreamState().HeadSHA
	if got := git(originBare, "rev-parse", upstreamStore.Layout().CheckpointRef(42, legitimateCheckpoint)); got != legitimateCheckpoint {
		t.Fatal("verified fork checkpoint was not retained upstream")
	}
	if err := os.WriteFile(filepath.Join(wt42, "feature.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 5. Tampering with .swarm/validation/required-checks.yaml or .github/workflows/swarm-fork-coordinator.yml in fork worktree must fail submission BEFORE PR creation despite allow_harness_changes: true
	origChecks, err := os.ReadFile(filepath.Join(wt42, ".swarm", "validation", "required-checks.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt42, ".swarm", "validation", "required-checks.yaml"), []byte("required_checks: []\nprotected_paths: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runCmd(RootOptions{RepoRoot: aliceRoot, Backend: "github", trackerOverride: aliceTracker}, "--fork", "issue", "submit", "42"); err == nil {
		t.Fatalf("expected --fork submit with tampered required-checks.yaml to fail, got:\n%s", out)
	}
	// Discard the rejected tamper while preserving the recorded checkpoint.
	git(wt42, "reset", "--hard", legitimateCheckpoint)
	if err := os.WriteFile(filepath.Join(wt42, ".swarm", "validation", "required-checks.yaml"), origChecks, 0o644); err != nil {
		t.Fatal(err)
	}
	origWorkflow, err := os.ReadFile(filepath.Join(wt42, ".github", "workflows", "swarm-fork-coordinator.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt42, ".github", "workflows", "swarm-fork-coordinator.yml"), []byte("name: Tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runCmd(RootOptions{RepoRoot: aliceRoot, Backend: "github", trackerOverride: aliceTracker}, "--fork", "issue", "submit", "42"); err == nil {
		t.Fatalf("expected --fork submit with tampered workflow to fail, got:\n%s", out)
	}
	mu.Lock()
	prCountAfterTamper := len(prs)
	mu.Unlock()
	if prCountAfterTamper != 0 {
		t.Fatalf("expected 0 PRs created after rejected fork submissions, got %d", prCountAfterTamper)
	}

	// Continue additively from the recorded checkpoint with legitimate feature.go.
	git(wt42, "reset", "--hard", legitimateCheckpoint)
	if err := os.WriteFile(filepath.Join(wt42, ".github", "workflows", "swarm-fork-coordinator.yml"), origWorkflow, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt42, "feature.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Origin moves independently while the writable fork keeps its old main.
	if err := os.WriteFile(filepath.Join(upstreamRoot, "feature.go"), []byte("package upstream\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git(upstreamRoot, "add", "feature.go")
	git(upstreamRoot, "commit", "-m", "Advance target with conflicting feature")
	git(upstreamRoot, "push", "origin", "main")
	if got := git(aliceForkBare, "rev-parse", "main"); got != shaA {
		t.Fatal("fork main moved unexpectedly", got)
	}

	// 6. Alice runs `swarm --fork issue submit 42`: records checkpoint SHA B on fork & upstream, creates Draft PR with head=alice:swarm/issue-42-<claim_id> and base=main
	submitOut, err := runCmd(RootOptions{RepoRoot: aliceRoot, Backend: "github", trackerOverride: aliceTracker}, "--fork", "issue", "submit", "42")
	if err != nil {
		t.Fatalf("alice --fork submit 42 failed: %v\n%s", err, submitOut)
	}
	if !strings.Contains(submitOut, "behind the target by 1 commits") || !strings.Contains(submitOut, "swarm --fork pr rebase 1 --resolve") || !strings.Contains(submitOut, "feature.go") {
		t.Fatal(submitOut)
	}

	t.Logf("submit output:\n%s", submitOut)

	shaB := git(wt42, "rev-parse", "HEAD")
	stSubmitted := readUpstreamState()
	if stSubmitted.HeadSHA != shaB || stSubmitted.Branch != expectedAliceBranch || stSubmitted.PRNumber != 1 || stSubmitted.Lease.Status != "completed" {
		t.Fatalf("unexpected upstream state after submit: %+v (want shaB=%s)", stSubmitted, shaB)
	}
	// Repeat submit is idempotent and returns the same PR
	repeatOut, err := runCmd(RootOptions{RepoRoot: aliceRoot, Backend: "github", trackerOverride: aliceTracker}, "--fork", "issue", "submit", "42")
	if err != nil {
		t.Fatalf("repeat --fork submit 42 failed: %v\n%s", err, repeatOut)
	}
	mu.Lock()
	totalPRs := len(prs)
	createdPR := prs[1]
	mu.Unlock()
	if totalPRs != 1 || createdPR == nil || createdPR.Head != expectedAliceBranch || createdPR.HeadRepo != "alice/swarm" || createdPR.Base != "main" {
		t.Fatalf("unexpected PR state after repeat submit: count=%d pr=%+v", totalPRs, createdPR)
	}

	if !strings.Contains(createdPR.Body, "swarm --fork pr rebase 1 --resolve") || !strings.Contains(createdPR.Body, "feature.go") {
		t.Fatal("missing fork freshness note", createdPR.Body)
	}
	if stSubmitted.BaseSHA != shaA {
		t.Fatal("freshness changed approved base")
	}

	// 7. Expiry and bob takeover on a second issue #43: alice's old claim cannot mutate upstream state
	mu.Lock()
	issues[43] = &tracker.Issue{
		Number:  43,
		Title:   "Takeover task",
		Body:    strings.ReplaceAll(wuBody, "ISSUE-42", "ISSUE-43"),
		State:   "open",
		Author:  "maintainer",
		HTMLURL: "https://github.com/octo/swarm/issues/43",
	}
	mu.Unlock()
	if out, err := runCmd(RootOptions{RepoRoot: upstreamRoot, Backend: "github", trackerOverride: maintTracker}, "issue", "approve", "43"); err != nil {
		t.Fatalf("approve 43: %v\n%s", err, out)
	}
	if out, err := runCmd(RootOptions{RepoRoot: aliceRoot, Backend: "github", trackerOverride: aliceTracker}, "--fork", "issue", "claim", "43", "--agent", "mock", "--json"); err != nil {
		t.Fatalf("alice claim 43: %v\n%s", err, out)
	}

	// Advance clock past lease expiry and have bob claim #43
	mu.Lock()
	now = now.Add(5 * time.Hour)
	mu.Unlock()

	bobRoot := filepath.Join(tmp, "bob-clone")
	git(tmp, "clone", originBare, bobRoot)
	git(bobRoot, "config", "user.name", "Bob")
	git(bobRoot, "config", "user.email", "bob@example.invalid")
	git(bobRoot, "remote", "add", "fork", bobForkBare)
	git(bobRoot, "remote", "set-url", "--push", "origin", filepath.Join(tmp, "no-upstream-push.git"))

	if out, err := runCmd(RootOptions{RepoRoot: bobRoot, Backend: "github", trackerOverride: bobTracker}, "--fork", "issue", "claim", "43", "--agent", "mock", "--json"); err != nil {
		t.Fatalf("bob takeover claim 43 failed: %v\n%s", err, out)
	}
	raw43Before, _, err := upstreamStore.Load(ctx, 43)
	if err != nil {
		t.Fatal(err)
	}
	var st43Before lease.IssueState
	_ = json.Unmarshal(raw43Before, &st43Before)
	if st43Before.Lease.Owner != "bob" || st43Before.ForkRepo != "bob/swarm" {
		t.Fatalf("expected bob to hold #43 after takeover, got %+v", st43Before)
	}

	// Alice's stale renew and submit on #43 must fail and leave upstream #43 state untouched
	if _, err := runMutation("renew", "rejected", RootOptions{RepoRoot: aliceRoot, Backend: "github", trackerOverride: aliceTracker}, "--fork", "issue", "renew", "43", "--json"); err == nil {
		t.Fatal("expected alice stale renew on #43 to fail after bob takeover")
	}
	if _, err := runCmd(RootOptions{RepoRoot: aliceRoot, Backend: "github", trackerOverride: aliceTracker}, "--fork", "issue", "submit", "43"); err == nil {
		t.Fatal("expected alice stale submit on #43 to fail after bob takeover")
	}
	raw43After, _, err := upstreamStore.Load(ctx, 43)
	if err != nil {
		t.Fatal(err)
	}
	var st43After lease.IssueState
	_ = json.Unmarshal(raw43After, &st43After)
	if st43After.Lease.Owner != "bob" || st43After.Lease.ClaimID != st43Before.Lease.ClaimID || st43After.ForkRepo != "bob/swarm" {
		t.Fatalf("alice stale operations modified #43 state after takeover: %+v", st43After)
	}
	if out, err := runMutation("release", "accepted", RootOptions{RepoRoot: bobRoot, Backend: "github", trackerOverride: bobTracker}, "--fork", "issue", "release", "43", "--json"); err != nil {
		t.Fatalf("JSON release: %v %s", err, out)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestInitDefaultsToInstalledReleaseForCIAndForkCoordinator(t *testing.T) {
	origVersion, origCommit := Version, Commit
	defer func() { Version, Commit = origVersion, origCommit }()
	Commit = "none"

	runCmd := func(root string, args ...string) error {
		var buf bytes.Buffer
		cmd := NewRootCmd()
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs(append([]string{"--root", root, "--backend", "local"}, args...))
		return cmd.Execute()
	}

	root := t.TempDir()
	if out, err := gitfixture.Command(t, "", "-C", root, "init", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	Version = "0.6.0"
	if err := runCmd(root, "repo", "init", "--fork-coordinator"); err != nil {
		t.Fatalf("expected repo init --fork-coordinator on release v0.6.0 to succeed, got: %v", err)
	}
	for _, rel := range []string{".github/workflows/swarm-validate.yml", ".github/workflows/swarm-fork-coordinator.yml"} {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !strings.Contains(string(raw), "SWARM_VERSION: v0.6.0") {
			t.Fatalf("expected %s to pin v0.6.0, got:\n%s", rel, string(raw))
		}
	}

	for _, version := range []string{"0.5.0", "0.6.0-dev", "0.6.0+dirty", "snapshot"} {
		t.Run(version, func(t *testing.T) {
			Version = version
			devRoot := t.TempDir()
			if out, err := gitfixture.Command(t, "", "-C", devRoot, "init", "-b", "main").CombinedOutput(); err != nil {
				t.Fatalf("git init: %v (%s)", err, out)
			}
			if err := runCmd(devRoot, "repo", "init", "--fork-coordinator"); err == nil {
				t.Fatal("expected non-release build without --swarm-version to fail")
			}
			if err := runCmd(devRoot, "repo", "init"); err != nil {
				t.Fatalf("ordinary init with %s: %v", version, err)
			}
			if _, err := os.Stat(filepath.Join(devRoot, ".swarm", "config.yaml")); err != nil {
				t.Fatalf("ordinary init omitted the harness: %v", err)
			}
			if _, err := os.Stat(filepath.Join(devRoot, ".github", "workflows", "swarm-validate.yml")); !os.IsNotExist(err) {
				t.Fatalf("ordinary init automatically selected incompatible pin %s: %v", version, err)
			}
		})
	}
	Version = "dev"
	for _, version := range []string{"v0.5.0", "v0.6.0", "v0.6.0-rc.1", "v1.0.0", strings.Repeat("a", 40)} {
		t.Run(version, func(t *testing.T) {
			pinRoot := t.TempDir()
			if out, err := gitfixture.Command(t, "", "-C", pinRoot, "init", "-b", "main").CombinedOutput(); err != nil {
				t.Fatalf("git init: %v (%s)", err, out)
			}
			if err := runCmd(pinRoot, "repo", "init", "--swarm-version", version); (err == nil) == (version == "v0.5.0") {
				t.Fatalf("unexpected compatibility result for %s: %v", version, err)
			}
			if version == "v0.5.0" {
				if _, err := os.Stat(filepath.Join(pinRoot, ".swarm")); !os.IsNotExist(err) {
					t.Fatalf("incompatible pin wrote scaffolding: %v", err)
				}
			}
		})
	}
}

func TestCLIStatusProvenance(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	number := f.create(t, "agent")
	n := strconv.Itoa(number)
	f.must(t, "issue", "approve", n)
	f.must(t, "issue", "work", n, "--agent", "mock", "--headless")
	before := f.state(t, number)
	roles := []string{"planner", "reviewer", "developer", "tester"}
	if len(before.Attestations) != len(roles) {
		t.Fatalf("expected four records: %+v", before.Attestations)
	}
	out := f.must(t, "issue", "status", n)
	previous := -1
	for i, record := range before.Attestations {
		if record.Role != roles[i] {
			t.Fatalf("unexpected role order: %+v", before.Attestations)
		}
		digest, err := hex.DecodeString(record.ArtifactDigest)
		if err != nil || len(digest) != 32 {
			t.Fatalf("invalid digest: %q", record.ArtifactDigest)
		}
		marker := "  " + record.Role + ":"
		pos := strings.Index(out, marker)
		if pos <= previous {
			t.Fatalf("missing or unordered %s record: %s", record.Role, out)
		}
		previous = pos
		row := strings.SplitN(out[pos:], "\n", 2)[0]
		fields := []struct{ label, value string }{
			{"Agent", record.Agent}, {"Harness", record.Harness}, {"Version", record.HarnessVersion},
			{"Model", record.Model}, {"Sandbox boundary", record.Sandbox},
			{"Artifact type", record.ArtifactType}, {"Artifact SHA-256", record.ArtifactDigest},
		}
		for _, field := range fields {
			if field.value == "" || !strings.Contains(row, field.label+": "+field.value) {
				t.Errorf("missing %s for %s: %s", field.label, record.Role, row)
			}
		}
	}
	for _, detail := range []string{"#" + n, "Base: " + before.BaseSHA, "Task: " + before.TaskDigest, "Approved by: " + before.ApprovedBy} {
		if !strings.Contains(out, detail) {
			t.Errorf("missing detail %q: %s", detail, out)
		}
	}
	for _, artifact := range before.Artifacts {
		if !strings.Contains(out, fmt.Sprintf("  %s: %s (%s)", artifact.Type, artifact.Verdict, artifact.Producer)) {
			t.Errorf("missing artifact: %+v", artifact)
		}
	}
	if before.PRNumber != 0 && !strings.Contains(out, fmt.Sprintf("PR #%d: %s", before.PRNumber, before.PRURL)) {
		t.Errorf("missing PR: %s", out)
	}
	for _, args := range [][]string{{"issue", "status", n, "--json"}, {"issue", "status", "--json"}} {
		var result struct {
			Kind   string `json:"kind"`
			Issues []struct {
				Repository string                     `json:"repository"`
				Issue      *tracker.Issue             `json:"issue"`
				State      *lease.IssueState          `json:"state"`
				Provenance []contract.RoleAttestation `json:"provenance"`
				RoleRuns   []map[string]any           `json:"role_runs"`
			} `json:"issues"`
		}
		raw := f.must(t, args...)
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			t.Fatalf("invalid JSON: %v: %s", err, raw)
		}
		if result.Kind != "status" || len(result.Issues) != 1 {
			t.Fatalf("unexpected envelope: %+v", result)
		}
		entry := result.Issues[0]
		if entry.Repository == "" || entry.Issue == nil || entry.Issue.Number != number || !reflect.DeepEqual(entry.State, before) || !reflect.DeepEqual(entry.Provenance, before.Attestations) {
			t.Fatalf("unexpected status: %+v", entry)
		}
		if len(entry.RoleRuns) != len(before.RoleRuns) || len(entry.RoleRuns) != len(roles) {
			t.Fatalf("role runs = %+v", entry.RoleRuns)
		}
		for i, run := range entry.RoleRuns {
			// Unavailable values are explicit JSON nulls; measured values are numbers.
			if run["role"] != roles[i] || run["run_id"] != before.RoleRuns[i].RunID || run["effort"] != nil || run["input_tokens"] != float64(1200) || run["cached_input_tokens"] != float64(0) {
				t.Fatalf("role run JSON = %+v", run)
			}
			if value, ok := run["model_ms"]; !ok || value != nil {
				t.Fatalf("unavailable model time must be null: %+v", run)
			}
		}
	}
	if !strings.Contains(out, "Role runs:\n"+lease.RoleRunTableHeader) {
		t.Fatalf("missing role run table: %s", out)
	}
	for _, run := range before.RoleRuns {
		if !strings.Contains(out, "| "+run.Role+" | "+run.RunID[:8]+" | ") || !strings.Contains(out, "| 1200/300/0 | 0/0 | 0/0 | mock/unavailable |") {
			t.Fatalf("missing %s run row: %s", run.Role, out)
		}
	}
	if listing := f.must(t, "issue", "status"); strings.Contains(listing, "Provenance:") || strings.Contains(listing, "Artifact SHA-256:") || strings.Contains(listing, "Role runs:") {
		t.Fatalf("listing contains details: %s", listing)
	}
	if !reflect.DeepEqual(before, f.state(t, number)) {
		t.Fatal("status mutated authoritative state")
	}
}

func TestCLIStatusProvenanceMissing(t *testing.T) {
	t.Parallel()
	for _, approved := range []bool{false, true} {
		t.Run(fmt.Sprintf("approved=%t", approved), func(t *testing.T) {
			f := newCLIFixture(t)
			number := f.create(t, "agent")
			n := strconv.Itoa(number)
			var before *lease.IssueState
			if approved {
				f.must(t, "issue", "approve", n)
				before = f.state(t, number)
			}
			out := f.must(t, "issue", "status", n)
			want := "draft/unapproved:"
			if approved {
				want = "Provenance: none recorded"
				if !strings.Contains(out, "Role runs: none recorded") {
					t.Fatalf("missing empty role runs: %s", out)
				}
			}
			if !strings.Contains(out, want) {
				t.Fatalf("missing %q: %s", want, out)
			}
			for _, args := range [][]string{{"issue", "status", n, "--json"}, {"issue", "status", "--json"}} {
				var result struct {
					Issues []struct {
						Repository string                     `json:"repository"`
						Issue      *tracker.Issue             `json:"issue"`
						State      *lease.IssueState          `json:"state"`
						Provenance []contract.RoleAttestation `json:"provenance"`
					} `json:"issues"`
				}
				raw := f.must(t, args...)
				if err := json.Unmarshal([]byte(raw), &result); err != nil {
					t.Fatalf("invalid JSON: %v: %s", err, raw)
				}
				if len(result.Issues) != 1 {
					t.Fatalf("unexpected entries: %s", raw)
				}
				entry := result.Issues[0]
				if entry.Provenance == nil || len(entry.Provenance) != 0 || !reflect.DeepEqual(entry.State, before) {
					t.Fatalf("expected empty array and original state: %s", raw)
				}
			}
			if approved {
				if !reflect.DeepEqual(before, f.state(t, number)) {
					t.Fatal("status mutated state")
				}
			} else if _, err := f.manager.GetCurrentState(context.Background(), number); !errors.Is(err, coordination.ErrNotFound) {
				t.Fatalf("status created state: %v", err)
			}
		})
	}
}

func TestInitPinsBuildsAndUpgradesUntouchedFiles(t *testing.T) {
	origVersion, origCommit := Version, Commit
	defer func() { Version, Commit = origVersion, origCommit }()
	run := func(root string, args ...string) (string, error) {
		var buf bytes.Buffer
		cmd := NewRootCmd()
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs(append([]string{"--root", root, "--backend", "local"}, args...))
		err := cmd.Execute()
		return buf.String(), err
	}
	read := func(root, rel string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	root := t.TempDir()
	if out, err := gitfixture.Command(t, "", "-C", root, "init", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	// A clean development build pins its exact commit, with no flag required.
	buildCommit := strings.Repeat("b", 40)
	Version, Commit = "dev", buildCommit
	if out, err := run(root, "repo", "init", "--fork-coordinator"); err != nil || !strings.Contains(out, "push that commit") {
		t.Fatalf("dev build init: %v\n%s", err, out)
	}
	coordinator := ".github/workflows/swarm-fork-coordinator.yml"
	if !strings.Contains(read(root, coordinator), "SWARM_VERSION: "+buildCommit) {
		t.Fatal("coordinator did not pin the build commit")
	}

	// A source build Go stamps with a pseudo-version pins it, with the same reminder.
	pseudoRoot := t.TempDir()
	if out, err := gitfixture.Command(t, "", "-C", pseudoRoot, "init", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	Version = "0.6.1-0.20261023222858-9ad1652a4325"
	if out, err := run(pseudoRoot, "repo", "init"); err != nil || !strings.Contains(out, "push that commit") || !strings.Contains(read(pseudoRoot, ".github/workflows/swarm-validate.yml"), "SWARM_VERSION: v0.6.1-0.20261023222858-9ad1652a4325") {
		t.Fatalf("pseudo-version init: %v\n%s", err, out)
	}
	Version = "dev"

	// A dirty build keeps the repository's existing pin instead of dropping CI.
	Version, Commit = "0.6.0+dirty", "none"
	if _, err := run(root, "repo", "init"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(root, ".github/workflows/swarm-validate.yml"), "SWARM_VERSION: "+buildCommit) {
		t.Fatal("dirty build dropped the existing workflow pin")
	}

	// Customize one generated file, then upgrade to a release: untouched files
	// (including the coordinator, without --fork-coordinator) move to the new pin,
	// and the customized file is kept.
	customized := filepath.Join(root, ".swarm", "goals.md")
	if err := os.WriteFile(customized, []byte("# Our goals\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	Version = "0.6.1"
	out, err := run(root, "repo", "init")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(root, coordinator), "SWARM_VERSION: v0.6.1") || !strings.Contains(read(root, ".github/workflows/swarm-validate.yml"), "SWARM_VERSION: v0.6.1") {
		t.Fatal("upgrade did not move untouched workflows to the new release")
	}
	if read(root, ".swarm/goals.md") != "# Our goals\n" || !strings.Contains(out, "kept your edits to .swarm/goals.md") {
		t.Fatalf("upgrade overwrote or failed to report a customized file:\n%s", out)
	}

	// Installs that predate the manifest are recognized by their pinned template.
	if err := os.Remove(filepath.Join(root, ".swarm", "managed.json")); err != nil {
		t.Fatal(err)
	}
	Version = "0.6.2"
	if _, err := run(root, "repo", "init"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(root, coordinator), "SWARM_VERSION: v0.6.2") {
		t.Fatal("legacy coordinator workflow was not upgraded")
	}
	if read(root, ".swarm/goals.md") != "# Our goals\n" {
		t.Fatal("legacy upgrade overwrote a customized file")
	}
	if _, err := run(root, "repo", "init", "--force"); err != nil || read(root, ".swarm/goals.md") == "# Our goals\n" {
		t.Fatalf("--force did not replace the customized file: %v", err)
	}
}

func TestRepoOnboardAndInitDryRun(t *testing.T) {
	origVersion, origCommit := Version, Commit
	defer func() { Version, Commit = origVersion, origCommit }()
	Version, Commit = "0.6.1", "none"
	run := func(root string, args ...string) (string, error) {
		var buf bytes.Buffer
		cmd := NewRootCmd()
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs(append([]string{"--root", root, "--backend", "local"}, args...))
		err := cmd.Execute()
		return buf.String(), err
	}
	root := t.TempDir()
	if out, err := gitfixture.Command(t, "", "-C", root, "init", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	assertOnlyGit := func(step string) {
		t.Helper()
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != ".git" {
			t.Fatalf("%s wrote files: %v", step, entries)
		}
	}

	// The guide is available before anything is scaffolded.
	out, err := run(root, "repo", "onboard")
	if err != nil || !strings.Contains(out, "swarm repo init --dry-run --json") || strings.HasPrefix(out, "---") {
		t.Fatalf("repo onboard: %v\n%s", err, out)
	}
	assertOnlyGit("repo onboard")

	if _, err := run(root, "repo", "init", "--dry-run", "--sync-labels"); err == nil {
		t.Fatal("--dry-run --sync-labels should be rejected")
	}
	out, err = run(root, "repo", "init", "--dry-run", "--verbose")
	if err != nil || !strings.Contains(out, "nothing written") || !strings.Contains(out, "would create .swarm/config.yaml") {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	assertOnlyGit("repo init --dry-run")

	if _, err := run(root, "repo", "init"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".swarm", "goals.md"), []byte("# Our goals\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = run(root, "repo", "init", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("dry run json: %v\n%s", err, out)
	}
	var report struct {
		Status     string            `json:"status"`
		Customized []string          `json:"customized"`
		Diffs      map[string]string `json:"diffs"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if report.Status != "dry-run" || !slices.Equal(report.Customized, []string{".swarm/goals.md"}) || !strings.Contains(report.Diffs[".swarm/goals.md"], "-# Our goals") {
		t.Fatalf("dry run report: %+v", report)
	}
}

func TestFreshClaimBaseCommands(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"claim", "work", "next", "node", "stale next"} {
		t.Run(mode, func(t *testing.T) {
			f := newCLIFixture(t)
			refused := 0
			if mode == "next" || mode == "node" || mode == "stale next" {
				refused = f.create(t, "human")
				f.must(t, "issue", "approve", strconv.Itoa(refused))
				if mode == "stale next" {
					issue, err := f.tracker.GetIssue(context.Background(), refused)
					if err != nil {
						t.Fatal(err)
					}
					body := strings.Replace(issue.Body, "Exercise the lifecycle", "Changed objective", 1)
					if _, err = f.tracker.UpdateIssueBody(context.Background(), refused, body); err != nil {
						t.Fatal(err)
					}
				}
			}
			n := f.create(t, "agent")
			f.must(t, "issue", "approve", strconv.Itoa(n))
			old := f.state(t, n).BaseSHA
			cmdGit := gitfixture.Command(t, "", "-C", f.root, "commit", "--allow-empty", "-m", "advance")
			if out, err := cmdGit.CombinedOutput(); err != nil {
				t.Fatalf("%v %s", err, out)
			}
			cmd := f.command(nil)
			var out, diagnostics bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&diagnostics)
			args := []string{"issue", "claim", strconv.Itoa(n), "--agent", "mock", "--json"}
			switch mode {
			case "work":
				args = []string{"issue", "work", strconv.Itoa(n), "--agent", "mock", "--json", "--progress=false"}
			case "next", "stale next":
				args = []string{"issue", "work", "--next", "--agent", "mock", "--json", "--progress=false"}
			case "node":
				args = []string{"node", "run", "--max-jobs", "1", "--agent", "mock"}
			}
			cmd.SetArgs(append([]string{"--root", f.root, "--backend", "local"}, args...))
			if err := cmd.Execute(); err != nil {
				t.Fatalf("%v: %s %s", err, out.String(), diagnostics.String())
			}
			got := f.state(t, n)
			if got.BaseSHA == old || got.PreviousBaseSHA != old {
				t.Fatal("did not repin")
			}
			if !strings.Contains(diagnostics.String(), old+" -> "+got.BaseSHA) {
				t.Fatalf("missing transition: %s", diagnostics.String())
			}
			if mode == "work" || mode == "next" || mode == "stale next" {
				issue, err := f.tracker.GetIssue(context.Background(), n)
				if err != nil {
					t.Fatal(err)
				}
				wu, err := contract.ParseWorkUnitFromIssue(n, issue.Title, issue.Body)
				if err != nil {
					t.Fatal(err)
				}
				if err = lease.VerifyApproval(got, wu); err != nil {
					t.Fatal(err)
				}
				for _, a := range got.Artifacts {
					if a.Type == contract.ArtifactReview && a.Metadata["base_sha"] != got.BaseSHA {
						t.Fatal("review used old base")
					}
				}
				f.must(t, "issue", "submit", strconv.Itoa(n))
			}
			if mode != "node" && !json.Valid(out.Bytes()) {
				t.Fatalf("invalid JSON %s", out.String())
			}
			if refused != 0 {
				if !strings.Contains(diagnostics.String(), fmt.Sprintf("swarm issue approve %d", refused)) {
					t.Fatalf("missing refusal: %s", diagnostics.String())
				}
				if f.state(t, refused).Lease.Status == "active" {
					t.Fatal("claimed refused issue")
				}
				lock, err := lease.LockClaim(f.root, refused)
				if err != nil {
					t.Fatal(err)
				}
				lease.UnlockClaim(lock)
			}
		})
	}
}

func TestCLIQueueSkipsResumedStaleApproval(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"work", "node"} {
		t.Run(mode, func(t *testing.T) {
			f := newCLIFixture(t)
			ctx := context.Background()
			stale := f.create(t, "agent")
			n := strconv.Itoa(stale)
			f.must(t, "issue", "approve", n)
			f.must(t, "issue", "work", n, "--agent", "mock", "--headless")
			f.must(t, "issue", "release", n)
			before := f.state(t, stale)
			if before.Branch == "" || before.HeadSHA == "" || before.PRNumber != 0 || before.Lease.Status == "active" {
				t.Fatalf("expected released checkpoint without a PR: %+v", before)
			}
			issue, err := f.tracker.GetIssue(ctx, stale)
			if err != nil {
				t.Fatal(err)
			}
			body := strings.Replace(issue.Body, "Exercise the lifecycle", "Changed objective", 1)
			issue, err = f.tracker.UpdateIssueBody(ctx, stale, body)
			if err != nil {
				t.Fatal(err)
			}
			wu, err := contract.ParseWorkUnitFromIssue(stale, issue.Title, issue.Body)
			if err != nil {
				t.Fatal(err)
			}
			if before.TaskDigest == contract.DigestWorkUnit(wu) {
				t.Fatal("edited task must have stale approval")
			}
			ready := f.create(t, "agent")
			f.must(t, "issue", "approve", strconv.Itoa(ready))
			issues, err := f.tracker.ListIssues(ctx, tracker.IssueFilter{State: "open", Labels: []string{"swarm:ready"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(issues) != 2 || issues[0].Number != stale || issues[1].Number != ready {
				t.Fatalf("expected stale issue before ready issue: %+v", issues)
			}
			args := []string{"issue", "work", "--next", "--agent", "mock", "--headless"}
			if mode == "node" {
				args = []string{"node", "run", "--max-jobs", "1", "--agent", "mock"}
			}
			f.must(t, args...)
			got := f.state(t, ready)
			if got.Branch == "" || got.HeadSHA == "" || len(got.Artifacts) == 0 {
				t.Fatalf("valid issue was not processed: %+v", got)
			}
			if after := f.state(t, stale); !reflect.DeepEqual(before, after) {
				t.Fatalf("stale issue state changed: before=%+v after=%+v", before, after)
			}
			lock, err := lease.LockClaim(f.root, stale)
			if err != nil {
				t.Fatalf("stale claim lock was not released: %v", err)
			}
			lease.UnlockClaim(lock)
		})
	}
}

// snapshotCLIFixture restores the entire repository at its original absolute
// path, including linked worktrees, Git common metadata, tracker and CAS files.
// Callers must run rows serially and stop all commands before restoring. The
// fixture environment is deliberately left to the caller, as are external files.
func snapshotCLIFixture(t *testing.T, f *cliFixture) func(*testing.T) {
	t.Helper()
	saved := filepath.Join(t.TempDir(), "repository")
	copyFixtureTree(t, saved, f.root)
	refs := prGit(t, f.root, "show-ref")
	worktrees := prGit(t, f.root, "worktree", "list", "--porcelain")
	return func(t *testing.T) {
		t.Helper()
		if err := os.RemoveAll(f.root); err != nil {
			t.Fatal(err)
		}
		copyFixtureTree(t, f.root, saved)
		if prGit(t, f.root, "show-ref") != refs || prGit(t, f.root, "worktree", "list", "--porcelain") != worktrees {
			t.Fatal("fixture restore changed Git refs or linked worktrees")
		}
	}
}

func copyFixtureTree(t *testing.T, dst, src string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if entry.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unexpected fixture file type: %s", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0111 != 0 {
			return testutil.WriteExecutable(target, data, info.Mode().Perm())
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}
