package contract

import "fmt"

// Ledger namespaces select where SwarmOS keeps coordination state,
// checkpoint retention, and fork contributor indexes on the upstream remote.
// Task branches are PR heads and stay under refs/heads/ in both modes.
const (
	// LedgerNamespaceBranch keeps ledger refs under refs/heads/ so GitHub
	// rulesets can protect them. It is the default when the key is absent.
	LedgerNamespaceBranch = "branch"
	// LedgerNamespaceHidden keeps ledger refs under refs/swarm/, outside the
	// branch list. Rulesets cannot restrict these refs.
	LedgerNamespaceHidden = "hidden"
)

// ValidateLedgerNamespace accepts an omitted value or one of the two modes.
func ValidateLedgerNamespace(ns string) error {
	switch ns {
	case "", LedgerNamespaceBranch, LedgerNamespaceHidden:
		return nil
	}
	return fmt.Errorf("ledger_namespace must be %s or %s, got %q", LedgerNamespaceBranch, LedgerNamespaceHidden, ns)
}

// EffectiveLedgerNamespace returns the configured mode, branch when omitted.
func (c *SwarmConfig) EffectiveLedgerNamespace() string {
	if c == nil || c.LedgerNamespace == "" {
		return LedgerNamespaceBranch
	}
	return c.LedgerNamespace
}
