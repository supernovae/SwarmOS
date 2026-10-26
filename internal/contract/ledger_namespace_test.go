package contract

import "testing"

func TestLedgerNamespaceValidationAndDefault(t *testing.T) {
	for _, tc := range []struct {
		value, effective string
		valid            bool
	}{
		{"", LedgerNamespaceBranch, true},
		{"branch", LedgerNamespaceBranch, true},
		{"hidden", LedgerNamespaceHidden, true},
		{"Hidden", "", false},
		{"refs/swarm", "", false},
		{" hidden", "", false},
	} {
		err := ValidateLedgerNamespace(tc.value)
		if (err == nil) != tc.valid {
			t.Errorf("ValidateLedgerNamespace(%q) = %v, valid %v", tc.value, err, tc.valid)
		}
		if tc.valid {
			if got := (&SwarmConfig{LedgerNamespace: tc.value}).EffectiveLedgerNamespace(); got != tc.effective {
				t.Errorf("EffectiveLedgerNamespace(%q) = %q, want %q", tc.value, got, tc.effective)
			}
		}
	}
	var missing *SwarmConfig
	if got := missing.EffectiveLedgerNamespace(); got != LedgerNamespaceBranch {
		t.Fatalf("nil config selects %q", got)
	}
}
