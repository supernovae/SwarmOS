package validation

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
		"main.go",
	} {
		full := filepath.Join(tmp, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("initial\n"), 0o644); err != nil {
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

	// Tamper with a protected file (.github/workflows/swarm-validate.yml)
	if err := os.WriteFile(filepath.Join(repo, ".github/workflows/swarm-validate.yml"), []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	wu := &contract.WorkUnit{
		ID:                  "ISSUE-1",
		IssueNumber:         1,
		AllowHarnessChanges: false,
	}
	spec := &contract.ValidationSpec{
		ProtectedPaths: []string{".github/workflows/**", ".swarm/validation/**", ".swarm/contracts/**"},
		RequiredChecks: []contract.CheckSpec{{Name: "ok", Command: "true"}},
	}

	_, err := Validate(ctx, repo, wu, spec)
	if !errors.Is(err, ErrProtectedPathModified) {
		t.Fatalf("expected ErrProtectedPathModified, got: %v", err)
	}

	// Now allow harness changes and verify it passes
	wu.AllowHarnessChanges = true
	report, err := Validate(ctx, repo, wu, spec)
	if err != nil {
		t.Fatalf("expected pass when AllowHarnessChanges=true, got: %v", err)
	}
	if !report.Passed {
		t.Fatal("expected report.Passed to be true")
	}
}
