package contract

import "testing"

func TestPlanReplanAttempts(t *testing.T) {
	if (LeaseConfig{}).PlanReplanAttempts() != 2 {
		t.Fatal("default is not two")
	}
	for _, n := range []int{0, 1, 7} {
		if got := (LeaseConfig{PlanReplanLimit: &n}).PlanReplanAttempts(); got != n {
			t.Fatalf("limit %d: %d", n, got)
		}
	}
}
