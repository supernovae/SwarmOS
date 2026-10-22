package agent

import (
	"os"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
)

func TestAgentEnvironmentHelper(t *testing.T) {
	if os.Getenv("SWARM_AGENT_ENV_HELPER") != "1" {
		return
	}
	if os.Getenv("GITHUB_TOKEN") != "" {
		os.Exit(23)
	}
	if os.Getenv("MODEL_API_KEY") != "model-secret" {
		os.Exit(24)
	}
	os.Exit(0)
}

func TestAgentModelHelper(t *testing.T) {
	if os.Getenv("SWARM_AGENT_MODEL_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--model" && i+1 < len(os.Args) && os.Args[i+1] == "approved-model" {
			os.Exit(0)
		}
	}
	os.Exit(25)
}

func TestConfiguredRunnerRejectsUnapprovedOrUnavailableSelections(t *testing.T) {
	cfg := &contract.SwarmConfig{Execution: contract.ExecutionConfig{Mode: contract.ExecutionTrustedLocal}, Agents: map[string]contract.AgentConfig{
		"claude": {Binary: os.Args[0], Roles: []string{"planner"}, Model: "approved-model"},
	}}
	if _, err := NewConfiguredRunner("codex", cfg, false); err == nil {
		t.Fatal("unallowlisted adapter was accepted")
	}
	if _, err := Preflight("claude", cfg, false, "reviewer"); err == nil {
		t.Fatal("disallowed role was accepted")
	}
	if _, err := Preflight("claude", cfg, false, "planner"); err != nil {
		t.Fatalf("configured available planner rejected: %v", err)
	}
	if _, err := NewConfiguredRunner("mock", cfg, false); err == nil {
		t.Fatal("mock was accepted outside local backend")
	}
	if _, err := NewConfiguredRunner("mock", cfg, true); err != nil {
		t.Fatalf("local mock rejected: %v", err)
	}
}

func TestConfiguredRunnerRejectsMissingBinaryBeforeExecution(t *testing.T) {
	cfg := &contract.SwarmConfig{Execution: contract.ExecutionConfig{Mode: contract.ExecutionTrustedLocal}, Agents: map[string]contract.AgentConfig{"claude": {Binary: "definitely-not-a-swarm-agent"}}}
	if _, err := NewConfiguredRunner("claude", cfg, false); err == nil {
		t.Fatal("unavailable configured executable was accepted")
	}
}

func TestConfiguredRunnerRejectsPublishingCredentialForwarding(t *testing.T) {
	cfg := &contract.SwarmConfig{Execution: contract.ExecutionConfig{Mode: contract.ExecutionTrustedLocal}, Agents: map[string]contract.AgentConfig{"claude": {Binary: os.Args[0], Env: []string{"GITHUB_TOKEN"}}}}
	if _, err := NewConfiguredRunner("claude", cfg, false); err == nil {
		t.Fatal("publishing token was accepted as agent authentication")
	}
}

func TestRestrictedEnvRemovesPublishingCredentials(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "publish-secret")
	t.Setenv("SSH_AUTH_SOCK", "/host/agent.sock")
	t.Setenv("MODEL_API_KEY", "model-secret")
	env := restrictedEnv([]string{"MODEL_API_KEY"}, t.TempDir())
	seen := map[string]bool{}
	for _, entry := range env {
		seen[entry] = true
	}
	if seen["GITHUB_TOKEN=publish-secret"] {
		t.Fatal("publishing credential reached agent environment")
	}
	if !seen["MODEL_API_KEY=model-secret"] {
		t.Fatal("explicit model credential did not reach agent environment")
	}
	if seen["SSH_AUTH_SOCK=/host/agent.sock"] {
		t.Fatal("host SSH credential socket reached agent environment")
	}
}

func TestAdapterProcessHidesPublishingCredentials(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "publish-secret")
	t.Setenv("SWARM_AGENT_ENV_HELPER", "1")
	t.Setenv("MODEL_API_KEY", "model-secret")
	runner := &CLIAdapter{agentName: "custom", binary: os.Args[0], prefixArgs: []string{"-test.run=TestAgentEnvironmentHelper", "--"}, envAllow: []string{"MODEL_API_KEY", "SWARM_AGENT_ENV_HELPER"}}
	if _, err := runner.Run(t.Context(), RunRequest{Role: "planner", WorktreePath: t.TempDir()}); err != nil {
		t.Fatalf("agent inherited prohibited credentials or lost explicit model auth: %v", err)
	}
}

func TestConfiguredModelReachesSupportedHarness(t *testing.T) {
	t.Setenv("SWARM_AGENT_MODEL_HELPER", "1")
	cfg := &contract.SwarmConfig{Execution: contract.ExecutionConfig{Mode: contract.ExecutionTrustedLocal}, Agents: map[string]contract.AgentConfig{
		"reviewer": {Binary: os.Args[0], Harness: "claude", Args: []string{"-test.run=TestAgentModelHelper", "--"}, Model: "approved-model"},
	}}
	runner, err := NewConfiguredRunner("reviewer", cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(t.Context(), RunRequest{Role: "reviewer", WorktreePath: t.TempDir(), Mode: "headless"}); err != nil {
		t.Fatalf("configured model did not reach supported harness: %v", err)
	}
}

func TestOpenCodeV2CommandsPreserveOptionalModel(t *testing.T) {
	for _, mode := range []string{"headless", "interactive"} {
		for _, model := range []string{"", "project-provider/project-model"} {
			adapter := &CLIAdapter{driver: "opencode", binary: "opencode"}
			_, args := adapter.BuildCommand(RunRequest{Mode: mode, Model: model, TaskPrompt: "task"})
			want := "run"
			if mode == "interactive" {
				want = "mini"
			}
			if len(args) < 3 || args[0] != want || args[1] != "--standalone" {
				t.Fatal(args)
			}
			found := false
			for i, arg := range args {
				if arg == "--model" {
					found = true
					if i == 0 || i+1 >= len(args) || args[i+1] != model {
						t.Fatal(args)
					}
				}
			}
			if found != (model != "") {
				t.Fatal("model selection changed", args)
			}
		}
	}
}
