package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/safefs"
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
func (h helperReviewer) Provenance(role string, readOnly bool) contract.RoleAttestation {
	return contract.RoleAttestation{
		Role:           role,
		Agent:          h.Name(),
		Harness:        "mock",
		HarnessVersion: "builtin",
		Model:          "default",
		Sandbox:        "mock",
	}
}
func (h helperReviewer) Run(ctx context.Context, req RunRequest) (*RunResult, error) {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestReviewHelperProcess", "--")
	cmd.Dir = req.WorktreePath
	cmd.Env = append(os.Environ(), "SWARM_REVIEW_HELPER=1", "SWARM_REVIEW_ROOT="+req.WorktreePath)
	if h.fail {
		cmd.Env = append(cmd.Env, "SWARM_REVIEW_FAIL=1")
	}
	out, err := cmd.CombinedOutput()
	result := &RunResult{Agent: h.Name(), Role: req.Role, Output: string(out), Attestation: h.Provenance(req.Role, req.ReadOnly)}
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
	cfg := &contract.SwarmConfig{Execution: contract.ExecutionConfig{Mode: contract.ExecutionTrustedLocal}, Agents: map[string]contract.AgentConfig{
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
	if got, err := os.ReadFile(filepath.Join(root, "tracked.txt")); err != nil || string(got) != "original" {
		t.Fatalf("original source changed: %q, %v", got, err)
	}
	if boundaryErr := sandbox.CheckReadOnlyRuntime("", "", ""); boundaryErr != nil {
		if reviewErr == nil {
			t.Fatal("review succeeded without the required host sandbox")
		}
	} else {
		if reviewErr != nil {
			// Runner errors deliberately redact harness diagnostics. Probe namespace
			// availability independently so a helper assertion can never trigger a skip.
			if runtime.GOOS == "linux" {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				probe := exec.CommandContext(ctx, "bwrap", "--ro-bind", "/", "/",
					"--unshare-user", "--unshare-ipc", "--unshare-pid", "--unshare-uts",
					"--unshare-net", "--", "/bin/true")
				output, probeErr := probe.CombinedOutput()
				if probeErr != nil && strings.Contains(string(output), "bwrap:") && strings.Contains(string(output), "Operation not permitted") {
					t.Skipf("host denies nested Bubblewrap namespaces: %s", strings.TrimSpace(string(output)))
				}
			}
			t.Fatal(reviewErr)
		}
		if got, err := os.ReadFile(filepath.Join(root, ".swarm/active/REVIEW.md")); err != nil || string(got) != "VERDICT: APPROVED\n" {
			t.Fatalf("isolated approval was not imported: %q, %v", got, err)
		}
	}
}

type reviewOutputRunner struct {
	*MockRunner
	produce func(RunRequest) error
}

func (r reviewOutputRunner) Run(_ context.Context, req RunRequest) (*RunResult, error) {
	return &RunResult{}, r.produce(req)
}

func TestReviewRejectsUnsafeOutput(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "oversize", "directory"} {
		t.Run(kind, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			mustWriteReviewFixture(t, outside, "canary", "coordinator-canary")
			runner := reviewOutputRunner{MockRunner: &MockRunner{}, produce: func(req RunRequest) error {
				path := filepath.Join(req.WorktreePath, ".swarm", "active-output", "REVIEW.md")
				switch kind {
				case "symlink":
					return os.Symlink(filepath.Join(outside, "canary"), path)
				case "hardlink":
					return os.Link(filepath.Join(outside, "canary"), path)
				case "directory":
					return os.Mkdir(path, 0o700)
				default:
					return os.WriteFile(path, []byte(strings.Repeat("x", maxReviewBytes+1)), 0o600)
				}
			}}
			if _, err := RunReview(context.Background(), runner, RunRequest{Role: "reviewer", WorktreePath: root}); err == nil {
				t.Fatal("unsafe output was imported")
			}
			if _, err := os.Stat(filepath.Join(root, ".swarm", "active", "REVIEW.md")); !os.IsNotExist(err) {
				t.Fatalf("unsafe output created destination: %v", err)
			}
			if got, err := os.ReadFile(filepath.Join(outside, "canary")); err != nil || string(got) != "coordinator-canary" {
				t.Fatalf("canary changed: %q %v", got, err)
			}
		})
	}
}

func TestReviewRejectsDestinationSwap(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	mustWriteReviewFixture(t, root, ".swarm/active/PLAN.md", "plan")
	runner := reviewOutputRunner{MockRunner: &MockRunner{}, produce: func(req RunRequest) error {
		if err := os.Rename(filepath.Join(root, ".swarm", "active"), filepath.Join(root, ".swarm", "old-active")); err != nil {
			return err
		}
		if err := os.Symlink(outside, filepath.Join(root, ".swarm", "active")); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(req.WorktreePath, ".swarm", "active-output", "REVIEW.md"), []byte("approved"), 0o600)
	}}
	if _, err := RunReview(context.Background(), runner, RunRequest{Role: "reviewer", WorktreePath: root}); err == nil {
		t.Fatal("accepted swapped destination directory")
	}
	if _, err := os.Stat(filepath.Join(outside, "REVIEW.md")); !os.IsNotExist(err) {
		t.Fatalf("wrote outside destination: %v", err)
	}
}

func TestSnapshotLimitsAndCancellation(t *testing.T) {
	for _, kind := range []string{"entries", "file", "aggregate", "cancelled", "depth"} {
		t.Run(kind, func(t *testing.T) {
			source := t.TempDir()
			mustWriteReviewFixture(t, source, "one", "1234")
			mustWriteReviewFixture(t, source, "two", "5678")
			limits := snapshotLimits{entries: 1000, fileBytes: 16, totalBytes: 32, duration: time.Minute}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "entries":
				limits.entries = 1
			case "file":
				limits.fileBytes = 3
			case "aggregate":
				limits.totalBytes = 7
			case "cancelled":
				cancel()
			case "depth":
				mustWriteReviewFixture(t, source, strings.Repeat("d/", 66)+"file", "deep")
			}
			if err := copySnapshotContext(ctx, source, filepath.Join(t.TempDir(), "snapshot"), limits); err == nil {
				t.Fatal("snapshot ignored resource boundary")
			}
		})
	}
}

func TestSnapshotSymlinkPolicy(t *testing.T) {
	for _, kind := range []string{"relative", "escape", "absolute", "active"} {
		t.Run(kind, func(t *testing.T) {
			source, target, outside := t.TempDir(), filepath.Join(t.TempDir(), "snapshot"), t.TempDir()
			mustWriteReviewFixture(t, source, "real.txt", "safe")
			mustWriteReviewFixture(t, outside, "secret", "canary")
			link, dest := "real.txt", filepath.Join(source, "link")
			switch kind {
			case "escape":
				link, _ = filepath.Rel(source, filepath.Join(outside, "secret"))
			case "absolute":
				link = filepath.Join(outside, "secret")
			case "active":
				if err := os.Mkdir(filepath.Join(source, ".swarm"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(source, "other"), 0o700); err != nil {
					t.Fatal(err)
				}
				link, dest = "../other", filepath.Join(source, ".swarm", "active")
			}
			if err := os.Symlink(link, dest); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			err := copySnapshot(source, target)
			if kind == "relative" && err != nil {
				t.Fatal(err)
			}
			if kind != "relative" && err == nil {
				t.Fatal("unsafe link accepted")
			}
		})
	}
}

type timeoutRecoveryReviewer struct {
	helperReviewer
	t        *testing.T
	original string
	snapshot string
}

func (r *timeoutRecoveryReviewer) Run(ctx context.Context, req RunRequest) (*RunResult, error) {
	r.t.Helper()
	r.snapshot = req.WorktreePath
	if req.RecoveryRoot != r.original || req.WorktreePath == r.original {
		r.t.Fatalf("recovery root lost: %+v", req)
	}
	// The sandbox exporter has its own patch safety tests. This runner verifies
	// the outer review snapshot's durable coordinator-selected destination.
	root, err := safefs.Open(req.RecoveryRoot)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err := root.MkdirAll(".swarm/active-output", 0700); err != nil {
		return nil, err
	}
	if err := root.WriteFile(".swarm/active-output/reviewer-timeout.patch", []byte("retained patch"), 0600); err != nil {
		return nil, err
	}
	return nil, context.DeadlineExceeded
}
func TestReviewTimeoutRecoveryRootSurvivesSnapshotCleanup(t *testing.T) {
	root := t.TempDir()
	mustWriteReviewFixture(t, root, ".swarm/active/REVIEW.md", "existing review")
	runner := &timeoutRecoveryReviewer{t: t, original: root}
	_, err := RunReview(context.Background(), runner, RunRequest{Role: "reviewer", WorktreePath: root})
	if err != context.DeadlineExceeded {
		t.Fatal(err)
	}
	if _, err := os.Stat(runner.snapshot); !os.IsNotExist(err) {
		t.Fatalf("snapshot remains: %v", err)
	}
	patch, err := os.ReadFile(filepath.Join(root, ".swarm/active-output/reviewer-timeout.patch"))
	if err != nil || string(patch) != "retained patch" {
		t.Fatalf("%s %v", patch, err)
	}
	review, err := os.ReadFile(filepath.Join(root, ".swarm/active/REVIEW.md"))
	if err != nil || string(review) != "existing review" {
		t.Fatalf("review changed: %s %v", review, err)
	}
}
