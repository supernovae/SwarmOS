package workflow

import (
	"errors"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/lease"
)

// issue work claims through WorkOptions.LeaseDuration; the lease is capped at
// the approved policy duration (the scaffold default of 4h).
func TestWorkLeaseDurationCappedAtApprovedPolicy(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requested time.Duration
		want      time.Duration
	}{
		{"default", 0, 4 * time.Hour},
		{"shorter", 30 * time.Minute, 30 * time.Minute},
		{"longer", 100 * time.Hour, 4 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, issue := budgetEngine(t)
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			e.LeaseManager.SetNowFunc(func() time.Time { return now })
			stop := errors.New("stop after claim")
			var claimed *lease.IssueState
			_, err := e.ExecuteWork(t.Context(), WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue.Number, Owner: "maintainer", AgentName: "mock", Phase: "plan", Headless: true, LeaseDuration: tc.requested,
				OnClaim: func(*lease.IssueState) error {
					var loadErr error
					claimed, loadErr = e.LeaseManager.GetCurrentState(t.Context(), issue.Number)
					if loadErr != nil {
						return loadErr
					}
					return stop
				}})
			if !errors.Is(err, stop) {
				t.Fatalf("work = %v", err)
			}
			if claimed == nil || claimed.Lease.Status != "active" {
				t.Fatalf("authoritative lease at claim = %+v", claimed)
			}
			if got := claimed.Lease.ExpiresAt.Sub(now); got != tc.want {
				t.Fatalf("work lease = %s, want %s", got, tc.want)
			}
		})
	}
}
