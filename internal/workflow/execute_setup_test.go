package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"github.com/supernovae/swarmos/internal/tracker"
)

func TestExecuteWorkSetupCleanup(t *testing.T) {
	for _, name := range []string{"fresh", "resumed", "later"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			tmp := t.TempDir()
			if out, err := gitfixture.Command(t, "", "init", "-b", "main", tmp).CombinedOutput(); err != nil {
				t.Fatalf("git init failed: %v (%s)", err, string(out))
			}
			if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: tmp}); err != nil {
				t.Fatal(err)
			}
			configureValidation(t, tmp)
			if out, err := gitfixture.Command(t, "", "-C", tmp, "add", ".").CombinedOutput(); err != nil {
				t.Fatalf("git add harness failed: %v (%s)", err, string(out))
			}
			if out, err := gitfixture.Command(t, "", "-C", tmp, "-c", "user.name=SwarmOS", "-c", "user.email=swarm@example.test", "commit", "-m", "initialize harness").CombinedOutput(); err != nil {
				t.Fatalf("git commit harness failed: %v (%s)", err, string(out))
			}

			lt := tracker.NewLocalTracker(tmp).WithUser("local-dev", "OWNER")
			wu := &contract.WorkUnit{
				Objective:   "Add version command",
				Constraints: []string{"Stdlib only"},
				Acceptance:  []string{"Outputs JSON"},
				Workflow:    "feature",
				Gate:        contract.GateAgent,
			}
			iss, err := lt.CreateIssue(ctx, "Add version command", contract.FormatIssueMarkdown(wu), []string{"swarm:ready"})
			if err != nil {
				t.Fatal(err)
			}

			eng, err := NewEngine(tmp, lt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := eng.LeaseManager.ApproveTask(ctx, iss.Number, "local-dev"); err != nil {
				t.Fatal(err)
			}

			cause := errors.New("setup sentinel")
			opts := WorkOptions{RepoRoot: tmp, IssueNumber: iss.Number, Owner: "local-dev", AgentName: "mock", Phase: "all", Headless: true}
			var claimID string
			if name == "resumed" {
				st, err := eng.LeaseManager.Acquire(ctx, iss.Number, opts.Owner, "mock", "all", time.Hour, 3)
				if err != nil {
					t.Fatal(err)
				}
				claimID = st.Lease.ClaimID
				opts.ClaimID = claimID
			}
			opts.OnClaim = func(st *lease.IssueState) error {
				if claimID == "" {
					claimID = st.Lease.ClaimID
				}
				if name != "later" {
					return cause
				}
				return nil
			}
			t.Setenv("SWARM_MOCK_VERDICT", "CHANGES_REQUESTED")
			result, err := eng.ExecuteWork(ctx, opts)
			if name == "later" {
				if result == nil || !errors.Is(err, ErrPlanReviewRejected) {
					t.Fatalf("later failure: %v, %v", result, err)
				}
			} else {
				if result != nil || !errors.Is(err, cause) || !strings.Contains(err.Error(), "persist claim token") {
					t.Fatalf("setup failure: %v, %v", result, err)
				}
			}
			st, err := eng.LeaseManager.GetCurrentState(ctx, iss.Number)
			if err != nil {
				t.Fatal(err)
			}
			if name == "fresh" {
				if st.Lease.Status != "released" {
					t.Fatalf("fresh lease status: %q", st.Lease.Status)
				}
				if err := eng.LeaseManager.CheckClaim(ctx, iss.Number, opts.Owner); err == nil {
					t.Fatalf("fresh claim not released: %+v", st)
				}
			} else {
				if st.Lease.Status != "active" || st.Lease.ClaimID != claimID || st.Lease.Owner != opts.Owner {
					t.Fatalf("claim changed: %+v", st.Lease)
				}
				if err := eng.LeaseManager.CheckClaim(ctx, iss.Number, opts.Owner); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
