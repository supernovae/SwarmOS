package sandbox

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests intentionally require two explicit opt-ins. Run them only against
// a disposable audit daemon/VM with a locally present, digest-pinned shell image.
func workerOCIOptions(t *testing.T) WorkerOptions {
	t.Helper()
	image, runtime := os.Getenv("SWARM_TEST_WORKER_IMAGE"), os.Getenv("SWARM_TEST_WORKER_RUNTIME")
	if image == "" || runtime == "" {
		t.Skip("set SWARM_TEST_WORKER_IMAGE and SWARM_TEST_WORKER_RUNTIME in a disposable audit environment")
	}
	if !pinnedWorkerImage.MatchString(image) {
		t.Fatal("SWARM_TEST_WORKER_IMAGE must be digest-pinned")
	}
	opts := WorkerOptions{Runtime: runtime, Image: image, Role: "developer", Timeout: 2 * time.Minute}
	if err := CheckWorkerRuntime(context.Background(), runtime, image); err != nil {
		t.Fatalf("required OCI boundary unavailable: %v", err)
	}
	return opts
}

func shellWorkerQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func testWorkerOCIWritableBoundaries(t *testing.T, role string) {
	opts := workerOCIOptions(t)
	opts.Role = role
	worktree, host := t.TempDir(), t.TempDir()
	writeWorkerFixture(t, worktree, "source.txt", "original")
	writeWorkerFixture(t, worktree, "delete.txt", "delete")
	writeWorkerFixture(t, worktree, ".git/config", "publication authority")
	writeWorkerFixture(t, worktree, ".swarm/coordination/state", "coordinator state")
	writeWorkerFixture(t, host, "canary", "host-only-canary")
	t.Setenv("GH_TOKEN", "synthetic-publishing-token")
	t.Setenv("GITHUB_TOKEN", "synthetic-github-token")
	t.Setenv("ANTHROPIC_API_KEY", "synthetic-provider-token")
	canary := shellWorkerQuote(filepath.Join(host, "canary"))
	script := `test -z "${GH_TOKEN+x}"
 test -z "${GITHUB_TOKEN+x}"
 test -z "${ANTHROPIC_API_KEY+x}"
 test ! -e .git
 test ! -e .swarm/coordination
 test ! -e /var/run/docker.sock
 test ! -e /run/podman/podman.sock
 for interface in /sys/class/net/*; do
   case "$interface" in */lo) ;; *) exit 51;; esac
 done
 if cat ` + canary + ` >/dev/null 2>&1; then exit 52; fi
 if printf changed > ` + canary + ` 2>/dev/null; then exit 53; fi
 printf changed > source.txt
 printf new > added.txt
 rm delete.txt
 printf private > /home/worker/session
 printf private > /tmp/session`
	if err := RunWorker(context.Background(), worktree, "/bin/sh", []string{"-ec", script}, opts, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"source.txt": "changed", "added.txt": "new", ".git/config": "publication authority", ".swarm/coordination/state": "coordinator state"} {
		got, err := os.ReadFile(filepath.Join(worktree, filepath.FromSlash(name)))
		if err != nil || string(got) != want {
			t.Fatalf("%s: %q %v", name, got, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(host, "canary"))
	if err != nil || string(got) != "host-only-canary" {
		t.Fatalf("host canary changed: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "delete.txt")); !os.IsNotExist(err) {
		t.Fatalf("worker deletion not imported: %v", err)
	}
}

func TestWorkerOCIReadonlyPlanner(t *testing.T) {
	t.Parallel()
	opts := workerOCIOptions(t)
	opts.Role = "planner"
	opts.ReadOnly = true
	worktree := t.TempDir()
	writeWorkerFixture(t, worktree, "source.txt", "original")
	script := `if printf overwrite > source.txt 2>/dev/null; then exit 61; fi
 if chmod u+w source.txt 2>/dev/null; then exit 62; fi
 if rm source.txt 2>/dev/null; then exit 63; fi
 printf plan > .swarm/active/PLAN.md
 printf tests > .swarm/active/TEST_PLAN.md`
	if err := RunWorker(context.Background(), worktree, "/bin/sh", []string{"-ec", script}, opts, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"source.txt": "original", ".swarm/active/PLAN.md": "plan", ".swarm/active/TEST_PLAN.md": "tests"} {
		got, err := os.ReadFile(filepath.Join(worktree, filepath.FromSlash(name)))
		if err != nil || string(got) != want {
			t.Fatalf("%s: %q %v", name, got, err)
		}
	}
}

func TestWorkerOCIRejectsUnsafeOutput(t *testing.T) {
	t.Parallel()
	opts := workerOCIOptions(t)
	for name, script := range map[string]string{
		"git":      "mkdir .git; printf unauthorized > .git/config",
		"symlink":  "ln -s /etc/passwd alias",
		"hardlink": "ln source.txt alias",
	} {
		t.Run(name, func(t *testing.T) {
			worktree := t.TempDir()
			writeWorkerFixture(t, worktree, "source.txt", "original")
			if err := RunWorker(context.Background(), worktree, "/bin/sh", []string{"-ec", script}, opts, nil, io.Discard, io.Discard); err == nil {
				t.Fatal("unsafe worker output was imported")
			}
			entries, err := os.ReadDir(worktree)
			if err != nil || len(entries) != 1 || entries[0].Name() != "source.txt" {
				t.Fatalf("unsafe output changed host workspace: %v %v", entries, err)
			}
		})
	}
}

type workerStartSignal struct {
	mu      sync.Mutex
	text    string
	started chan struct{}
	once    sync.Once
}

func (w *workerStartSignal) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.text) < 4096 {
		w.text += string(data)
	}
	if strings.Contains(w.text, "worker-started") {
		w.once.Do(func() { close(w.started) })
	}
	return len(data), nil
}

func TestWorkerOCICancellationTerminatesDescendants(t *testing.T) {
	t.Parallel()
	opts := workerOCIOptions(t)
	runtime, err := workerRuntime(opts.Runtime, opts.Image)
	if err != nil {
		t.Fatal(err)
	}
	before, err := workerRun(context.Background(), runtime, "ps", "--all", "--filter", "label=swarmos.worker=true", "--format", "{{.ID}}")
	if err != nil {
		t.Fatal(err)
	}
	worktree := t.TempDir()
	writeWorkerFixture(t, worktree, "source.txt", "original")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signal := &workerStartSignal{started: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- RunWorker(ctx, worktree, "/bin/sh", []string{"-ec", `/bin/sh -c 'sleep 30; printf escaped > late.txt' &
 printf early > early.txt
 printf worker-started
 wait`}, opts, nil, signal, io.Discard)
	}()
	select {
	case <-signal.started:
		cancel()
	case err := <-done:
		t.Fatalf("worker never reached descendant test: %v", err)
	case <-time.After(time.Minute):
		cancel()
		<-done
		t.Fatal("worker did not start within a minute")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled worker reported success")
		}
	case <-time.After(40 * time.Second):
		t.Fatal("worker cancellation did not complete cleanup")
	}
	for _, name := range []string{"early.txt", "late.txt"} {
		if _, err := os.Stat(filepath.Join(worktree, name)); !os.IsNotExist(err) {
			t.Fatalf("cancelled worker imported %s: %v", name, err)
		}
	}
	after, err := workerRun(context.Background(), runtime, "ps", "--all", "--filter", "label=swarmos.worker=true", "--format", "{{.ID}}")
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("cancelled worker left a container behind")
	}
}

func TestWorkerOCIMissingCapabilitiesFailClosed(t *testing.T) {
	t.Parallel()
	opts := workerOCIOptions(t)
	root := t.TempDir()
	writeWorkerFixture(t, root, "source.txt", "original")
	for name, bad := range map[string]WorkerOptions{
		"missing-runtime": {Runtime: filepath.Join(t.TempDir(), "docker"), Image: opts.Image},
		"missing-image":   {Runtime: opts.Runtime, Image: "swarmos-missing-audit-image@sha256:" + strings.Repeat("0", 64)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := RunWorker(context.Background(), root, "/bin/sh", []string{"-ec", "printf escaped > host-execution"}, bad, nil, io.Discard, io.Discard); err == nil {
				t.Fatal("missing isolation capability accepted")
			}
			if _, err := os.Stat(filepath.Join(root, "host-execution")); !os.IsNotExist(err) {
				t.Fatalf("worker fell back to host execution: %v", err)
			}
		})
	}
}

func TestWorkerOCIDeveloperBoundaries(t *testing.T) { testWorkerOCIWritableBoundaries(t, "developer") }
func TestWorkerOCITesterBoundaries(t *testing.T)    { testWorkerOCIWritableBoundaries(t, "tester") }

func TestWorkerOCIReviewerOutputChannel(t *testing.T) {
	t.Parallel()
	opts := workerOCIOptions(t)
	opts.Role = "reviewer"
	opts.ReadOnly = true
	root := t.TempDir()
	writeWorkerFixture(t, root, "source.txt", "original")
	writeWorkerFixture(t, root, ".swarm/active/PLAN.md", "plan")
	opts.OutputDir = filepath.Join(root, ".swarm", "active-output")
	if err := os.MkdirAll(opts.OutputDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(opts.OutputDir, "REVIEW.md"), filepath.Join(root, ".swarm", "active", "REVIEW.md")); err != nil {
		t.Skipf("review snapshot link unavailable: %v", err)
	}
	script := `test "$(cat .swarm/active/PLAN.md)" = plan
 if printf overwrite > source.txt 2>/dev/null; then exit 61; fi
 if printf overwrite > .swarm/active/PLAN.md 2>/dev/null; then exit 62; fi
 printf reviewed > .swarm/active/REVIEW.md`
	if err := RunWorker(context.Background(), root, "/bin/sh", []string{"-ec", script}, opts, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(opts.OutputDir, "REVIEW.md"))
	if err != nil || string(got) != "reviewed" {
		t.Fatalf("review output not imported: %q %v", got, err)
	}
}

func TestWorkerOCIInteractiveInputAndStorageLimit(t *testing.T) {
	t.Parallel()
	opts := workerOCIOptions(t)
	root := t.TempDir()
	script := `command -v dd >/dev/null
 IFS= read -r value
 test "$value" = interactive-input
 if dd if=/dev/zero of=/tmp/overflow bs=1048576 count=80 2>/dev/null; then exit 71; fi
 rm /tmp/overflow
 printf checked > result`
	if err := RunWorker(context.Background(), root, "/bin/sh", []string{"-ec", script}, opts, strings.NewReader("interactive-input\n"), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "result"))
	if err != nil || string(got) != "checked" {
		t.Fatalf("interactive/storage result: %q %v", got, err)
	}
}

func TestWorkerOCIDeadline(t *testing.T) {
	t.Parallel()
	opts := workerOCIOptions(t)
	opts.Timeout = 20 * time.Second
	root := t.TempDir()
	signal := &workerStartSignal{started: make(chan struct{})}
	started := time.Now()
	err := RunWorker(context.Background(), root, "/bin/sh", []string{"-ec", "printf worker-started; sleep 60; printf late > late.txt"}, opts, nil, signal, io.Discard)
	if err == nil {
		t.Fatal("worker exceeded deadline successfully")
	}
	select {
	case <-signal.started:
	default:
		t.Fatalf("worker timed out before exercising deadline: %v", err)
	}
	if time.Since(started) > 55*time.Second {
		t.Fatal("worker deadline and cleanup budget exceeded")
	}
	if _, err := os.Stat(filepath.Join(root, "late.txt")); !os.IsNotExist(err) {
		t.Fatalf("timed-out output imported: %v", err)
	}
}
