package harness

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

func TestCommitMessageConfigModesAndLegacyPolicy(t *testing.T) {
	const legacy = `version: 1
default_agent: codex
default_gate: agent
execution:
  mode: isolated
  backend: native
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
	legacyCfg, err := decodeConfig([]byte(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if legacyCfg.CommitMessage != nil || legacyCfg.CommitMessageDetail() != "concise" {
		t.Fatalf("legacy config must not materialize defaults: %+v", legacyCfg.CommitMessage)
	}
	b, err := json.Marshal(legacyCfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"commit_message"`) {
		t.Fatalf("legacy JSON acquired commit_message field: %s", b)
	}
	legacyDigest := contract.DigestApprovedPolicy("0123456789abcdef0123456789abcdef01234567", legacyCfg)

	for _, detail := range []string{"", "concise", "detailed"} {
		t.Run("accepted "+detail, func(t *testing.T) {
			cfg, err := decodeConfig([]byte(legacy + "commit_message:\n  detail: " + detail + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.CommitMessage == nil || cfg.CommitMessage.Detail != detail {
				t.Fatalf("explicit detail not preserved: %+v", cfg.CommitMessage)
			}
			want := detail
			if want == "" {
				want = "concise"
			}
			if got := cfg.CommitMessageDetail(); got != want {
				t.Fatalf("effective detail = %q, want %q", got, want)
			}
			if contract.DigestApprovedPolicy("0123456789abcdef0123456789abcdef01234567", cfg) == legacyDigest {
				t.Fatal("explicit commit_message did not affect policy digest")
			}
		})
	}
	for _, detail := range []string{"verbose", "Concise", "super-detailed"} {
		t.Run("invalid "+detail, func(t *testing.T) {
			cfg, err := decodeConfig([]byte(legacy + "commit_message:\n  detail: " + detail + "\n"))
			if err == nil || cfg != nil || !strings.Contains(err.Error(), "commit_message.detail") {
				t.Fatalf("invalid detail accepted or unclear error: cfg=%+v err=%v", cfg, err)
			}
		})
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
	configPath := filepath.Join(root, ".swarm", "config.yaml")
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	detailed := strings.Replace(string(configData), "detail: concise", "detail: detailed", 1)
	if detailed == string(configData) {
		t.Fatal("generated config missing concise detail")
	}
	if err := os.WriteFile(configPath, []byte(detailed), 0o600); err != nil {
		t.Fatal(err)
	}
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
	if cfg.CommitMessage == nil || cfg.CommitMessage.Detail != "detailed" || cfg.CommitMessageDetail() != "detailed" {
		t.Fatalf("approved detailed commit message policy lost: %+v", cfg.CommitMessage)
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
	for _, version := range []string{"main", "latest", "v0.1.0", "v0.2.0", "v0.3.0", "v0.5.99", "v1.2.3;echo injected", "v0.6.0+dirty", "v0.6.0-", "v18446744073709551616.0.0", "v0.18446744073709551616.0", "v0.6.18446744073709551616", strings.Repeat("A", 40)} {
		root := t.TempDir()
		if _, err := InitRepository(InitOptions{RepoRoot: root, SwarmVersion: version}); err == nil {
			t.Fatalf("unsafe/incompatible pin %q accepted", version)
		}
		if fileExists(filepath.Join(root, ".swarm", "config.yaml")) || fileExists(filepath.Join(root, ".github", "workflows", "swarm-validate.yml")) {
			t.Fatalf("invalid pin %q wrote scaffold files", version)
		}
	}
}

func TestValidateSwarmVersionPin(t *testing.T) {
	for _, version := range []string{"v0.6.0", "v0.6.0-rc.1", "v0.6.1", "v1.0.0", "v18446744073709551615.0.0", strings.Repeat("a", 40)} {
		if err := ValidateSwarmVersionPin(version); err != nil {
			t.Errorf("compatible pin %q rejected: %v", version, err)
		}
	}
	for _, version := range []string{"", "v0.5.999", "v0.5.9-rc.1", "v0.6", "v0.6.0+build", "v0.6.0-rc+build", strings.Repeat("A", 40), "v18446744073709551616.0.0", "v1.18446744073709551616.0", "v1.0.18446744073709551616"} {
		if err := ValidateSwarmVersionPin(version); err == nil || err.Error() != "swarm version must be a resource-CLI-compatible immutable release tag (v0.6.0 or newer) or full commit SHA" {
			t.Errorf("invalid pin %q: error = %v", version, err)
		}
	}
}

func TestInitCompatiblePins(t *testing.T) {
	for _, version := range []string{"v0.6.0", "v0.6.0-rc.1", "v1.0.0", strings.Repeat("a", 40)} {
		root := t.TempDir()
		if _, err := InitRepository(InitOptions{RepoRoot: root, SwarmVersion: version}); err != nil {
			t.Fatalf("compatible pin %q rejected: %v", version, err)
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
	if _, err := InitRepository(InitOptions{RepoRoot: root, SwarmVersion: "v0.6.0"}); err != nil {
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
		".github/ISSUE_TEMPLATE/org-custom.yml",
		".claude/commands/swarm-project-plan.md",
		".opencode/commands/swarm-project-plan.md",
	}
	for _, rel := range expectedFiles {
		if !fileExists(filepath.Join(root, rel)) {
			t.Fatalf("expected %s to be created by InitRepository", rel)
		}
	}
	if fileExists(filepath.Join(root, ".github/ISSUE_TEMPLATE/config.yml")) {
		t.Fatal("init created the repository-wide issue chooser config")
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

	// --fork-coordinator requires a resource-CLI-compatible --swarm-version pin.
	for _, badPin := range []string{"", "v0.2.0", "v0.3.0", "v0.5.0", "main"} {
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

func TestInitSharesRepositoryFilesWithoutTakingThemOver(t *testing.T) {
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	write := func(rel, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(rel string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	ownAgents := "# Team agent guide\n\nRun make check before committing.\n"
	ownChooser := "blank_issues_enabled: false\n"
	ownWorkflow := "name: Team CI\non: [push]\njobs: {}\n"
	ownSwarmNamed := "name: Our own swarm-validate\non: [push]\njobs: {}\n"
	write("AGENTS.md", ownAgents)
	write(".github/ISSUE_TEMPLATE/config.yml", ownChooser)
	write(".github/workflows/ci.yml", ownWorkflow)
	write(".github/workflows/swarm-validate.yml", ownSwarmNamed)

	res, err := InitRepository(InitOptions{RepoRoot: root, SwarmVersion: "v0.6.0"})
	if err != nil {
		t.Fatal(err)
	}
	agents := read("AGENTS.md")
	if !strings.HasPrefix(agents, ownAgents) || !strings.Contains(agents, harnessSectionStart) || !strings.Contains(agents, ".swarm/SKILLS.md") {
		t.Fatalf("AGENTS.md section not appended after the team's content:\n%s", agents)
	}
	if !slices.Contains(res.SectionFiles, "AGENTS.md") {
		t.Fatalf("section not reported: %+v", res)
	}
	if read(".github/ISSUE_TEMPLATE/config.yml") != ownChooser || read(".github/workflows/ci.yml") != ownWorkflow || read(".github/workflows/swarm-validate.yml") != ownSwarmNamed {
		t.Fatal("init changed repository-owned GitHub files")
	}
	if !slices.Contains(res.PreservedFiles, ".github/workflows/swarm-validate.yml") || slices.Contains(res.RemovedFiles, ".github/ISSUE_TEMPLATE/config.yml") {
		t.Fatalf("repository-owned files misreported: %+v", res)
	}

	// Rerunning is idempotent, and only the marked section is ever rewritten.
	if _, err := InitRepository(InitOptions{RepoRoot: root, SwarmVersion: "v0.6.0"}); err != nil {
		t.Fatal(err)
	}
	if read("AGENTS.md") != agents {
		t.Fatal("rerun changed AGENTS.md")
	}
	stale := strings.Replace(agents, "for the issue and PR workflow", "for an outdated workflow", 1)
	write("AGENTS.md", stale+"\n## Added later by the team\n")
	if _, err := InitRepository(InitOptions{RepoRoot: root, SwarmVersion: "v0.6.0"}); err != nil {
		t.Fatal(err)
	}
	if got := read("AGENTS.md"); got != agents+"\n## Added later by the team\n" {
		t.Fatalf("section refresh touched content outside the markers:\n%s", got)
	}
}

func TestInitRemovesRetiredFilesOnlyWhenUntouched(t *testing.T) {
	for name, edited := range map[string]bool{"untouched": false, "edited": true} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			chooser := filepath.Join(root, ".github", "ISSUE_TEMPLATE", "config.yml")
			if err := os.MkdirAll(filepath.Dir(chooser), 0o755); err != nil {
				t.Fatal(err)
			}
			body := retiredFiles[".github/ISSUE_TEMPLATE/config.yml"][0]
			if edited {
				body += "  - name: Team support\n    url: https://example.invalid\n    about: Ask us\n"
			}
			if err := os.WriteFile(chooser, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			res, err := InitRepository(InitOptions{RepoRoot: root, SwarmVersion: "v0.6.0"})
			if err != nil {
				t.Fatal(err)
			}
			if fileExists(chooser) == !edited || slices.Contains(res.RemovedFiles, ".github/ISSUE_TEMPLATE/config.yml") == edited {
				t.Fatalf("retired config.yml handling (edited=%t): exists=%t removed=%v", edited, fileExists(chooser), res.RemovedFiles)
			}
		})
	}
}

func TestInitNamesProjectAfterOriginRepository(t *testing.T) {
	root := filepath.Join(t.TempDir(), "local-clone-name")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"remote", "add", "origin", "https://github.com/octo/widgets.git"}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	if _, err := InitRepository(InitOptions{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	project, err := os.ReadFile(filepath.Join(root, ".swarm", "project.md"))
	if err != nil || !strings.HasPrefix(string(project), "# Project: widgets\n") {
		t.Fatalf("project.md = %q, %v", project, err)
	}
}
