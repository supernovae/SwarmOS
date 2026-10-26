package pr

import (
	"errors"
	"testing"

	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
)

func TestWorkPreflightStopErrorPrecedence(t *testing.T) {
	r := &Result{IssueNumber: 4, PR: &tracker.PullRequest{Number: 9}}
	for _, tc := range []struct {
		name     string
		w        WorkPreflightResult
		approval bool
		paused   bool
	}{
		{"conflict", WorkPreflightResult{BaseResult: "matching", Blockers: []string{"PR #9 is conflicting"}}, false, false},
		{"moved base needs approval first", WorkPreflightResult{BaseResult: "moved", Blockers: []string{"moved", "conflicting"}}, true, false},
		{"paused rebase finishes first", WorkPreflightResult{BaseResult: "moved", Blockers: []string{"paused"}, pausedRebase: true}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.w.stopError(r)
			var task *lease.TaskApprovalError
			var conflict *ConflictError
			switch {
			case tc.approval:
				if !errors.As(err, &task) || task.IssueNumber != 4 || errors.As(err, &conflict) {
					t.Fatalf("err = %#v", err)
				}
			default:
				if !errors.As(err, &conflict) || conflict.PRNumber != 9 || conflict.Paused != tc.paused || conflict.Manual || errors.As(err, &task) {
					t.Fatalf("err = %#v", err)
				}
			}
			if err.Error() == "" {
				t.Fatal("stop lost its guidance")
			}
		})
	}
}
