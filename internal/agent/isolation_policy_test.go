package agent

import (
	"os"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
)

func TestDefaultModeNeverRunsHostAgent(t *testing.T) {
	t.Setenv("SWARM_EXECUTION_MODE", "")
	cfg := &contract.SwarmConfig{Agents: map[string]contract.AgentConfig{"claude": {Binary: os.Args[0]}}}
	_, err := NewConfiguredRunner("claude", cfg, false)
	if err == nil || !strings.Contains(err.Error(), "no verified isolated adapter") {
		t.Fatalf("default isolation did not fail closed: %v", err)
	}
}

func TestOperatorCannotWeakenApprovedIsolation(t *testing.T) {
	t.Setenv("SWARM_EXECUTION_MODE", contract.ExecutionTrustedLocal)
	cfg := &contract.SwarmConfig{Security: contract.SecurityConfig{RequireIsolation: true}, Agents: map[string]contract.AgentConfig{"claude": {Binary: os.Args[0]}}}
	if _, err := NewConfiguredRunner("claude", cfg, false); err == nil {
		t.Fatal("local override weakened repository isolation")
	}
}

func TestMockCannotBypassIsolation(t *testing.T) {
	t.Setenv("SWARM_EXECUTION_MODE", "")
	for _, required := range []bool{false, true} {
		cfg := &contract.SwarmConfig{Security: contract.SecurityConfig{RequireIsolation: required}, Agents: map[string]contract.AgentConfig{"mock": {Binary: "mock"}}}
		if _, err := NewConfiguredRunner("mock", cfg, true); err == nil {
			t.Fatal("host mock accepted in isolated mode")
		}
	}
}

func TestIsolatedRunnerRejectsHostCredentialsAndUnrestrictedNetwork(t *testing.T) {
	t.Setenv("SWARM_EXECUTION_MODE", "")
	for _, ac := range []contract.AgentConfig{
		{Binary: "claude", Authentication: "none", Env: []string{"MODEL_API_KEY"}},
		{Binary: "claude", Authentication: "none", ReviewNetwork: true},
		{Binary: "claude", Authentication: "none", ContainerRuntime: "./docker"},
	} {
		cfg := &contract.SwarmConfig{Execution: contract.ExecutionConfig{Backend: contract.BackendOCI}, Agents: map[string]contract.AgentConfig{"claude": ac}}
		if _, err := NewConfiguredRunner("claude", cfg, false); err == nil {
			t.Fatal("accepted unsafe isolated capabilities")
		}
	}
}

func TestNativeCodexRejectsPolicyOverrides(t *testing.T) {
	t.Setenv("SWARM_EXECUTION_MODE", "")
	for _, ac := range []contract.AgentConfig{
		{Binary: os.Args[0], Args: []string{"--yolo"}},
		{Binary: os.Args[0], Env: []string{"OPENAI_API_KEY"}},
		{Binary: os.Args[0], ReviewNetwork: true},
		{Binary: os.Args[0], Authentication: "none"},
		{Binary: os.Args[0], Sandbox: "container"},
	} {
		cfg := &contract.SwarmConfig{Agents: map[string]contract.AgentConfig{"codex": ac}}
		if _, err := NewConfiguredRunner("codex", cfg, false); err == nil {
			t.Fatalf("accepted native override %+v", ac)
		}
	}
}

func TestIsolatedBackendIsExplicit(t *testing.T) {
	t.Setenv("SWARM_EXECUTION_MODE", "")
	cfg := &contract.SwarmConfig{Agents: map[string]contract.AgentConfig{"codex": {Binary: os.Args[0]}}}
	r, err := NewConfiguredRunner("codex", cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Provenance("developer", false).Sandbox; got != "codex-native" {
		t.Fatalf("default selected %q", got)
	}
	cfg.Execution.Backend = contract.BackendOCI
	if _, err := NewConfiguredRunner("codex", cfg, false); err == nil {
		t.Fatal("OCI silently reused host authentication")
	}
	ac := cfg.Agents["codex"]
	ac.Authentication = "none"
	cfg.Agents["codex"] = ac
	r, err = NewConfiguredRunner("codex", cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Provenance("developer", false).Sandbox; got != "container" {
		t.Fatalf("explicit OCI selected %q", got)
	}
}
