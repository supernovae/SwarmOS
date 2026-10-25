package fork

import (
	"errors"
	"github.com/supernovae/swarmos/internal/lease"
	"testing"
)

func TestPlanReplanLimitCoordinatorError(t *testing.T) {
	err := mapCoordinatorError("update-phase", "automatic plan retry limit reached (2/2): run swarm issue review-plan 1")
	if !errors.Is(err, lease.ErrPlanReplanLimit) {
		t.Fatalf("cap lost across transport: %v", err)
	}
}
