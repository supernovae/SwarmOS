package contract

import "testing"

func TestRebaseRepairLimitDefaultsAndExplicitZero(t *testing.T) {
	if got := (LeaseConfig{}).RebaseRepairAttempts(); got != 2 {
		t.Fatalf("default %d", got)
	}
	for _, limit := range []int{0, 1, 5} {
		cfg := LeaseConfig{RebaseRepairLimit: &limit}
		if cfg.RebaseRepairAttempts() != limit {
			t.Fatalf("explicit %d lost", limit)
		}
	}
}
