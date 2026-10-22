package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
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
		"unapproved default": strings.Replace(string(valid), "default_agent: codex", "default_agent: unapproved", 1),
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

	// Pre-populate a custom issue form to verify non-force InitRepository preserves it
	issueTmplDir := filepath.Join(root, ".github", "ISSUE_TEMPLATE")
	if err := os.MkdirAll(issueTmplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	customFormPath := filepath.Join(issueTmplDir, "swarm-community.yml")
	if err := os.WriteFile(customFormPath, []byte("name: Custom Community Form\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := InitRepository(InitOptions{RepoRoot: root, TemplateDir: customDir}); err != nil {
		t.Fatal(err)
	}

	preserved, err := os.ReadFile(customFormPath)
	if err != nil || string(preserved) != "name: Custom Community Form\n" {
		t.Fatalf("expected existing custom form to be preserved without --force, got %q (%v)", string(preserved), err)
	}

	// Re-run with Force: true to write the built-in swarm-community.yml
	if _, err := InitRepository(InitOptions{RepoRoot: root, TemplateDir: customDir, Force: true}); err != nil {
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
		".github/ISSUE_TEMPLATE/swarm-community.yml",
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

	commForm, err := os.ReadFile(filepath.Join(root, ".github/ISSUE_TEMPLATE/swarm-community.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(commForm), "name: Community report") || !strings.Contains(string(commForm), "swarm:community") || strings.Contains(string(commForm), "Acceptance Criteria") {
		t.Fatalf("unexpected swarm-community.yml content:\n%s", string(commForm))
	}

	wuForm, err := os.ReadFile(filepath.Join(root, ".github/ISSUE_TEMPLATE/swarm-workunit.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wuForm), "swarm:draft") || strings.Contains(string(wuForm), "swarm:ready") {
		t.Fatalf("expected swarm-workunit.yml to default to swarm:draft, got:\n%s", string(wuForm))
	}

	cmdContent, err := os.ReadFile(filepath.Join(root, ".claude/commands/swarm-project-plan.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cmdContent), "--from") || !strings.Contains(string(cmdContent), "--apply") || !strings.Contains(string(cmdContent), "swarm issue approve") {
		t.Fatalf("expected swarm-project-plan.md to document preview, explicit --apply, and selective child approval:\n%s", string(cmdContent))
	}
}

func TestForkCoordinatorWorkflowScaffoldAndPolicyDigest(t *testing.T) {
	valid, err := embeddedTemplates.ReadFile("templates/swarm/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	baseCfg, err := decodeConfig(valid)
	if err != nil {
		t.Fatal(err)
	}
	if baseCfg.MaxActiveForkClaims() != 1 {
		t.Fatalf("MaxActiveForkClaims() default = %d, want 1", baseCfg.MaxActiveForkClaims())
	}
	// Omitting fork config keeps DigestApprovedPolicy unchanged.
	digestBefore := contract.DigestApprovedPolicy("0123456789abcdef0123456789abcdef01234567", baseCfg)
	if _, err := decodeConfig([]byte(string(valid) + "fork:\n  max_active_claims: 0\n")); err == nil {
		t.Fatal("expected non-positive fork.max_active_claims to fail decodeConfig")
	}
	withFork, err := decodeConfig([]byte(string(valid) + "fork:\n  max_active_claims: 2\n"))
	if err != nil || withFork.MaxActiveForkClaims() != 2 {
		t.Fatalf("decodeConfig with fork.max_active_claims=2 failed: %v (%+v)", err, withFork)
	}
	withNilFork := *withFork
	withNilFork.Fork = nil
	if got := contract.DigestApprovedPolicy("0123456789abcdef0123456789abcdef01234567", &withNilFork); got != digestBefore {
		t.Fatalf("DigestApprovedPolicy changed when Fork is nil: got %s, want %s", got, digestBefore)
	}

	// Default init does not create swarm-fork-coordinator.yml
	defaultRoot := t.TempDir()
	if _, err := InitRepository(InitOptions{RepoRoot: defaultRoot}); err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(defaultRoot, ".github", "workflows", "swarm-fork-coordinator.yml")) {
		t.Fatal("default InitRepository must not scaffold swarm-fork-coordinator.yml without --fork-coordinator")
	}

	// --fork-coordinator requires a fork-capable --swarm-version pin
	for _, badPin := range []string{"", "v0.2.0", "v0.3.0", "main"} {
		if _, err := InitRepository(InitOptions{RepoRoot: t.TempDir(), ForkCoordinator: true, SwarmVersion: badPin}); err == nil {
			t.Fatalf("expected ForkCoordinator with pin %q to fail", badPin)
		}
	}

	pin := "a1b2c3d4e5f60123456789abcdef0123456789ab"
	optInRoot := t.TempDir()
	if out, err := exec.Command("git", "-C", optInRoot, "init", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	if _, err := InitRepository(InitOptions{RepoRoot: optInRoot, ForkCoordinator: true, SwarmVersion: pin}); err != nil {
		t.Fatalf("InitRepository(ForkCoordinator) error = %v", err)
	}
	coordPath := filepath.Join(optInRoot, ".github", "workflows", "swarm-fork-coordinator.yml")
	raw, err := os.ReadFile(coordPath)
	if err != nil {
		t.Fatalf("read generated swarm-fork-coordinator.yml: %v", err)
	}
	var parsed struct {
		On struct {
			IssueComment struct {
				Types []string `yaml:"types"`
			} `yaml:"issue_comment"`
		} `yaml:"on"`
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			If    string `yaml:"if"`
			Steps []struct {
				Uses string            `yaml:"uses"`
				With map[string]any    `yaml:"with"`
				Env  map[string]string `yaml:"env"`
				Run  string            `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal swarm-fork-coordinator.yml: %v", err)
	}
	if len(parsed.On.IssueComment.Types) != 1 || parsed.On.IssueComment.Types[0] != "created" {
		t.Fatalf("unexpected trigger types: %+v", parsed.On.IssueComment.Types)
	}
	if parsed.Permissions["contents"] != "write" || parsed.Permissions["issues"] != "write" {
		t.Fatalf("unexpected workflow permissions: %+v", parsed.Permissions)
	}
	job, ok := parsed.Jobs["coordinate"]
	if !ok || len(job.Steps) < 5 {
		t.Fatalf("missing coordinate job steps: %+v", parsed.Jobs)
	}
	if !strings.Contains(job.Steps[0].Run, "current_comment = fetch(") || !strings.Contains(job.Steps[0].Run, "current_issue = fetch(") {
		t.Fatal("API admission must run before checkout")
	}
	if job.Steps[1].With["persist-credentials"] != false {
		t.Fatalf("checkout step must set persist-credentials: false, got %+v", job.Steps[1].With)
	}
	if job.Steps[3].Env["SWARM_VERSION"] != pin {
		t.Fatalf("install step SWARM_VERSION = %q, want %q", job.Steps[3].Env["SWARM_VERSION"], pin)
	}
	if !strings.Contains(job.Steps[4].Run, `fork-coordinator --event "$GITHUB_EVENT_PATH"`) {
		t.Fatalf("unexpected coordinator run command: %q", job.Steps[4].Run)
	}
}
