package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestConfigRejectsInvalidSecurityPolicy(t *testing.T) {
	valid, err := embeddedTemplates.ReadFile("templates/swarm/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"syntax":             "security: [",
		"unknown field":      string(valid) + "require_reddy_label: false\n",
		"second document":    string(valid) + "---\nsecurity: {}\n",
		"readiness disabled": strings.Replace(string(valid), "require_ready_label: true", "require_ready_label: false", 1),
		"unknown gate":       strings.Replace(string(valid), "default_gate: agent", "default_gate: typo", 1),
		"nonpositive lease":  strings.Replace(string(valid), "duration: 4h", "duration: 0s", 1),
		"unapproved default": strings.Replace(string(valid), "default_agent: claude", "default_agent: unapproved", 1),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, err := decodeConfig([]byte(data))
			if err == nil || cfg != nil {
				t.Fatalf("invalid policy accepted: cfg=%v err=%v", cfg, err)
			}
		})
	}
	cfg, err := LoadConfig(t.TempDir())
	if err == nil || cfg != nil {
		t.Fatalf("missing config received defaults: cfg=%v err=%v", cfg, err)
	}
}

func TestValidationPolicyCannotSilentlyDisableChecks(t *testing.T) {
	for name, data := range map[string]string{
		"empty":         "required_checks: []\nprotected_paths: []\n",
		"empty command": "required_checks: [{name: test, command: '', timeout: 1m}]\nprotected_paths: ['.swarm/**']\n",
		"zero timeout":  "required_checks: [{name: test, command: echo test, timeout: 0s}]\nprotected_paths: ['.swarm/**']\n",
		"duplicate":     "required_checks: [{name: test, command: echo test, timeout: 1m}, {name: test, command: echo duplicate, timeout: 1m}]\nprotected_paths: ['.swarm/**']\n",
		"unknown key":   "required_checks: [{name: test, command: echo test, timeout: 1m}]\nprotected_paths: ['.swarm/**']\nignore_errors: true\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeValidationSpec([]byte(data)); err == nil {
				t.Fatal("unsafe validation policy accepted")
			}
		})
	}
}

func TestApprovedPolicyIgnoresCandidateChanges(t *testing.T) {
	root := t.TempDir()
	if _, err := InitRepository(InitOptions{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main")
	git("config", "user.name", "Harness Test")
	git("config", "user.email", "test@example.invalid")
	git("add", ".")
	git("commit", "-m", "approved policy")
	base := git("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(root, ".swarm", "config.yaml"), []byte("security: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(root); err == nil {
		t.Fatal("candidate malformed policy accepted")
	}
	cfg, err := LoadConfigAt(root, base)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Security.RequireReadyLabel {
		t.Fatal("approved readiness policy weakened by candidate")
	}
	if _, err := LoadConfigAt(root, "HEAD"); err == nil {
		t.Fatal("mutable base accepted")
	}
}

func TestInitPreservesExistingPolicyAndRejectsMutablePins(t *testing.T) {
	root := t.TempDir()
	if _, err := InitRepository(InitOptions{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".swarm", "config.yaml")
	custom := []byte("maintainer policy must not be overwritten\n")
	if err := os.WriteFile(path, custom, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InitRepository(InitOptions{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(custom) {
		t.Fatalf("init overwrote policy: %q, %v", got, err)
	}
	for _, version := range []string{"main", "latest", "v0.1.0", "v1.2.3;echo injected"} {
		if _, err := InitRepository(InitOptions{RepoRoot: t.TempDir(), SwarmVersion: version}); err == nil {
			t.Fatalf("unsafe/incompatible pin %q accepted", version)
		}
	}
}

func TestInitWorkflowUsesNewRepositoryCurrentBranch(t *testing.T) {
	root := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	runGit("init", "-b", "trunk")
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/consumer\ngo 1.24\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InitRepository(InitOptions{RepoRoot: root, SwarmVersion: "v0.2.0"}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "swarm-validate.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On struct {
			Push struct {
				Branches []string `yaml:"branches"`
			} `yaml:"push"`
		} `yaml:"on"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	if got := workflow.On.Push.Branches; len(got) != 1 || got[0] != "trunk" {
		t.Fatalf("push branches = %q, want [trunk]", got)
	}
}

func TestWorkflowDefaultBranchPrefersOriginSymbolicRef(t *testing.T) {
	root := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	runGit("init", "-b", "fallback")
	runGit("remote", "add", "origin", "https://example.invalid/consumer.git")
	runGit("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/trunk")

	branch, warning := workflowDefaultBranch(root)
	if warning != "" {
		t.Fatalf("unexpected warning: %s", warning)
	}
	if branch != "trunk" {
		t.Fatalf("default branch = %q, want trunk", branch)
	}
}

func TestInitScaffoldsGoalsSkillsIssueFormsAndCustomTemplateDir(t *testing.T) {
	root := t.TempDir()
	customDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(customDir, "rules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(customDir, "org-custom.yml"), []byte("name: Custom Org Form\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(customDir, "rules", "security.md"), []byte("# Security Rule\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := InitRepository(InitOptions{RepoRoot: root, TemplateDir: customDir}); err != nil {
		t.Fatal(err)
	}

	expectedFiles := []string{
		"AGENTS.md",
		".swarm/goals.md",
		".swarm/SKILLS.md",
		".swarm/rules/default.md",
		".swarm/rules/security.md",
		".swarm/skills/triage-and-plan.md",
		".github/ISSUE_TEMPLATE/swarm-workunit.yml",
		".github/ISSUE_TEMPLATE/swarm-feature.yml",
		".github/ISSUE_TEMPLATE/swarm-bugfix.yml",
		".github/ISSUE_TEMPLATE/swarm-refactor.yml",
		".github/ISSUE_TEMPLATE/config.yml",
		".github/ISSUE_TEMPLATE/org-custom.yml",
		".claude/commands/swarm-project-plan.md",
		".opencode/command/swarm-project-plan.md",
	}
	for _, rel := range expectedFiles {
		if !fileExists(filepath.Join(root, rel)) {
			t.Fatalf("expected %s to be created by InitRepository", rel)
		}
	}
}
