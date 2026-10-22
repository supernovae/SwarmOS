package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
)

func TestDoctorSelectedFailuresAndOptionalHarnesses(t *testing.T) {
	for _, mode := range []string{"isolated", "trusted-local"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("SWARM_EXECUTION_MODE", mode)
			cfg := &contract.SwarmConfig{DefaultAgent: "codex", Roles: map[string]string{"tester": "limited"}, Agents: map[string]contract.AgentConfig{
				"codex": {}, "limited": {Roles: []string{"developer"}}, "claude": {},
			}}
			checks := doctorAgentChecks(cfg, false, func(name string, _ *contract.SwarmConfig, _ bool, role string) (string, error) {
				if name == "codex" {
					return "verified", nil
				}
				return "", fmt.Errorf("unavailable for %s", role)
			})
			statuses := map[string]string{}
			for _, c := range checks {
				statuses[c.Name] = c.Status
			}
			for name, want := range map[string]string{"codex/planner": "OK", "limited/tester": "FAIL", "limited/developer": "OPTIONAL", "claude/reviewer": "OPTIONAL"} {
				if statuses[name] != want {
					t.Errorf("%s = %s; want %s", name, statuses[name], want)
				}
			}
		})
	}
}

func TestDoctorRejectsUnavailableSelectedAgent(t *testing.T) {
	t.Setenv("SWARM_EXECUTION_MODE", "trusted-local")
	cfg := &contract.SwarmConfig{DefaultAgent: "codex", Agents: map[string]contract.AgentConfig{"codex": {Binary: "/no-such-swarmos-test-binary"}}}
	checks := doctorAgentChecks(cfg, false, agent.Preflight)
	if len(checks) != 4 {
		t.Fatalf("checks: %+v", checks)
	}
	for _, c := range checks {
		if c.Status != "FAIL" || !strings.Contains(c.Detail, "unavailable") {
			t.Fatalf("missed selected failure: %+v", c)
		}
	}
}

func TestDoctorRejectsExcludedSelectedRole(t *testing.T) {
	t.Setenv("SWARM_EXECUTION_MODE", "trusted-local")
	cfg := &contract.SwarmConfig{DefaultAgent: "limited", Agents: map[string]contract.AgentConfig{
		"limited": {Binary: "git", Harness: "codex", Roles: []string{"developer"}},
	}}
	checks := doctorAgentChecks(cfg, false, agent.Preflight)
	if len(checks) != 4 {
		t.Fatalf("checks: %+v", checks)
	}
	for _, c := range checks {
		if c.Name == "limited/developer" {
			if c.Status != "OK" {
				t.Fatalf("allowed role: %+v", c)
			}
		} else if c.Status != "FAIL" || !strings.Contains(c.Detail, "not permitted for role") {
			t.Fatalf("excluded selected role: %+v", c)
		}
	}
}
