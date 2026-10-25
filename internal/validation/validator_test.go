package validation

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
)

func setupGitRepoForValidation(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", tmp}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, string(out))
		}
	}
	runGit("init", "-b", "main")
	runGit("config", "user.name", "Test")
	runGit("config", "user.email", "test@localhost")

	for _, rel := range []string{
		".github/workflows/swarm-validate.yml",
		".swarm/validation/required-checks.yaml",
		".swarm/config.yaml",
		"main.go",
	} {
		full := filepath.Join(tmp, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		var contents string
		if rel == ".swarm/validation/required-checks.yaml" {
			contents = "required_checks:\n  - name: ok\n    command: \"true\"\n    timeout: 1m\nprotected_paths:\n  - .github/workflows/**\n  - .swarm/validation/**\n"
		} else if rel == ".swarm/config.yaml" {
			// These fixtures exercise validation policy in explicit host compatibility mode.
			contents = "version: 1\ndefault_agent: mock\ndefault_gate: none\nexecution:\n  mode: trusted-local\nlease:\n  duration: 1h\n  max_renewals: 1\nsecurity:\n  require_ready_label: true\n  ready_label: swarm:ready\nagents:\n  mock:\n    binary: git\n    harness: mock\n"
		} else {
			contents = "initial\n"
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit("add", ".")
	runGit("commit", "-m", "initial")

	activeDir := filepath.Join(tmp, ".swarm", "active")
	_ = os.MkdirAll(activeDir, 0o755)
	_ = os.WriteFile(filepath.Join(activeDir, "PLAN.md"), []byte("# Plan\n"), 0o644)
	_ = os.WriteFile(filepath.Join(activeDir, "TEST_PLAN.md"), []byte("# Test Plan\n"), 0o644)
	return tmp
}

func TestProtectedPathTamperDetection(t *testing.T) {
	ctx := context.Background()
	repo := setupGitRepoForValidation(t)
	base := gitRevision(t, repo, "HEAD")
	// Git's nonfatal line-ending warnings must never become filename data.
	if out, err := exec.Command("git", "-C", repo, "config", "core.autocrlf", "true").CombinedOutput(); err != nil {
		t.Fatalf("configure CRLF conversion: %v (%s)", err, out)
	}

	// Tamper with a protected file (.github/workflows/swarm-validate.yml)
	if err := os.WriteFile(filepath.Join(repo, ".github/workflows/swarm-validate.yml"), []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wu := &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1}
	_, err := Validate(ctx, repo, wu, ValidateOptions{BaseSHA: base})
	if !errors.Is(err, ErrProtectedPathModified) {
		t.Fatalf("expected ErrProtectedPathModified, got: %v", err)
	}

	// A candidate cannot remove protected paths or checks by replacing its own
	// policy; the baseline remains authoritative.
	if err := os.WriteFile(filepath.Join(repo, ".swarm", "validation", "required-checks.yaml"), []byte("required_checks: []\nprotected_paths: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Validate(ctx, repo, wu, ValidateOptions{BaseSHA: base})
	if !errors.Is(err, ErrProtectedPathModified) {
		t.Fatalf("candidate policy tampering must not weaken baseline: %v", err)
	}
}

func TestCommittedPolicyTamperingUsesTrustedBase(t *testing.T) {
	repo := setupGitRepoForValidation(t)
	base := gitRevision(t, repo, "HEAD")
	policy := filepath.Join(repo, ".swarm", "validation", "required-checks.yaml")
	if err := os.WriteFile(policy, []byte("required_checks: []\nprotected_paths: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("add", ".swarm/validation/required-checks.yaml")
	run("commit", "-m", "tamper policy")
	_, err := Validate(context.Background(), repo, &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1}, ValidateOptions{BaseSHA: base})
	if !errors.Is(err, ErrProtectedPathModified) {
		t.Fatalf("committed tampering must fail: %v", err)
	}
}

func TestNULPathsAndInstructionFilesAreNotIgnored(t *testing.T) {
	repo := setupGitRepoForValidation(t)
	base := gitRevision(t, repo, "HEAD")
	path := filepath.Join(repo, "AGENTS.md")
	if err := os.WriteFile(path, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	name := "space \n name.yml"
	if runtime.GOOS == "windows" {
		// Windows rejects control characters in filenames; retain whitespace coverage.
		name = "space name.yml"
	}
	weird := filepath.Join(repo, ".github", "workflows", name)
	if err := os.WriteFile(weird, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Validate(context.Background(), repo, &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1}, ValidateOptions{BaseSHA: base})
	if !errors.Is(err, ErrProtectedPathModified) {
		t.Fatalf("protected filename containing whitespace must fail: %v", err)
	}
}

func TestSymlinkEscapeFailsClosed(t *testing.T) {
	repo := setupGitRepoForValidation(t)
	base := gitRevision(t, repo, "HEAD")
	outside := filepath.Join(t.TempDir(), "outside.yml")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(repo, ".github", "workflows", "escape.yml")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	_, err := Validate(context.Background(), repo, &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1}, ValidateOptions{BaseSHA: base})
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("symlink escape must fail closed: %v", err)
	}
}

func gitRevision(t *testing.T, repo, revision string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "rev-parse", revision).Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out[:len(out)-1])
}

func TestValidationRejectsMutableBaseline(t *testing.T) {
	repo := setupGitRepoForValidation(t)
	for _, base := range []string{"HEAD", "HEAD~0", gitRevision(t, repo, "HEAD")[:12]} {
		t.Run(base, func(t *testing.T) {
			_, err := Validate(context.Background(), repo, nil, ValidateOptions{BaseSHA: base, CIMode: true})
			if !errors.Is(err, ErrInvalidPolicy) {
				t.Fatalf("mutable or abbreviated baseline %q must fail closed: %v", base, err)
			}
		})
	}
}

func TestForkModeProtectedPathsRejectHarnessAndWorkflowsDespiteAllowHarnessChanges(t *testing.T) {
	ctx := context.Background()
	for _, tamperedFile := range []string{
		".swarm/validation/required-checks.yaml",
		".swarm/config.yaml",
		".github/workflows/swarm-fork-coordinator.yml",
	} {
		t.Run(tamperedFile, func(t *testing.T) {
			repo := setupGitRepoForValidation(t)
			base := gitRevision(t, repo, "HEAD")
			full := filepath.Join(repo, tamperedFile)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte("tampered: true\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			wu := &contract.WorkUnit{ID: "ISSUE-42", IssueNumber: 42, AllowHarnessChanges: true}
			_, err := Validate(ctx, repo, wu, ValidateOptions{BaseSHA: base, ForkMode: true})
			if !errors.Is(err, ErrProtectedPathModified) {
				t.Fatalf("ForkMode Validate(%s) with AllowHarnessChanges=true = %v, want ErrProtectedPathModified", tamperedFile, err)
			}
		})
	}
}

func TestTemporaryTaskPointerIsNotAProtectedChange(t *testing.T) {
	ctx := context.Background()
	repo := setupGitRepoForValidation(t)
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=Test", "-c", "user.email=test@localhost"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	policy := "required_checks:\n  - name: ok\n    command: \"true\"\n    timeout: 1m\nprotected_paths:\n  - .github/workflows/**\n  - .swarm/validation/**\n  - AGENTS.md\n"
	if err := os.WriteFile(filepath.Join(repo, ".swarm", "validation", "required-checks.yaml"), []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}
	agents := "# Team guide\n\nKeep changes small.\n"
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte(agents), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "protect AGENTS.md")
	base := gitRevision(t, repo, "HEAD")
	wu := &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1}
	pointer := "<!-- swarm:active:start -->\n## Active SwarmOS Work Unit\n<!-- swarm:active:end -->\n\n"

	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(pointer + agents)
	if _, err := Validate(ctx, repo, wu, ValidateOptions{BaseSHA: base}); err != nil {
		t.Fatalf("temporary task pointer failed validation: %v", err)
	}
	write(pointer + agents + "Also skip tests.\n")
	if _, err := Validate(ctx, repo, wu, ValidateOptions{BaseSHA: base}); !errors.Is(err, ErrProtectedPathModified) {
		t.Fatalf("edit beside the task pointer passed: %v", err)
	}
	write(pointer + agents)
	git("add", "AGENTS.md")
	if _, err := Validate(ctx, repo, wu, ValidateOptions{BaseSHA: base}); !errors.Is(err, ErrProtectedPathModified) {
		t.Fatalf("staged task pointer passed: %v", err)
	}
}

func TestRunnerIsolatedChecksDoNotNeedAgentSandbox(t *testing.T) {
	ctx := context.Background()
	repo := setupGitRepoForValidation(t)
	// Standard policy: Codex's native sandbox, which stock CI runners lack.
	config := "version: 1\ndefault_agent: codex\ndefault_gate: agent\nexecution:\n  mode: isolated\n  backend: native\nlease:\n  duration: 1h\n  max_renewals: 1\nsecurity:\n  require_isolation: true\n  require_ready_label: true\n  ready_label: swarm:ready\nagents:\n  codex:\n    binary: swarm-test-missing-codex\n"
	if err := os.WriteFile(filepath.Join(repo, ".swarm", "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "standard isolation policy"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=T", "-c", "user.email=t@localhost"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	base := gitRevision(t, repo, "HEAD")
	if _, err := Validate(ctx, repo, nil, ValidateOptions{CIMode: true, BaseSHA: base}); err == nil {
		t.Fatal("workstation validation ran without the configured sandbox")
	}
	report, err := Validate(ctx, repo, nil, ValidateOptions{CIMode: true, RunnerIsolated: true, BaseSHA: base})
	if err != nil || !report.Passed {
		t.Fatalf("CI runner validation = %+v, %v", report, err)
	}
}

func TestReportProtectedChangesListsPolicyEditsAndStillRunsBaseChecks(t *testing.T) {
	ctx := context.Background()
	repo := setupGitRepoForValidation(t)
	base := gitRevision(t, repo, "HEAD")
	for _, rel := range []string{".github/workflows/swarm-validate.yml", ".swarm/validation/required-checks.yaml"} {
		// The candidate also tries to drop every required check.
		if err := os.WriteFile(filepath.Join(repo, rel), []byte("required_checks: []\nprotected_paths: []\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	report, err := Validate(ctx, repo, nil, ValidateOptions{CIMode: true, BaseSHA: base, ReportProtectedChanges: true})
	if err != nil {
		t.Fatalf("report mode: %v", err)
	}
	want := []string{".github/workflows/swarm-validate.yml", ".swarm/validation/required-checks.yaml"}
	if len(report.ProtectedChanges) != len(want) || report.ProtectedChanges[0] != want[0] || report.ProtectedChanges[1] != want[1] {
		t.Fatalf("protected changes = %v, want %v", report.ProtectedChanges, want)
	}
	if len(report.CheckOutcomes) != 1 || report.CheckOutcomes[0].Name != "ok" || !report.Passed {
		t.Fatalf("base checks must still run in report mode: %+v", report.CheckOutcomes)
	}

	if _, err := Validate(ctx, repo, nil, ValidateOptions{CIMode: true, BaseSHA: base}); !errors.Is(err, ErrProtectedPathModified) {
		t.Fatalf("enforce mode must still reject: %v", err)
	}
	wu := &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1}
	if _, err := Validate(ctx, repo, wu, ValidateOptions{BaseSHA: base, ReportProtectedChanges: true}); err == nil || errors.Is(err, ErrProtectedPathModified) {
		t.Fatalf("agent validation must refuse report mode before checking files: %v", err)
	}
	if _, err := Validate(ctx, repo, nil, ValidateOptions{CIMode: true, ForkMode: true, BaseSHA: base, ReportProtectedChanges: true}); err == nil {
		t.Fatal("fork mode must refuse report mode")
	}
}
