package fork

import (
	"errors"
	"testing"

	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/lease"
)

func TestPlanReplanLimitCoordinatorError(t *testing.T) {
	err := mapCoordinatorError("update-phase", "automatic plan retry limit reached (2/2): run swarm issue review-plan 1")
	if !errors.Is(err, lease.ErrPlanReplanLimit) {
		t.Fatalf("cap lost across transport: %v", err)
	}
}

// Every sentinel must survive the transport, including ones whose message has
// capitals: the coordinator text is lowercased before matching.
func TestCoordinatorErrorSentinelsSurviveTransport(t *testing.T) {
	for _, sentinel := range []error{
		lease.ErrPlanReplanLimit, lease.ErrClaimFence, lease.ErrPlanApprovalRequired, lease.ErrApprovalRequired,
		lease.ErrAlreadyClaimed, lease.ErrClaimRaceLost, lease.ErrMaxRenewals,
		coordination.ErrForkClaimLimit, coordination.ErrReplay,
	} {
		if err := mapCoordinatorError("op", "issue #7: "+sentinel.Error()+" (detail)"); !errors.Is(err, sentinel) {
			t.Errorf("%q lost across transport: %v", sentinel, err)
		}
	}
	if err := mapCoordinatorError("op", "something else"); errors.Is(err, lease.ErrClaimFence) || err == nil {
		t.Fatal(err)
	}
}
