package contract

import (
	"fmt"
	"slices"
	"strings"
)

// effortValues is the fixed reasoning effort allowlist of each harness that
// exposes a typed effort setting. Other harnesses reject effort entirely.
var effortValues = map[string][]string{
	"claude": {"low", "medium", "high", "max"},
	"codex":  {"minimal", "low", "medium", "high"},
}

// EffortValues returns the allowed effort values for driver, or nil when the
// harness has no supported effort setting.
func EffortValues(driver string) []string {
	return slices.Clone(effortValues[driver])
}

// ValidateAgentEffort checks an agent's effort against the allowlist of its
// harness, resolved like the configured runner: harness, else the agent name.
// An empty effort keeps the harness default and is always valid.
func ValidateAgentEffort(name string, ac AgentConfig) error {
	if ac.Effort == "" {
		return nil
	}
	driver := strings.ToLower(strings.TrimSpace(ac.Harness))
	if driver == "" {
		driver = strings.ToLower(strings.TrimSpace(name))
	}
	allowed := effortValues[driver]
	if allowed == nil {
		return fmt.Errorf("agent %q: effort is supported only for claude and codex harnesses, not %q", name, driver)
	}
	if !slices.Contains(allowed, ac.Effort) {
		return fmt.Errorf("agent %q: effort %q must be one of %s", name, ac.Effort, strings.Join(allowed, ", "))
	}
	return nil
}
