package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/sandbox"
)

// TestReviewHelperProcess is a portable hostile reviewer used by the regression
// below. It deliberately changes tracked/untracked files and can fail after it.
func TestReviewHelperProcess(t *testing.T) {
	if os.Getenv("SWARM_REVIEW_HELPER") != "1" {
		return
	}
	root := os.Getenv("SWARM_REVIEW_ROOT")
	_ = os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("overwritten"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "rogue.txt"), []byte("rogue"), 0o644)
	_ = os.WriteFile(filepath.Join(root, ".swarm", "active", "REVIEW.md"), []byte("VERDICT: APPROVED\n"), 0o644)
	if os.Getenv("SWARM_REVIEW_FAIL") == "1" {
		os.Exit(19)
	}
	os.Exit(0)
}

type helperReviewer struct{ fail bool }

func (h helperReviewer) Name() string                               { return "test-helper" }
func (h helperReviewer) Available() (string, bool)                  { return os.Args[0], true }
func (h helperReviewer) BuildCommand(RunRequest) (string, []string) { return os.Args[0], nil }
func (h helperReviewer) Run(ctx context.Context, req RunRequest) (*RunResult, error) {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestReviewHelperProcess", "--")
	cmd.Dir = req.WorktreePath
	cmd.Env = append(os.Environ(), "SWARM_REVIEW_HELPER=1", "SWARM_REVIEW_ROOT="+req.WorktreePath)
	if h.fail {
		cmd.Env = append(cmd.Env, "SWARM_REVIEW_FAIL=1")
	}
	out, err := cmd.CombinedOutput()
	result := &RunResult{Agent: h.Name(), Role: req.Role, Output: string(out)}
	if err != nil {
		return result, err
	}
	return result, nil
}

func TestRunReviewPreservesOriginalOnHostileSuccessAndFailure(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[failed], func(t *testing.T) {
			root := t.TempDir()
			mustWriteReviewFixture(t, root, "tracked.txt", "original")
			mustWriteReviewFixture(t, root, "untracked.txt", "keep")
			mustWriteReviewFixture(t, root, ".swarm/active/PLAN.md", "plan")
			before := snapshotFiles(t, root)
			_, err := RunReview(context.Background(), helperReviewer{fail: failed}, RunRequest{Role: "reviewer", WorktreePath: root})
			if failed && err == nil {
				t.Fatal("failed reviewer returned nil error")
			}
			if !failed && err != nil {
				t.Fatalf("successful reviewer: %v", err)
			}
			after := snapshotFiles(t, root)
			if failed {
				if after != before {
					t.Fatalf("failed review changed original workspace\nbefore=%q\nafter=%q", before, after)
				}
			} else {
				if strings.Contains(after, "overwritten") || strings.Contains(after, "rogue.txt") {
					t.Fatalf("hostile reviewer changed original workspace: %q", after)
				}
				if got, err := os.ReadFile(filepath.Join(root, ".swarm/active/REVIEW.md")); err != nil || string(got) != "VERDICT: APPROVED\n" {
					t.Fatalf("review output was not imported: %q %v", got, err)
				}
			}
		})
	}
}

func mustWriteReviewFixture(t *testing.T, root, name, contents string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
func snapshotFiles(t *testing.T, root string) string {
	t.Helper()
	var rows []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rows = append(rows, rel+"="+string(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(rows, "\n")
}

func TestSandboxReviewerHelper(t *testing.T) {
	if os.Getenv("SWARM_SANDBOX_HELPER") != "1" {
		return
	}
	if os.Getenv("MODEL_AUTH_TEST") != "allowed-model-key" || os.Getenv("GH_TOKEN") != "" {
		t.Fatal("model authentication or publishing credential boundary failed")
	}
	if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), "runtime-cache"), 0o700); err != nil {
		t.Fatalf("reviewer cannot use its private scratch home: %v", err)
	}
	if _, err := os.ReadFile(os.Getenv("SWARM_SECRET_FILE")); err == nil {
		t.Fatal("reviewer read coordinator home credentials")
	}
	for _, path := range []string{"tracked.txt", os.Getenv("SWARM_SOURCE_FILE")} {
		if err := os.WriteFile(path, []byte("unauthorized"), 0o644); err == nil {
			t.Fatalf("reviewer changed readonly source %s", path)
		}
	}
	if err := os.WriteFile(".swarm/active/REVIEW.md", []byte("VERDICT: APPROVED\n"), 0o644); err != nil {
		t.Fatalf("review output channel unavailable: %v", err)
	}
}

func TestReviewerSandboxAllowsScratchButDeniesSourceAndCredentials(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	mustWriteReviewFixture(t, root, "tracked.txt", "original")
	mustWriteReviewFixture(t, home, "secret.txt", "coordinator-only")
	t.Setenv("HOME", home)
	t.Setenv("SWARM_SANDBOX_HELPER", "1")
	t.Setenv("MODEL_AUTH_TEST", "allowed-model-key")
	t.Setenv("GH_TOKEN", "publishing-key")
	t.Setenv("SWARM_SECRET_FILE", filepath.Join(home, "secret.txt"))
	t.Setenv("SWARM_SOURCE_FILE", filepath.Join(root, "tracked.txt"))
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &contract.SwarmConfig{Agents: map[string]contract.AgentConfig{
		"isolated-review": {
			Binary: binary, Harness: "claude", Roles: []string{"reviewer"},
			Args: []string{"-test.run=^TestSandboxReviewerHelper$", "--"},
			Env:  []string{"SWARM_SANDBOX_HELPER", "MODEL_AUTH_TEST", "SWARM_SECRET_FILE", "SWARM_SOURCE_FILE"},
		},
	}}
	runner, err := NewConfiguredRunner("isolated-review", cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	_, reviewErr := RunReview(context.Background(), runner, RunRequest{Role: "reviewer", WorktreePath: root, Mode: "headless"})
	if boundaryErr := sandbox.CheckReadOnlyRuntime("", "", ""); boundaryErr != nil {
		if reviewErr == nil {
			t.Fatal("review succeeded without the required host sandbox")
		}
	} else {
		if reviewErr != nil {
			t.Fatal(reviewErr)
		}
		if got, err := os.ReadFile(filepath.Join(root, ".swarm/active/REVIEW.md")); err != nil || string(got) != "VERDICT: APPROVED\n" {
			t.Fatalf("isolated approval was not imported: %q, %v", got, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(root, "tracked.txt")); err != nil || string(got) != "original" {
		t.Fatalf("original source changed: %q, %v", got, err)
	}
}
