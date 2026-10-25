package contract

import (
	"fmt"
	"time"
)

// AgentLimits is coordinator-owned policy, independent of required-check limits.
type AgentLimits struct {
	Active, Wall time.Duration
	ExcludeIdle  bool
}

func positiveDuration(field, value string) (time.Duration, error) {
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration (got %q)", field, value)
	}
	return d, nil
}
func validateAgentTimeouts(e ExecutionConfig) error {
	if e.AgentTimeout != nil {
		if _, err := positiveDuration("execution.agent_timeout", *e.AgentTimeout); err != nil {
			return err
		}
	}
	for role, value := range e.RoleTimeouts {
		switch role {
		case "planner", "reviewer", "developer", "tester":
		default:
			return fmt.Errorf("execution.role_timeouts: unknown role %q", role)
		}
		if _, err := positiveDuration("execution.role_timeouts."+role, value); err != nil {
			return err
		}
	}
	return nil
}

// ResolveAgentLimits must receive approved-base policy, never candidate config.
// A positive operator limit can only shorten policy.
func ResolveAgentLimits(e ExecutionConfig, role string, interactive bool, operator time.Duration) (AgentLimits, error) {
	if err := validateAgentTimeouts(e); err != nil {
		return AgentLimits{}, err
	}
	d, explicit := time.Hour, false
	if e.AgentTimeout != nil {
		d, _ = time.ParseDuration(*e.AgentTimeout)
		explicit = true
	}
	if value, ok := e.RoleTimeouts[role]; ok {
		d, _ = time.ParseDuration(value)
		explicit = true
	}

	if !interactive {
		if operator > 0 {
			d = min(d, operator)
		}
		return AgentLimits{Wall: d}, nil
	}
	if e.Backend == BackendOCI {
		wall := 4 * time.Hour
		if explicit && d < wall {
			wall = d
		}
		if operator > 0 {
			wall = min(wall, operator)
		}
		return AgentLimits{Wall: wall}, nil
	}
	if operator > 0 {
		d = min(d, operator)
	}
	return AgentLimits{Active: d, Wall: 4 * time.Hour, ExcludeIdle: true}, nil
}

func AgentWarningLead(limit time.Duration) time.Duration {
	return min(2*time.Minute, limit/10)
}
