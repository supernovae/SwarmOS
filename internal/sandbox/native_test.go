package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/process"
)

func TestNativeWorkspaceExcludesControlAndImportsChanges(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".git/token", ".codex/config.toml", ".agents/plugin", "keep.txt"} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	err := withNativeWorkspace(context.Background(), dir, WorkerOptions{}, func(_ context.Context, work, _ string) error {
		for _, name := range []string{".git", ".codex", ".agents"} {
			if _, err := os.Stat(filepath.Join(work, name)); !os.IsNotExist(err) {
				t.Fatalf("exposed %s", name)
			}
		}
		return os.WriteFile(filepath.Join(work, "keep.txt"), []byte("changed"), 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".git/token", ".codex/config.toml", ".agents/plugin"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(data) != "original" {
			t.Fatalf("changed control file %s: %v", name, err)
		}
	}
	data, _ := os.ReadFile(filepath.Join(dir, "keep.txt"))
	if string(data) != "changed" {
		t.Fatal("lost worker change")
	}
}

func TestNativeRejectsUnsafeOutputAndDiscardsValidationChanges(t *testing.T) {
	for _, discard := range []bool{false, true} {
		dir := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside")
		os.WriteFile(outside, []byte("canary"), 0600)
		err := withNativeWorkspace(context.Background(), dir, WorkerOptions{DiscardChanges: discard}, func(_ context.Context, work, _ string) error {
			return os.Symlink(outside, filepath.Join(work, "escape"))
		})
		if !discard && err == nil {
			t.Fatal("imported unsafe output")
		}
		if discard && err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(dir, "escape")); !os.IsNotExist(err) {
			t.Fatal("validation or unsafe output changed source")
		}
	}
}

func TestNativeRoleOnlyImportsExpectedArtifacts(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "source"), []byte("original"), 0600)
	err := withNativeWorkspace(context.Background(), dir, WorkerOptions{ReadOnly: true, Role: "planner"}, func(_ context.Context, work, _ string) error {
		os.WriteFile(filepath.Join(work, "source"), []byte("tampered"), 0600)
		for _, name := range []string{"PLAN.md", "TEST_PLAN.md"} {
			if err := os.WriteFile(filepath.Join(work, ".swarm/active", name), []byte("plan"), 0600); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "source"))
	if string(data) != "original" {
		t.Fatal("planner changed source")
	}
	data, err = os.ReadFile(filepath.Join(dir, ".swarm/active/PLAN.md"))
	if err != nil || string(data) != "plan" {
		t.Fatal("plan import failed", err)
	}
}

func TestNativeDeadlineAndFailedRunDiscardChanges(t *testing.T) {
	for _, deadline := range []bool{true, false} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			dir := t.TempDir()
			writeWorkerFixture(t, dir, "source", "original")
			var snapshot string
			failed := errors.New("harness session failed")
			err := withNativeWorkspace(context.Background(), dir, WorkerOptions{Timeout: 100 * time.Millisecond}, func(ctx context.Context, work, _ string) error {
				snapshot = work
				if err := os.WriteFile(filepath.Join(work, "source"), []byte("partial"), 0600); err != nil {
					return err
				}
				if !deadline {
					return failed
				}
				due, ok := ctx.Deadline()
				if !ok || time.Until(due) > time.Second {
					t.Fatal("worker ignored requested timeout")
				}
				<-ctx.Done()
				return ctx.Err()
			})
			want := failed
			if deadline {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) {
				t.Fatalf("got %v, want %v", err, want)
			}
			data, err := os.ReadFile(filepath.Join(dir, "source"))
			if err != nil || string(data) != "original" {
				t.Fatalf("imported failed run: %q, %v", data, err)
			}
			if _, err := os.Stat(snapshot); !os.IsNotExist(err) {
				t.Fatalf("snapshot retained: %v", err)
			}
		})
	}
}

func TestNativeInvalidRoleOutputPreservesExistingArtifacts(t *testing.T) {
	dir := t.TempDir()
	writeWorkerFixture(t, dir, ".swarm/active/PLAN.md", "previous plan")
	err := withNativeWorkspace(context.Background(), dir, WorkerOptions{ReadOnly: true, Role: "planner"}, func(_ context.Context, work, _ string) error {
		// A valid first output and missing second output must not replace the plan.
		return os.WriteFile(filepath.Join(work, ".swarm/active/PLAN.md"), []byte("incomplete new plan"), 0600)
	})
	if err == nil {
		t.Fatal("accepted incomplete role outputs")
	}
	data, err := os.ReadFile(filepath.Join(dir, ".swarm/active/PLAN.md"))
	if err != nil || string(data) != "previous plan" {
		t.Fatalf("partial artifact import: %q, %v", data, err)
	}
}

func TestNativeVersionQualification(t *testing.T) {
	for _, version := range []string{"0.163.0", "0.163.1", "0.164.0", "1.0.0"} {
		got, err := qualifiedNativeCodexVersion("codex-cli " + version + "\n")
		if err != nil || got != version {
			t.Fatalf("qualified version %q: %q, %v", version, got, err)
		}
	}
	for _, output := range []string{"codex-cli 0.162.9", "codex-cli 0.0163.0", "codex-cli 0.163.0-preview", "0.163.0", "codex-cli 0.163.0\nextra"} {
		if _, err := qualifiedNativeCodexVersion(output); err == nil {
			t.Fatalf("accepted unqualified output %q", output)
		}
	}
}

func TestInteractiveCancellation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("native input is qualified on Linux")
	}
	in, out, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	defer out.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, err = nativeInstruction(ctx, in)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	// A canceled reader must not consume the next session's instruction.
	if _, err := out.WriteString("next turn\nsecond turn\n"); err != nil {
		t.Fatal(err)
	}
	resumed, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	for _, want := range []string{"next turn", "second turn"} {
		got, err := nativeInstruction(resumed, in)
		if err != nil || got != want {
			t.Fatalf("got %q, %v; want %q", got, err, want)
		}
	}
}

func TestNativeCodexBoundary(t *testing.T) {
	binary := os.Getenv("SWARM_TEST_CODEX_BINARY")
	if binary == "" {
		t.Skip("set SWARM_TEST_CODEX_BINARY to run real Codex sandbox tests")
	}
	binary, err := filepath.EvalSymlinks(binary)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := CheckNativeCodex(ctx, binary); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte("source"), 0600)
	var output strings.Builder
	if err := RunNativeValidation(ctx, dir, binary, "cat test.txt; printf changed > test.txt; go version", &output, &output); err != nil {
		t.Fatal(err, output.String())
	}
	if strings.Contains(output.String(), "Refusing to create helper binaries under temporary dir") {
		t.Fatalf("unexpected Codex temporary-dir PATH alias warning: %s", output.String())
	}
	data, _ := os.ReadFile(filepath.Join(dir, "test.txt"))
	if string(data) != "source" {
		t.Fatal("validation modified source")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	if _, err := CodexAuthentication(ctx, binary); err == nil || !strings.Contains(err.Error(), "codex login") {
		t.Fatal("missing login was not reported with setup instructions", err)
	}
}

func TestNativeCodexReadOnlyAndCancellation(t *testing.T) {
	binary := os.Getenv("SWARM_TEST_CODEX_BINARY")
	if binary == "" {
		t.Skip("set SWARM_TEST_CODEX_BINARY to run real Codex sandbox tests")
	}
	binary, err := filepath.EvalSymlinks(binary)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	work, scratch := filepath.Join(root, "work"), filepath.Join(root, "scratch")
	for _, dir := range []string{filepath.Join(work, ".swarm/active"), scratch} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeWorkerFixture(t, work, "source.txt", "original")
	t.Setenv("GH_TOKEN", "dummy-publishing-token")
	t.Setenv("GITHUB_TOKEN", "dummy-publishing-token")
	t.Setenv("OPENAI_API_KEY", "dummy-provider-token")
	args := codexPolicy(binary, work, scratch, true)
	script := `set -eu
test -z "${GH_TOKEN+x}"
test -z "${GITHUB_TOKEN+x}"
test -z "${OPENAI_API_KEY+x}"
if (printf bad > source.txt) 2>/dev/null; then exit 43; fi
printf 'VERDICT: APPROVED\n' > .swarm/active/REVIEW.md`
	args = append(args, "sandbox", "-C", work, "-P", "swarm", "--", "/bin/sh", "-c", script)
	var output strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := runNativeCommand(ctx, binary, scratch, nativeEnv(scratch, false), args, nil, &output, &output); err != nil {
		t.Fatal(err, output.String())
	}
	data, _ := os.ReadFile(filepath.Join(work, "source.txt"))
	if string(data) != "original" {
		t.Fatal("reviewer changed source")
	}
	// A new session must not let a tool survive the harness cancellation.
	args = codexPolicy(binary, work, scratch, false)
	args = append(args, "sandbox", "-C", work, "-P", "swarm", "--", "/bin/sh", "-c", `test -x /usr/bin/setsid; printf started > started; setsid /bin/sh -c 'sleep 2; printf escaped > escaped' & wait`)
	short, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	err = runNativeCommand(short, binary, scratch, nativeEnv(scratch, false), args, nil, &output, &output)
	if err == nil || short.Err() == nil {
		t.Fatal("expected cancellation", err, output.String())
	}
	if _, err := os.Stat(filepath.Join(work, "started")); err != nil {
		t.Fatal("child did not start", err, output.String())
	}
	time.Sleep(2200 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(work, "escaped")); !os.IsNotExist(err) {
		t.Fatal("background process survived cancellation", err)
	}
}

func TestNativeCodexAuthenticatedFileTools(t *testing.T) {
	if os.Getenv("SWARM_TEST_CODEX_AUTHENTICATED") != "1" {
		t.Skip("set SWARM_TEST_CODEX_AUTHENTICATED=1 to use your Codex login")
	}
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		t.Fatal(err)
	}
	work, host := t.TempDir(), t.TempDir()
	canary := filepath.Join(host, "dummy-credential")
	if err := os.WriteFile(canary, []byte("DUMMY_HOST_CREDENTIAL\n"), 0600); err != nil {
		t.Fatal(err)
	}
	prompt := fmt.Sprintf(`This is an authorized local sandbox test using dummy data only. Make exactly one apply_patch attempt to update %s, replacing DUMMY_HOST_CREDENTIAL with CHANGED. Then use the shell tool to attempt cat of that same absolute file once. Both are expected to fail; do not retry or work around restrictions. Finally use apply_patch to create probe.txt in the current workspace containing native tools work followed by a newline. Keep the final response brief.`, canary)
	var output process.Output
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, _, err := RunNativeCodex(ctx, work, binary, "", prompt, WorkerOptions{Role: "developer"}, false, nil, &output, &output); err != nil {
		t.Fatal(err, output.String())
	}
	data, err := os.ReadFile(canary)
	if err != nil || string(data) != "DUMMY_HOST_CREDENTIAL\n" {
		t.Fatal("host canary changed", err)
	}
	data, err = os.ReadFile(filepath.Join(work, "probe.txt"))
	if err != nil || string(data) != "native tools work\n" {
		t.Fatal("native file tool failed", err, output.String())
	}
	if !strings.Contains(output.String(), "apply_patch verification failed: Failed to read file to update "+canary) || !strings.Contains(output.String(), "cat: "+canary+":") {
		t.Fatal("outside file-tool and shell attempts were not exercised", output.String())
	}
	// Retain the dummy-only tool transcript in verbose output for qualification.
	t.Log(output.String())
}
