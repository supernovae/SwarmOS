package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestExternalWorkUnitRejectsSymlinkEscape(t *testing.T) {
	for _, parent := range []bool{false, true} {
		root, outside := t.TempDir(), t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "unit.yaml"), []byte("objective: Outside contract\nworkflow: feature\nacceptance:\n  - Preserve boundary\n"), 0600); err != nil {
			t.Fatal(err)
		}
		target, name := filepath.Join(outside, "unit.yaml"), "link"
		if parent {
			target = outside
			name = "link/unit.yaml"
		}
		if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
			t.Skip(err)
		}
		if _, err := ParseWorkUnitFromIssueWithRoot(1, "title", "swarm-workunit-path: "+name, root); err == nil {
			t.Fatal("external symlink accepted")
		}
	}
}

func TestCommitMessageConfigPreservesLegacySerializationAndDigest(t *testing.T) {
	cfg := &SwarmConfig{
		Version:      1,
		DefaultAgent: "codex",
		DefaultGate:  GateAgent,
		Lease:        LeaseConfig{Duration: "4h"},
		Security:     SecurityConfig{RequireReadyLabel: true, ReadyLabel: "swarm:ready"},
		Agents:       map[string]AgentConfig{"codex": {Binary: "codex"}},
	}
	// Match the pre-extension JSON representation rather than pinning an entire
	// serialized snapshot that would obscure unrelated contract changes.
	legacy := struct {
		Execution    ExecutionConfig        `json:"execution,omitempty"`
		Version      int                    `json:"version"`
		DefaultAgent string                 `json:"default_agent"`
		DefaultGate  GateMode               `json:"default_gate"`
		PlanMode     string                 `json:"plan_mode,omitempty"`
		Roles        map[string]string      `json:"roles,omitempty"`
		Lease        LeaseConfig            `json:"lease"`
		Security     SecurityConfig         `json:"security"`
		Agents       map[string]AgentConfig `json:"agents"`
		Fork         *ForkConfig            `json:"fork,omitempty"`
	}{
		Execution: cfg.Execution, Version: cfg.Version, DefaultAgent: cfg.DefaultAgent,
		DefaultGate: cfg.DefaultGate, PlanMode: cfg.PlanMode, Roles: cfg.Roles,
		Lease: cfg.Lease, Security: cfg.Security, Agents: cfg.Agents, Fork: cfg.Fork,
	}
	got, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("omitted commit_message altered legacy JSON: got %s, want %s", got, want)
	}
	sum := sha256.Sum256(want)
	if digest := DigestConfig(cfg); digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("legacy digest changed: got %s, want %x", digest, sum)
	}
	if got := cfg.CommitMessageDetail(); got != "concise" || cfg.CommitMessage != nil {
		t.Fatalf("effective default mutated legacy config: %q, %+v", got, cfg.CommitMessage)
	}
	if got := (*SwarmConfig)(nil).CommitMessageDetail(); got != "concise" {
		t.Fatalf("nil config effective detail = %q", got)
	}
}
