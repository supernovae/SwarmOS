package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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

// TestAgentLookupEnvironmentHelper is gated by argv, so a missing injected
// environment fails instead of skipping the checks.
func TestAgentLookupEnvironmentHelper(t *testing.T) {
	if !slices.Contains(os.Args, "--swarm-lookup-helper") {
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
	t.Setenv("SWARM_GITHUB_TOKEN_COMMAND", "op read op://vault/github/token")
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
	if seen["SWARM_GITHUB_TOKEN_COMMAND=op read op://vault/github/token"] {
		t.Fatal("coordinator credential command reached agent environment")
	}
}

// mapLookup is an injected environment holding exactly vars.
func mapLookup(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := vars[name]
		return value, ok
	}
}

func TestRestrictedEnvWithLookupKeepsAllowlist(t *testing.T) {
	// Conflicting process values must not reach the agent when a lookup is injected.
	t.Setenv("MODEL_API_KEY", "process-secret")
	t.Setenv("PATH", "/process/bin")
	home := t.TempDir()
	lookup := mapLookup(map[string]string{
		"PATH": "/injected/bin", "MODEL_API_KEY": "model-secret", "EMPTY_MODEL_KEY": "",
		"GITHUB_TOKEN": "publish-secret", "GH_TOKEN": "publish-secret", "SWARM_GITHUB_TOKEN_COMMAND": "op read op://vault/github/token",
		"SSH_AUTH_SOCK": "/host/agent.sock", "GIT_ASKPASS": "/host/askpass", "HOME": "/host/home", "XDG_CONFIG_HOME": "/host/config",
		"UNLISTED": "value", "SWARM_MOCK_DEVELOPER_CMD": "exit 1",
	})
	env := restrictedEnvWithLookup([]string{"MODEL_API_KEY", "EMPTY_MODEL_KEY", "UNSET_MODEL_KEY"}, home, lookup)
	got := map[string]string{}
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		got[name] = value
	}
	for name, want := range map[string]string{"PATH": "/injected/bin", "MODEL_API_KEY": "model-secret", "EMPTY_MODEL_KEY": "", "HOME": home, "XDG_CONFIG_HOME": filepath.Join(home, ".config")} {
		if value, ok := got[name]; !ok || value != want {
			t.Errorf("%s = %q (present %v), want %q", name, value, ok, want)
		}
	}
	for _, name := range []string{"UNSET_MODEL_KEY", "GITHUB_TOKEN", "GH_TOKEN", "SWARM_GITHUB_TOKEN_COMMAND", "SSH_AUTH_SOCK", "GIT_ASKPASS", "UNLISTED", "SWARM_MOCK_DEVELOPER_CMD"} {
		if value, ok := got[name]; ok {
			t.Errorf("%s=%q reached the agent environment", name, value)
		}
	}
}

func TestConfiguredRunnerWithLookupRejectsCredentialNames(t *testing.T) {
	lookup := mapLookup(map[string]string{"SWARM_EXECUTION_MODE": contract.ExecutionTrustedLocal})
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN", "SWARM_GITHUB_TOKEN_COMMAND", "GIT_ASKPASS", "SSH_AUTH_SOCK", "HOME", "XDG_CONFIG_HOME"} {
		t.Run(name, func(t *testing.T) {
			cfg := &contract.SwarmConfig{Agents: map[string]contract.AgentConfig{"claude": {Binary: os.Args[0], Env: []string{name}}}}
			lookup := func(key string) (string, bool) {
				if key == name {
					return "host-credential", true
				}
				return lookup(key)
			}
			if _, err := NewConfiguredRunnerWithOptions("claude", cfg, false, RunnerOptions{LookupEnv: lookup}); err == nil || !strings.Contains(err.Error(), "credential") {
				t.Fatalf("%s accepted as agent authentication: %v", name, err)
			}
			if _, err := PreflightWithOptions("claude", cfg, false, "planner", RunnerOptions{LookupEnv: lookup}); err == nil || !strings.Contains(err.Error(), "credential") {
				t.Fatalf("%s accepted by preflight: %v", name, err)
			}
		})
	}
}

func TestConfiguredRunnerExecutionModeLookup(t *testing.T) {
	cfg := &contract.SwarmConfig{Agents: map[string]contract.AgentConfig{}}
	t.Setenv("SWARM_EXECUTION_MODE", "process-mode")
	if _, err := NewConfiguredRunner("mock", cfg, true); err == nil || !strings.Contains(err.Error(), `"process-mode"`) {
		t.Fatalf("default runner did not read the process environment: %v", err)
	}
	trusted := RunnerOptions{LookupEnv: mapLookup(map[string]string{"SWARM_EXECUTION_MODE": contract.ExecutionTrustedLocal})}
	if _, err := NewConfiguredRunnerWithOptions("mock", cfg, true, trusted); err != nil {
		t.Fatalf("injected trusted-local mode was ignored: %v", err)
	}
	isolated := RunnerOptions{LookupEnv: mapLookup(map[string]string{"SWARM_EXECUTION_MODE": contract.ExecutionIsolated})}
	if _, err := NewConfiguredRunnerWithOptions("mock", cfg, true, isolated); err == nil || !strings.Contains(err.Error(), "trusted-local") {
		t.Fatalf("injected isolated mode admitted the host mock: %v", err)
	}
	if _, err := NewConfiguredRunnerWithOptions("mock", cfg, true, RunnerOptions{LookupEnv: mapLookup(nil)}); err == nil || !strings.Contains(err.Error(), "trusted-local") {
		t.Fatalf("unset injected mode did not default to isolated: %v", err)
	}
}

func TestAdapterProcessUsesLookupWithoutPublishingCredentials(t *testing.T) {
	lookup := mapLookup(map[string]string{
		"PATH": os.Getenv("PATH"), "MODEL_API_KEY": "model-secret", "GITHUB_TOKEN": "publish-secret",
	})
	runner := &CLIAdapter{agentName: "custom", binary: os.Args[0], prefixArgs: []string{"-test.run=^TestAgentLookupEnvironmentHelper$", "--", "--swarm-lookup-helper"}, envAllow: []string{"MODEL_API_KEY"}, lookupEnv: lookup}
	req := RunRequest{Role: "planner", WorktreePath: t.TempDir(), TaskPrompt: "task"}
	if _, args := runner.BuildCommand(req); strings.Contains(strings.Join(args, " "), "secret") {
		t.Fatalf("credential reached argv: %q", args)
	}
	if _, err := runner.Run(t.Context(), req); err != nil {
		t.Fatalf("injected model auth did not reach the agent, or a publishing credential did: %v", err)
	}
}

func TestMockRunnerControlsUseLookup(t *testing.T) {
	worktree := t.TempDir()
	envDump := filepath.Join(t.TempDir(), "env.txt")
	runner := &MockRunner{lookupEnv: mapLookup(map[string]string{
		"SWARM_MOCK_VERDICT":       "CHANGES_REQUESTED",
		"SWARM_MOCK_DEVELOPER_CMD": "env > " + strconv.Quote(envDump),
	})}
	if _, err := runner.Run(t.Context(), RunRequest{Role: "reviewer", WorktreePath: worktree}); err != nil {
		t.Fatal(err)
	}
	review, err := os.ReadFile(filepath.Join(worktree, ".swarm", "active", "REVIEW.md"))
	if err != nil || !strings.Contains(string(review), "VERDICT: CHANGES_REQUESTED") {
		t.Fatalf("injected verdict ignored: %v\n%s", err, review)
	}
	if _, err := runner.Run(t.Context(), RunRequest{Role: "developer", WorktreePath: worktree, TaskPrompt: "implement"}); err != nil {
		t.Fatal(err)
	}
	dump, err := os.ReadFile(envDump)
	if err != nil {
		t.Fatalf("injected developer command did not run: %v", err)
	}
	if strings.Contains(string(dump), "SWARM_MOCK_") {
		t.Fatalf("mock control reached the developer command environment:\n%s", dump)
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
