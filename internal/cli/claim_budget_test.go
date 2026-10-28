package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"github.com/supernovae/swarmos/internal/tracker"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/workflow"
)

func TestClaimBudgetGuidanceRetainsSelection(t *testing.T) {
	for _, resource := range []string{"issue", "pr"} {
		res := &workflow.WorkResult{HandedOffToPhase: "implement", ClaimBudgetPause: &workflow.ClaimBudgetPause{Reason: "paused for claim budget: 1h available, 3 renewals used, 0 remaining"}}
		opts := &RootOptions{Backend: "github", Repo: "owner/repo", RepoRoot: "/path with space", Fork: true, ForkRemote: "contributor"}
		text := claimBudgetGuidance(opts, res, resource, 79)
		for _, want := range []string{"paused for claim budget", "--backend 'github'", "--repo 'owner/repo'", "--root '/path with space'", "--fork --fork-remote 'contributor'", resource + " work 79 --phase implement"} {
			if !strings.Contains(text, want) {
				t.Fatalf("missing %s in %s", want, text)
			}
		}
		raw, err := json.Marshal(res)
		if err != nil || !strings.Contains(string(raw), "continue_command") {
			t.Fatalf("JSON guidance: %s %v", raw, err)
		}
	}
}

func TestCLIStatusClaimBudgetUsesManagerClock(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	issue := f.create(t, "none")
	n := strconv.Itoa(issue)
	f.must(t, "issue", "approve", n)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f.manager.SetNowFunc(func() time.Time { return base })
	st, err := f.manager.Acquire(t.Context(), issue, "alice", "mock", "plan", 4*time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	now := base.Add(time.Hour)
	run := func(args ...string) string {
		t.Helper()
		cmd := f.command(&RootOptions{RepoRoot: f.root, Backend: "local", nowFn: func() time.Time { return now }})
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %s", err, out.String())
		}
		return out.String()
	}
	for _, args := range [][]string{{"issue", "status", n, "--json"}, {"issue", "status", "--json"}} {
		raw := run(args...)
		var result struct {
			Issues []struct {
				ClaimBudget *lease.ClaimBudget `json:"claim_budget"`
			}
		}
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			t.Fatal(err)
		}
		b := result.Issues[0].ClaimBudget
		if b == nil || b.RenewalsUsed != 0 || b.RenewalsRemaining != 1 || b.Remaining != 15*time.Hour || !b.Deadline.Equal(base.Add(16*time.Hour)) {
			t.Fatalf("budget JSON: %s", raw)
		}
	}
	text := run("issue", "status", n)
	for _, want := range []string{"0 renewals used, 1 remaining", "total allowance left=15h0m0s", "current expiry="} {
		if !strings.Contains(text, want) {
			t.Fatalf("%s missing %s", text, want)
		}
	}
	f.manager.BindClaim(issue, st.Lease.ClaimID)
	if _, err = f.manager.Handoff(t.Context(), issue, "alice", "plan", "plan"); err != nil {
		t.Fatal(err)
	}
	if text = run("issue", "status", n); !strings.Contains(text, "available=0s") || !strings.Contains(text, "active=false") {
		t.Fatal(text)
	}
}

type budgetClockTracker struct {
	tracker.Tracker
	manager *lease.Manager
	elapsed *atomic.Int64
}

func (tr *budgetClockTracker) GetIssue(ctx context.Context, number int) (*tracker.Issue, error) {
	issue, err := tr.Tracker.GetIssue(ctx, number)
	if err == nil {
		if st, stateErr := tr.manager.GetCurrentState(ctx, number); stateErr == nil && st != nil && st.Lease.Status == "active" {
			tr.elapsed.Store(int64(3 * time.Hour))
		}
	}
	return issue, err
}

func TestCLIWorkAndNodeBudgetPauseIsSuccessfulAndActionable(t *testing.T) {
	t.Parallel()
	for _, resource := range []string{"issue", "node"} {
		for _, asJSON := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%t", resource, asJSON), func(t *testing.T) {
				f := newCLIFixture(t)
				cfg, err := harness.LoadConfig(f.root)
				if err != nil {
					t.Fatal(err)
				}
				cfg.Lease.MaxRenewals = 0
				raw, err := yaml.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(f.root, ".swarm/config.yaml"), raw, 0600); err != nil {
					t.Fatal(err)
				}
				if out, err := gitfixture.Command(t, "", "-C", f.root, "commit", "-am", "zero renewal policy").CombinedOutput(); err != nil {
					t.Fatalf("%s %v", out, err)
				}
				number := f.create(t, "none")
				n := strconv.Itoa(number)
				f.must(t, "issue", "approve", n)
				base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
				var elapsed atomic.Int64
				tr := &budgetClockTracker{Tracker: f.tracker, manager: f.manager, elapsed: &elapsed}
				cmd := f.command(&RootOptions{RepoRoot: f.root, Backend: "local", trackerOverride: tr, nowFn: func() time.Time { return base.Add(time.Duration(elapsed.Load())) }})
				var out, diag bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(&diag)
				args := []string{"issue", "work", n, "--agent", "mock"}
				if resource == "node" {
					args = []string{"node", "run", "--agent", "mock", "--max-jobs", "1", "--auto-submit"}
				}
				if asJSON {
					args = append(args, "--json")
				}
				cmd.SetArgs(args)
				if err := cmd.Execute(); err != nil {
					t.Fatalf("%v: %s %s", err, out.String(), diag.String())
				}
				combined := out.String() + diag.String()
				for _, want := range []string{"paused for claim budget", "issue work " + n + " --phase plan"} {
					if !strings.Contains(combined, want) {
						t.Fatalf("missing %s in %s", want, combined)
					}
				}
				if asJSON {
					var result map[string]any
					if err := json.Unmarshal(out.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					if resource == "node" && (result["failures"] != float64(0) || result["attempts"] != float64(1)) {
						t.Fatalf("node counted pause as failure: %s", out.String())
					}
					if resource == "issue" && result["status"] != "paused-claim-budget" {
						t.Fatal(out.String())
					}
				}
				prs, err := f.tracker.ListPullRequests(t.Context(), tracker.PullRequestFilter{})
				if err != nil || len(prs) != 0 {
					t.Fatalf("pause auto-submitted: %+v %v", prs, err)
				}
				st := f.state(t, number)
				if st.Lease.Status != "handoff" || len(st.Artifacts) != 0 {
					t.Fatalf("pause mutated verdicts: %+v", st)
				}
			})
		}
	}
}

// TestCLIForkStatusMissingClaimBudget reads state written by an upstream
// coordinator that predates claim budgets.
func TestCLIForkStatusMissingClaimBudget(t *testing.T) {
	t.Parallel()
	f := newUpstreamRemoteFixture(t)
	alice := f.aliceClone("alice")
	opts := f.as("alice", alice)
	if out, err := f.run(opts, "--upstream-remote", "upstream", "--fork", "issue", "claim", "42", "--agent", "mock"); err != nil {
		t.Fatalf("claim: %v\n%s", err, out)
	}
	status := func(args ...string) string {
		t.Helper()
		out, err := f.run(opts, append([]string{"--upstream-remote", "upstream", "--fork", "issue", "status", "42"}, args...)...)
		if err != nil {
			t.Fatalf("status: %v\n%s", err, out)
		}
		return out
	}
	supplied := status()
	if !strings.Contains(supplied, "Claim budget: 0 renewals used, ") || strings.Contains(supplied, "unavailable from upstream coordinator") {
		t.Fatalf("supplied budget output changed:\n%s", supplied)
	}

	store := trustedLedgerStore(t, f.upstreamRoot, "")
	raw, rev, err := store.Load(t.Context(), 42)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err = json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "claim_budget")
	if raw, err = json.Marshal(legacy); err != nil {
		t.Fatal(err)
	}
	if _, err = store.CompareAndSwap(t.Context(), 42, rev, raw); err != nil {
		t.Fatal(err)
	}
	if f.state().ClaimBudget != nil {
		t.Fatal("fixture still serves a claim budget")
	}

	text := status()
	for _, want := range []string{"claim budget: unavailable from upstream coordinator\n", "octo/swarm #42 active: owner=alice", "Base: " + f.baseSHA} {
		if !strings.Contains(text, want) {
			t.Fatalf("status lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Claim budget: ") {
		t.Fatalf("status fabricated a budget:\n%s", text)
	}
	var result struct {
		Issues []struct {
			ClaimBudget            *lease.ClaimBudget `json:"claim_budget"`
			ClaimBudgetUnavailable bool               `json:"claim_budget_unavailable"`
		}
	}
	if err = json.Unmarshal([]byte(status("--json")), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Issues) != 1 || result.Issues[0].ClaimBudget != nil || !result.Issues[0].ClaimBudgetUnavailable {
		t.Fatalf("status JSON: %+v", result)
	}
}
