package contract

import (
	"encoding/json"
	"testing"
)

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

func TestValidationRepairLimitDefaultsAndExplicitZero(t *testing.T) {
	if got := (LeaseConfig{}).ValidationRepairAttempts(); got != 1 {
		t.Fatalf("default %d", got)
	}
	rebase := 0
	if got := (LeaseConfig{RebaseRepairLimit: &rebase}).ValidationRepairAttempts(); got != 1 {
		t.Fatalf("rebase limit changed validation repair: %d", got)
	}
	for _, limit := range []int{0, 1, 3} {
		cfg := LeaseConfig{ValidationRepairLimit: &limit}
		if cfg.ValidationRepairAttempts() != limit || cfg.RebaseRepairAttempts() != 2 {
			t.Fatalf("explicit %d lost", limit)
		}
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var decoded LeaseConfig
		if err := json.Unmarshal(data, &decoded); err != nil || decoded.ValidationRepairAttempts() != limit {
			t.Fatalf("JSON round trip %s: %v", data, err)
		}
	}
}
