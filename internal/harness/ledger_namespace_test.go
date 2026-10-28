package harness

import (
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
)

const ledgerTestConfig = `version: 1
default_agent: codex
default_gate: agent
lease:
  duration: 4h
  renewable: true
  max_renewals: 3
security:
  require_ready_label: true
  ready_label: swarm:ready
agents:
  codex:
    binary: codex
`

func TestDecodeConfigLedgerNamespace(t *testing.T) {
	for _, tc := range []struct {
		name, extra, effective string
		valid                  bool
	}{
		{"omitted keeps branch refs", "", contract.LedgerNamespaceBranch, true},
		{"explicit branch", "ledger_namespace: branch\n", contract.LedgerNamespaceBranch, true},
		{"hidden", "ledger_namespace: hidden\n", contract.LedgerNamespaceHidden, true},
		{"unknown mode", "ledger_namespace: public\n", "", false},
		{"wrong case", "ledger_namespace: Hidden\n", "", false},
		{"list type", "ledger_namespace: [hidden]\n", "", false},
		{"map type", "ledger_namespace:\n  mode: hidden\n", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := decodeConfig([]byte(ledgerTestConfig + tc.extra))
			if !tc.valid {
				if err == nil {
					t.Fatalf("accepted %q", tc.extra)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.EffectiveLedgerNamespace(); got != tc.effective {
				t.Fatalf("effective namespace %q, want %q", got, tc.effective)
			}
		})
	}
}

func TestInitWritesHiddenLedgerNamespaceOnlyForNewConfig(t *testing.T) {
	root := t.TempDir()
	if _, err := InitRepository(InitOptions{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LedgerNamespace != contract.LedgerNamespaceHidden {
		t.Fatalf("new repository config selects %q, want hidden", cfg.LedgerNamespace)
	}
	existing := t.TempDir()
	if err := os.MkdirAll(filepath.Join(existing, ".swarm"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(existing, ".swarm", "config.yaml"), []byte(ledgerTestConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InitRepository(InitOptions{RepoRoot: existing}); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(existing)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EffectiveLedgerNamespace() != contract.LedgerNamespaceBranch {
		t.Fatalf("init changed an existing repository to %q", cfg.LedgerNamespace)
	}
}

func TestLedgerNamespaceAtTrustedCommit(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := gitfixture.Command(t, "", append([]string{"-C", root}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main")
	git("config", "user.name", "Harness Test")
	git("config", "user.email", "test@example.invalid")
	commit := func(config string) string {
		t.Helper()
		path := filepath.Join(root, ".swarm", "config.yaml")
		if config == "" {
			git("commit", "--allow-empty", "-m", "no config")
		} else {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			git("add", ".")
			git("commit", "-m", "config")
		}
		return git("rev-parse", "HEAD")
	}
	noConfig := commit("")
	omitted := commit(ledgerTestConfig)
	hidden := commit(ledgerTestConfig + "ledger_namespace: hidden\n")
	malformed := commit(ledgerTestConfig + "ledger_namespace: sideways\n")
	for _, tc := range []struct {
		name, sha, want string
	}{
		{"commit without config", noConfig, contract.LedgerNamespaceBranch},
		{"config without key", omitted, contract.LedgerNamespaceBranch},
		{"hidden", hidden, contract.LedgerNamespaceHidden},
		{"malformed", malformed, ""},
		{"mutable name", "HEAD", ""},
		{"unknown commit", strings.Repeat("a", 40), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := LedgerNamespaceAt(root, tc.sha)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("selected %q, want an error", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("LedgerNamespaceAt = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
