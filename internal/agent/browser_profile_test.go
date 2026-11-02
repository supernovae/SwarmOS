package agent

import (
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/sandbox"
	"runtime"
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
