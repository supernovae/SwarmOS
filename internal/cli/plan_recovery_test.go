package cli

import (
	"bytes"
	"context"
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

	"github.com/supernovae/swarmos/internal/lease"
)

func TestPlanReviewRecovery(t *testing.T) {
	st := &lease.IssueState{PendingPlanReview: &lease.PlanRejection{}, NextPhaseReason: "CHANGES_REQUESTED"}
	st.Lease.IssueNumber = 12
	opts := &RootOptions{Backend: "github", Repo: "owner/repo", RepoRoot: "a'b", Fork: true, ForkRemote: "fork"}
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
				cmd := f.command(&RootOptions{RepoRoot: f.root, Backend: backend, Repo: "octo/swarm", trackerOverride: tr})
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
