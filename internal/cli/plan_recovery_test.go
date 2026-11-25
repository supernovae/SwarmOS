package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/lease"
)

func TestPlanReviewRecovery(t *testing.T) {
	t.Parallel()
	st := &lease.IssueState{PendingPlanReview: &lease.PlanRejection{}, NextPhaseReason: "CHANGES_REQUESTED"}
	st.Lease.IssueNumber = 12
	opts := &RootOptions{Repo: "owner/repo", RepoRoot: "a'b", Fork: true, ForkRemote: "fork"}
	for _, resource := range []string{"issue", "pr"} {
		text := planReviewRecovery(opts, st, resource, 37)
		want := "issue review-plan 12"
		if resource == "pr" {
			want = "pr work 37 --phase review"
			if strings.Contains(text, "issue review-plan") {
				t.Fatal(text)
			}
		}
		for _, part := range []string{want, resource + " work 37 --phase plan", "--root 'a'\\''b'", "--fork", "per-revision cap"} {
			if !strings.Contains(text, part) {
				t.Fatalf("missing %q: %s", part, text)
			}
		}
	}
	if planReviewRecovery(opts, nil, "pr", 37) != "" || planReviewRecovery(opts, &lease.IssueState{}, "pr", 37) != "" {
		t.Fatal("spurious recovery")
	}
}

func TestCheckpointRecoveryPlanCLISelectors(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"1", "--phase", "all", "--plan-checkpoint"},
		{"1", "--phase", "plan", "--expected-head", strings.Repeat("a", 40)},
		{"1", "--phase", "plan", "--plan-checkpoint", "--handoff", "--expected-head", strings.Repeat("a", 40), "--candidate-head", strings.Repeat("b", 40)},
		{"--next", "--phase", "plan", "--plan-checkpoint", "--expected-head", strings.Repeat("a", 40), "--candidate-head", strings.Repeat("b", 40)},
	} {
		cmd := newWorkCmd(&RootOptions{})
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestCheckpointRecoveryPlanCLIInputs(t *testing.T) {
	t.Setenv("SWARM_EXECUTION_MODE", "trusted-local")
	for _, backend := range []string{"local", "github"} {
		t.Run(backend, func(t *testing.T) {
			f := newCLIFixture(t)
			n := f.create(t, "agent")
			number := strconv.Itoa(n)
			cfg, err := harness.LoadConfig(f.root)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Agents["mock"] = contract.AgentConfig{Binary: "true"}
			raw, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(f.root, ".swarm/config.yaml"), raw, 0644); err != nil {
				t.Fatal(err)
			}
			prGit(t, f.root, "add", ".swarm/config.yaml")
			prGit(t, f.root, "commit", "-m", "allow fixture mock")
			var tr tracker.Tracker = f.tracker
			if backend == "github" {
				issue, err := f.tracker.GetIssue(t.Context(), n)
				if err != nil {
					t.Fatal(err)
				}
				_, tr = newFakeGitHubTracker(t, issue)
				addTestOrigin(t, f)
				f.manager = lease.NewManager(tr, cfg.Security)
				if err = f.manager.ConfigureStore(f.root); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			f.resolverRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) { calls++; return nil, os.ErrNotExist }
			run := func(args ...string) (string, error) {
				cmd := f.command(&RootOptions{RepoRoot: f.root, Repo: "octo/swarm", trackerOverride: tr})
				var out bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(&out)
				cmd.SetArgs(args)
				err := cmd.ExecuteContext(context.Background())
				return out.String(), err
			}
			for _, args := range [][]string{{"issue", "approve", number}, {"issue", "claim", number, "--agent", "mock"}} {
				out, err := run(args...)
				if err != nil {
					t.Fatalf("%v: %v %s", args, err, out)
				}
			}
			st := f.state(t, n)
			path := workspace.WorktreePath(f.root, n)
			f.manager.BindClaim(n, st.Lease.ClaimID)
			branch := prGit(t, path, "branch", "--show-current")
			if _, err := f.manager.RecordCheckpoint(t.Context(), n, "alice", branch, st.BaseSHA); err != nil {
				t.Fatal(err)
			}
			st = f.state(t, n)
			if err := os.WriteFile(filepath.Join(path, "candidate.txt"), []byte("preserved\n"), 0644); err != nil {
				t.Fatal(err)
			}
			prGit(t, path, "add", "candidate.txt")
			prGit(t, path, "commit", "-m", "candidate")
			candidate := prGit(t, path, "rev-parse", "HEAD")
			f.setenv("SWARM_EXECUTION_MODE", "isolated")
			baseArgs := []string{"issue", "work", number, "--phase", "plan", "--plan-checkpoint", "--expected-head", st.HeadSHA, "--candidate-head", candidate, "--dry-run"}
			before := checkpointSnapshot(t, f.root)
			out, err := run(baseArgs...)
			if err != nil {
				t.Fatalf("preview: %v %s", err, out)
			}
			if !strings.Contains(out, "checkpoint-plan-dry-run") || calls != 0 {
				t.Fatalf("preview ran agent: %d %s", calls, out)
			}
			if after := checkpointSnapshot(t, f.root); !reflect.DeepEqual(before, after) {
				t.Fatal(snapshotDiff(before, after))
			}
			for _, bad := range []string{"short", strings.Repeat("z", 40), st.HeadSHA, strings.Repeat("0", 40)} {
				args := append([]string(nil), baseArgs...)
				args[len(args)-2] = bad
				if _, err = run(args...); err == nil {
					t.Fatalf("accepted candidate %q", bad)
				}
				if after := checkpointSnapshot(t, f.root); !reflect.DeepEqual(before, after) {
					t.Fatal(snapshotDiff(before, after))
				}
			}
			if err = os.WriteFile(filepath.Join(path, "candidate.txt"), []byte("dirty"), 0644); err != nil {
				t.Fatal(err)
			}
			dirty := checkpointSnapshot(t, f.root)
			if _, err = run(baseArgs...); err == nil {
				t.Fatal("accepted dirty candidate")
			}
			if after := checkpointSnapshot(t, f.root); !reflect.DeepEqual(dirty, after) {
				t.Fatal(snapshotDiff(dirty, after))
			}
			if calls != 0 {
				t.Fatal("preview/refusal invoked runner")
			}
		})
	}
}

func TestPlanRetryRecoveryCLI(t *testing.T) {
	id := strings.Repeat("a", 64)
	for _, tc := range []struct {
		flags []string
		fork  bool
	}{
		{flags: []string{"1"}},
		{flags: []string{"1", "--phase", "plan", "--dry-run"}},
		{flags: []string{"1", "--phase", "plan", "--handoff"}},
		{flags: []string{"1", "--phase", "plan", "--plan-checkpoint"}},
		{flags: []string{"1", "--phase", "plan", "--candidate-head", "head"}},
		{flags: []string{"--next", "--phase", "plan"}},
		{flags: []string{"--next", "--all", "--phase", "plan"}},
		{flags: []string{"1", "--phase", "plan"}, fork: true},
	} {
		cmd := newWorkCmd(&RootOptions{Fork: tc.fork})
		cmd.SetArgs(append(tc.flags, "--recover-plan-retry", id))
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--recover-plan-retry requires") {
			t.Fatalf("accepted conflicting selector %v: %v", tc.flags, err)
		}
	}

	f := newCLIFixture(t)
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)
	n := f.create(t, "agent")
	ctx := t.Context()
	issue, err := f.tracker.GetIssue(ctx, n)
	if err != nil {
		t.Fatal(err)
	}
	_, tr := newFakeGitHubTracker(t, issue)
	addTestOrigin(t, f)
	m := lease.NewManager(tr, contract.SecurityConfig{})
	if err := m.ConfigureStore(f.root); err != nil {
		t.Fatal(err)
	}
	const owner = "alice"
	if _, err := m.ApproveTask(ctx, n, owner); err != nil {
		t.Fatal(err)
	}
	st, err := m.Acquire(ctx, n, owner, "mock", "plan", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.SaveClaimToken(f.root, n, st.Lease.ClaimID, st.BaseSHA); err != nil {
		t.Fatal(err)
	}
	path, _, err := workspace.ProvisionAt(f.root, n, issue.Title, st.BaseSHA, "", "")
	if err != nil {
		t.Fatal(err)
	}
	plan := contract.Artifact{Type: contract.ArtifactDesign, Producer: "mock:planner", Summary: "plan", Body: "initial plan"}
	test := contract.Artifact{Type: contract.ArtifactTestPlan, Producer: "mock:planner", Summary: "test plan", Body: "initial tests"}
	st, err = m.RecordPlanPair(ctx, n, owner, plan, test)
	if err != nil {
		t.Fatal(err)
	}
	meta := lease.ReviewBindings(st)
	meta["kind"] = "agent"
	_, err = m.UpdateArtifact(ctx, n, owner, contract.Artifact{
		Type: contract.ArtifactReview, Producer: "mock:reviewer", Summary: "review", Verdict: "CHANGES_REQUESTED",
		Body: "VERDICT: CHANGES_REQUESTED\n\nRevise the plan", Metadata: meta,
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err = m.UpdatePhase(ctx, n, owner, "plan", "active")
	if err != nil {
		t.Fatal(err)
	}
	plan.Body, test.Body = "revised plan", "revised tests"
	outcome := lease.PlanRetryOutcome{Reservation: *st.PlanRetryReservation, Owner: owner, Plan: plan, Test: test}
	active := filepath.Join(path, ".swarm", "active")
	if err := os.MkdirAll(active, 0700); err != nil {
		t.Fatal(err)
	}
	for i, file := range []string{"PLAN.md", "TEST_PLAN.md"} {
		body := []byte([]string{plan.Body, test.Body}[i])
		if err := os.WriteFile(filepath.Join(active, file), body, 0600); err != nil {
			t.Fatal(err)
		}
		outcome.FileDigests[i] = fmt.Sprintf("%x", sha256.Sum256(body))
	}
	digest, err := outcome.Digest()
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(f.root)
	if err != nil {
		t.Fatal(err)
	}
	checkout := workspace.WorktreePath(canonical, n)
	data, err := json.Marshal(struct {
		Checkout, Digest string
		Outcome          lease.PlanRetryOutcome
	}{checkout, digest, outcome})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.SavePlanRetryRecovery(checkout, tr.BackendName()+"\n"+canonical, n, outcome.Reservation.ID, data); err != nil {
		t.Fatal(err)
	}
	runnerCalls := 0
	f.resolverRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
		runnerCalls++
		return nil, os.ErrNotExist
	}
	run := func(args ...string) (string, error) {
		t.Helper()
		cmd := f.command(&RootOptions{RepoRoot: f.root, Repo: "octo/swarm", trackerOverride: tr})
		var output, diagnostics bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(&diagnostics)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(ctx)
		return output.String(), err
	}
	output, err := run("issue", "work", strconv.Itoa(n), "--phase", "plan", "--json", "--headless")
	var stopped struct {
		NextAction NextAction `json:"next_action"`
	}
	if err == nil || json.Unmarshal([]byte(output), &stopped) != nil ||
		stopped.NextAction.Reason != ReasonPlanRetryRecovery || len(stopped.NextAction.Argv) == 0 {
		t.Fatalf("missing JSON recovery action: %v %s", err, output)
	}
	rendered := stopped.NextAction.Argv
	if last := rendered[len(rendered)-1]; last != outcome.Reservation.ID {
		t.Fatalf("rendered command lost the retry ID: %v", rendered)
	}
	output, err = run(append(rendered[1:], "--json")...)
	if err != nil || !strings.Contains(output, "plan-retry-recovery") || runnerCalls != 0 {
		t.Fatalf("rendered command did not recover without a planner: %v %s, calls %d", err, output, runnerCalls)
	}
	settled, err := m.GetCurrentState(ctx, n)
	if err != nil || settled.PlanReplanCount != 1 || !lease.HasRevisedPlan(settled) {
		t.Fatalf("retry outcome missing from upstream: %v", err)
	}
}
