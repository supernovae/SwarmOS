package agent

import (
	"encoding/json"
	"fmt"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/sandbox"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBrowserProfileConfiguredPropagation(t *testing.T) {
	for _, harness := range []string{"codex", "claude", "opencode", "omp"} {
		for _, enabled := range []bool{false, true} {
			cfg := &contract.SwarmConfig{DefaultAgent: harness, Execution: contract.ExecutionConfig{BrowserProfile: enabled}, Agents: map[string]contract.AgentConfig{harness: {Binary: "git", Harness: harness}}}
			runner, err := NewConfiguredRunnerWithOptions(harness, cfg, false, RunnerOptions{LookupEnv: func(string) (string, bool) { return "", false }})
			if err != nil {
				t.Fatal(err)
			}
			adapter := runner.(*configuredRunner).Runner.(*CLIAdapter)
			if adapter.executionPolicy.BrowserProfile != enabled {
				t.Fatalf("%s lost approved profile", harness)
			}
		}
	}
}

func TestCodexBrowserAdapter(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		cfg := &contract.SwarmConfig{DefaultAgent: "codex", Execution: contract.ExecutionConfig{BrowserProfile: enabled}, Agents: map[string]contract.AgentConfig{"codex": {Binary: "git", Harness: "codex"}}}
		runner, err := NewConfiguredRunnerWithOptions("codex", cfg, false, RunnerOptions{LookupEnv: func(string) (string, bool) { return "", false }})
		if err != nil {
			t.Fatal(err)
		}
		adapter := runner.(*configuredRunner).Runner.(*CLIAdapter)
		for _, role := range []string{"developer", "tester", "planner", "reviewer"} {
			got := sandbox.CodexBrowserEligible(adapter.executionPolicy.BrowserProfile, role, role == "planner" || role == "reviewer")
			want := runtime.GOOS == "linux" && enabled && (role == "developer" || role == "tester")
			if got != want {
				t.Fatal("adapter browser eligibility", role, enabled)
			}
		}
	}
}

func TestToolCapabilityOptionalAssemblyFailure(t *testing.T) {
	cfg := &contract.SwarmConfig{Execution: contract.ExecutionConfig{Mode: contract.ExecutionTrustedLocal}, Agents: map[string]contract.AgentConfig{"mock": {}}}
	opts := RunnerOptions{InspectCapabilityEntry: func(string) error { return fmt.Errorf("TOKEN /home/private-user/private") }}
	_, report, err := InspectWithOptions("mock", cfg, true, "developer", opts, "approved_task")
	if err != nil {
		t.Fatal("informational assembly blocked preflight", err)
	}
	raw, _ := json.Marshal(report)
	if strings.Contains(string(raw), "TOKEN") || strings.Contains(string(raw), "/home/") {
		t.Fatal(string(raw))
	}
	for _, e := range report.Entries {
		if e.Runtime != "not_probed" || e.Usable {
			t.Fatal(e)
		}
	}
}

func TestToolCapabilityMappings(t *testing.T) {
	for _, driver := range []string{"codex", "claude", "opencode", "omp"} {
		for _, role := range []string{"developer", "tester", "planner", "reviewer"} {
			cfg := &contract.SwarmConfig{Agents: map[string]contract.AgentConfig{"worker": {Harness: driver, Binary: "unused", Roles: []string{role}}}}
			r := CapabilityReport("worker", cfg, role, RunnerOptions{}, "approved_task", true)
			if !r.Entries[0].Usable || r.Harness != driver {
				t.Fatal(r)
			}
			cfg.Agents["worker"] = contract.AgentConfig{Harness: driver, Roles: []string{"denied"}}
			r = CapabilityReport("worker", cfg, role, RunnerOptions{}, "approved_task", true)
			if r.Entries[0].Authorized || r.Entries[0].Usable || r.Entries[0].Reason != "role_denied" {
				t.Fatal("role policy broadened", r)
			}
			r = CapabilityReport("candidate-agent", cfg, role, RunnerOptions{}, "approved_task", true)
			if r.Entries[0].Usable {
				t.Fatal("candidate granted capability")
			}
		}
	}
}

func TestToolCapabilityMissingDependencies(t *testing.T) {
	for _, driver := range []string{"codex", "claude", "opencode", "omp"} {
		cfg := &contract.SwarmConfig{Agents: map[string]contract.AgentConfig{"worker": {Harness: driver, Binary: filepath.Join(t.TempDir(), "TOKEN-private-user")}}}
		_, r, err := InspectWithOptions("worker", cfg, false, "developer", RunnerOptions{}, "approved_task")
		if err == nil || r.Entries[0].Usable || r.Entries[0].Runtime == "available" {
			t.Fatal("missing runtime reported usable", err, r)
		}
		if strings.Contains(err.Error(), "TOKEN") || strings.Contains(err.Error(), "/") {
			t.Fatal("raw probe diagnostics escaped", err)
		}
	}
}
