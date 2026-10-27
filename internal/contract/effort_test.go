package contract

import (
	"slices"
	"strings"
	"testing"
)

func TestValidateAgentEffort(t *testing.T) {
	cases := []struct {
		name, agent string
		ac          AgentConfig
		wantErr     string
	}{
		{name: "empty keeps default", agent: "omp", ac: AgentConfig{}},
		{name: "claude high", agent: "claude", ac: AgentConfig{Effort: "high"}},
		{name: "claude max", agent: "claude", ac: AgentConfig{Effort: "max"}},
		{name: "codex minimal", agent: "codex", ac: AgentConfig{Effort: "minimal"}},
		{name: "harness overrides name", agent: "dot", ac: AgentConfig{Harness: "codex", Effort: "low"}},
		{name: "harness is normalized", agent: "planner", ac: AgentConfig{Harness: " Claude ", Effort: "medium"}},
		{name: "claude minimal", agent: "claude", ac: AgentConfig{Effort: "minimal"}, wantErr: `agent "claude": effort "minimal" must be one of low, medium, high, max`},
		{name: "codex max", agent: "codex", ac: AgentConfig{Effort: "max"}, wantErr: `agent "codex": effort "max" must be one of minimal, low, medium, high`},
		{name: "case sensitive", agent: "claude", ac: AgentConfig{Effort: "HIGH"}, wantErr: `effort "HIGH" must be one of`},
		{name: "untrimmed", agent: "codex", ac: AgentConfig{Effort: " high "}, wantErr: `effort " high " must be one of`},
		{name: "omp unsupported", agent: "omp", ac: AgentConfig{Effort: "high"}, wantErr: `agent "omp": effort is supported only for claude and codex harnesses, not "omp"`},
		{name: "opencode unsupported", agent: "oc", ac: AgentConfig{Harness: "opencode", Effort: "high"}, wantErr: `not "opencode"`},
		{name: "mock unsupported", agent: "mock", ac: AgentConfig{Effort: "low"}, wantErr: `not "mock"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAgentEffort(tc.agent, tc.ac)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("rejected valid effort: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestEffortValuesAreFixedPerHarness(t *testing.T) {
	if got := EffortValues("claude"); !slices.Equal(got, []string{"low", "medium", "high", "max"}) {
		t.Fatalf("claude = %v", got)
	}
	if got := EffortValues("codex"); !slices.Equal(got, []string{"minimal", "low", "medium", "high"}) {
		t.Fatalf("codex = %v", got)
	}
	for _, driver := range []string{"omp", "opencode", "mock", ""} {
		if got := EffortValues(driver); got != nil {
			t.Fatalf("%q = %v", driver, got)
		}
	}
	EffortValues("claude")[0] = "turbo"
	if EffortValues("claude")[0] != "low" {
		t.Fatal("caller mutated the allowlist")
	}
}

func TestRoleAttestationRecordsEffortOnlyWhenSet(t *testing.T) {
	if m := (RoleAttestation{Model: "m"}).Metadata(nil); m["model"] != "m" {
		t.Fatalf("model missing: %v", m)
	} else if _, ok := m["effort"]; ok {
		t.Fatalf("empty effort recorded: %v", m)
	}
	if m := (RoleAttestation{Model: "m", Effort: "high"}).Metadata(nil); m["effort"] != "high" {
		t.Fatalf("effort missing: %v", m)
	}
}
