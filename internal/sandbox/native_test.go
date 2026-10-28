package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/process"
	"github.com/supernovae/swarmos/internal/testutil"
)

func TestNativeWorkspaceExcludesControlAndImportsChanges(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	for _, deadline := range []bool{true, false} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeWorkerFixture(t, dir, "source", "original")
			var snapshot string
			failed := errors.New("harness session failed")
			// The timeout also covers snapshot setup, which can exceed a short
			// deadline on slow runners; only the deadline case needs one.
			timeout := time.Minute
			if deadline {
				timeout = 100 * time.Millisecond
			}
			err := withNativeWorkspace(context.Background(), dir, WorkerOptions{Timeout: timeout}, func(ctx context.Context, work, _ string) error {
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	if err := RunNativeValidation(ctx, dir, binary, "cat test.txt; printf changed > test.txt; go version", nil, Dependencies{}, false, &output, &output); err != nil {
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
	args := codexPolicy(binary, work, scratch, true, nil, Dependencies{})
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
	if err := runNativeCommand(ctx, binary, scratch, nativeEnv(scratch, false, Dependencies{}), args, nil, &output, &output); err != nil {
		t.Fatal(err, output.String())
	}
	data, _ := os.ReadFile(filepath.Join(work, "source.txt"))
	if string(data) != "original" {
		t.Fatal("reviewer changed source")
	}
	// A new session must not let a tool survive the harness cancellation.
	args = codexPolicy(binary, work, scratch, false, nil, Dependencies{})
	args = append(args, "sandbox", "-C", work, "-P", "swarm", "--", "/bin/sh", "-c", `test -x /usr/bin/setsid; printf started > started; setsid /bin/sh -c 'sleep 2; printf escaped > escaped' & wait`)
	short, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	err = runNativeCommand(short, binary, scratch, nativeEnv(scratch, false, Dependencies{}), args, nil, &output, &output)
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
	t.Parallel()
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

func TestNativeCodexAgentBuildCacheReuse(t *testing.T) {
	binary := os.Getenv("SWARM_TEST_CODEX_BINARY")
	if binary == "" {
		t.Skip("set SWARM_TEST_CODEX_BINARY to run real Codex sandbox tests")
	}
	binary, err := filepath.EvalSymlinks(binary)
	if err != nil {
		t.Fatal(err)
	}
	root := useUserCache(t)
	repo := cacheTestRepo(t)
	caches, err := agentBuildCaches(t.Context(), repo, resolvedTempDir(t), WorkerOptions{Role: "developer"})
	if err != nil {
		t.Fatal(err)
	}
	validator := mustPrepare(t, ValidatorBuildCache, repo, root, resolvedTempDir(t))
	write := `set -eu; for d in "$GOCACHE" "$CARGO_TARGET_DIR" "$CCACHE_DIR"; do printf run1 > "$d/m"; done`
	read := `set -eu; for d in "$GOCACHE" "$CARGO_TARGET_DIR" "$CCACHE_DIR"; do test "$(cat "$d/m")" = run1; done; test ! -r "$1"; if (printf x > "$1/agent-wrote") 2>/dev/null; then exit 9; fi`
	for _, script := range []string{write, read} {
		work, scratch := resolvedTempDir(t), resolvedTempDir(t)
		args := codexPolicy(binary, work, scratch, false, caches, Dependencies{})
		args = append(args, "sandbox", "-C", work, "-P", "swarm", "--", "/bin/sh", "-c", script, "swarm-cache", validator[0].Dir)
		var output strings.Builder
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		err := runNativeCommand(ctx, binary, scratch, nativeEnv(codexHostHome(scratch), false, Dependencies{}), args, nil, &output, &output)
		cancel()
		if err != nil {
			t.Fatal(err, output.String())
		}
	}
	if _, err := os.Stat(filepath.Join(validator[0].Dir, "agent-wrote")); !os.IsNotExist(err) {
		t.Fatal("agent wrote the validator cache", err)
	}
}

// countingCodex writes a fake Codex that records each version, help and
// canary invocation under dir. The canary fails while dir/fail-canary exists.
func countingCodex(t *testing.T, dir, name, version string) string {
	t.Helper()
	script := `#!/bin/sh
for a; do [ "$a" = swarm-canary ] && canary=1; done
case "$1" in
--version) printf x >> '` + dir + `/version'; echo "codex-cli ` + version + `" ;;
exec) printf x >> '` + dir + `/help'; echo "--ignore-user-config --ignore-rules --ephemeral --skip-git-repo-check" ;;
*) if [ -n "${canary:-}" ]; then
     printf x >> '` + dir + `/canary'
     [ -e '` + dir + `/fail-canary' ] && exit 3
     printf SWARM_NATIVE_BOUNDARY_OK
   fi ;;
esac
`
	path := filepath.Join(dir, name)
	if err := testutil.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func invocations(t *testing.T, dir, kind string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, kind))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(data)
}

func requireLinuxInspection(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("native Codex inspection is qualified on Linux")
	}
	requireLoopbackCanary(t)
}

// requireLoopbackCanary skips when the host denies the loopback socket that the
// mandatory native network canary listens on.
func requireLoopbackCanary(t *testing.T) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		t.Skipf("host denies loopback sockets for the native network canary: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	listener.Close()
}

func TestInspectionReusesInterfaceAndRerunsCanaries(t *testing.T) {
	t.Parallel()
	requireLinuxInspection(t)
	dir := t.TempDir()
	binary := countingCodex(t, dir, "codex", "0.163.0")
	ctx := context.Background()
	session := NewInspectionSession()
	for _, role := range []string{"planner", "reviewer", "developer", "tester"} {
		version, err := session.InspectNativeCodex(ctx, binary, "policy")
		if err != nil || version != "0.163.0" {
			t.Fatalf("%s: %q, %v", role, version, err)
		}
	}
	if got := invocations(t, dir, "version"); got != 1 {
		t.Fatalf("version inspected %d times, want 1", got)
	}
	if got := invocations(t, dir, "help"); got != 1 {
		t.Fatalf("interface inspected %d times, want 1", got)
	}
	if got := invocations(t, dir, "canary"); got != 4 {
		t.Fatalf("boundary canaries ran %d times, want 4", got)
	}
	// A failing canary is rejected even though the interface is cached.
	if err := os.WriteFile(filepath.Join(dir, "fail-canary"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := session.InspectNativeCodex(ctx, binary, "policy"); err == nil || !strings.Contains(err.Error(), "boundary check failed") {
		t.Fatalf("cached inspection accepted a failing canary: %v", err)
	}
	if got := invocations(t, dir, "version"); got != 1 {
		t.Fatalf("version inspected %d times after canary failure, want 1", got)
	}
}

func TestInspectionNilAndIndependentSessionsDoNotShare(t *testing.T) {
	t.Parallel()
	requireLinuxInspection(t)
	dir := t.TempDir()
	binary := countingCodex(t, dir, "codex", "0.163.0")
	ctx := context.Background()
	var none *InspectionSession
	for range 2 {
		if _, err := none.InspectNativeCodex(ctx, binary, "policy"); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if _, err := NewInspectionSession().InspectNativeCodex(ctx, binary, "policy"); err != nil {
			t.Fatal(err)
		}
	}
	if got := invocations(t, dir, "version"); got != 4 {
		t.Fatalf("version inspected %d times, want 4", got)
	}
}

func TestInspectionInvalidatesOnExecutableIdentity(t *testing.T) {
	t.Parallel()
	requireLinuxInspection(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, dir, binary string) string
	}{
		{"path", func(t *testing.T, dir, binary string) string {
			data, err := os.ReadFile(binary)
			if err != nil {
				t.Fatal(err)
			}
			moved := filepath.Join(dir, "codex-copy")
			if err := testutil.WriteExecutable(moved, data, 0700); err != nil {
				t.Fatal(err)
			}
			return moved
		}},
		{"size", func(t *testing.T, dir, binary string) string {
			data, err := os.ReadFile(binary)
			if err != nil {
				t.Fatal(err)
			}
			if err := testutil.WriteExecutable(binary, append(data, []byte("# grown\n")...), 0700); err != nil {
				t.Fatal(err)
			}
			return binary
		}},
		{"mtime", func(t *testing.T, dir, binary string) string {
			later := time.Now().Add(time.Hour)
			if err := os.Chtimes(binary, later, later); err != nil {
				t.Fatal(err)
			}
			return binary
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			binary := countingCodex(t, dir, "codex", "0.163.0")
			session := NewInspectionSession()
			if _, err := session.InspectNativeCodex(ctx, binary, "policy"); err != nil {
				t.Fatal(err)
			}
			changed := tc.change(t, dir, binary)
			if _, err := session.InspectNativeCodex(ctx, changed, "policy"); err != nil {
				t.Fatal(err)
			}
			if got := invocations(t, dir, "version"); got != 2 {
				t.Fatalf("version inspected %d times after %s change, want 2", got, tc.name)
			}
		})
	}
}

func TestInspectionSameSizeSameMtimeReplacement(t *testing.T) {
	t.Parallel()
	requireLinuxInspection(t)
	dir := t.TempDir()
	binary := countingCodex(t, dir, "codex", "0.163.0")
	info, err := os.Stat(binary)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	session := NewInspectionSession()
	if version, err := session.InspectNativeCodex(ctx, binary, "policy"); err != nil || version != "0.163.0" {
		t.Fatal(version, err)
	}
	countingCodex(t, dir, "codex", "0.163.1")
	if err := os.Chtimes(binary, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if replaced, err := os.Stat(binary); err != nil || replaced.Size() != info.Size() || !replaced.ModTime().Equal(info.ModTime()) {
		t.Fatalf("replacement changed size or mtime: %v", err)
	}
	version, err := session.InspectNativeCodex(ctx, binary, "policy")
	if err != nil || version != "0.163.1" {
		t.Fatalf("same-size same-mtime replacement reused %q, %v", version, err)
	}
	if got := invocations(t, dir, "version"); got != 2 {
		t.Fatalf("version inspected %d times, want 2", got)
	}
}

func TestInspectionSymlinkRetargeting(t *testing.T) {
	t.Parallel()
	requireLinuxInspection(t)
	dir := t.TempDir()
	first := countingCodex(t, dir, "codex-a", "0.163.0")
	second := countingCodex(t, dir, "codex-b", "0.164.0")
	link := filepath.Join(dir, "codex")
	if err := os.Symlink(first, link); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	session := NewInspectionSession()
	if version, err := session.InspectNativeCodex(ctx, link, "policy"); err != nil || version != "0.163.0" {
		t.Fatal(version, err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, link); err != nil {
		t.Fatal(err)
	}
	if version, err := session.InspectNativeCodex(ctx, link, "policy"); err != nil || version != "0.164.0" {
		t.Fatalf("retargeted symlink reused %q, %v", version, err)
	}
	if got := invocations(t, dir, "version"); got != 2 {
		t.Fatalf("version inspected %d times, want 2", got)
	}
}

func TestInspectionPartitionsByDriverAndPolicy(t *testing.T) {
	t.Parallel()
	binary := filepath.Join(t.TempDir(), "harness")
	if err := testutil.WriteExecutable(binary, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	base := &contract.SwarmConfig{DefaultAgent: "codex", Roles: map[string]string{"tester": "codex"}, Agents: map[string]contract.AgentConfig{"codex": {Binary: "codex"}, "claude": {Binary: "claude"}}}
	same := &contract.SwarmConfig{DefaultAgent: "codex", Roles: map[string]string{"tester": "codex"}, Agents: map[string]contract.AgentConfig{"claude": {Binary: "claude"}, "codex": {Binary: "codex"}}}
	changed := *same
	changed.DefaultAgent = "claude"
	policy := func(cfg *contract.SwarmConfig) string {
		digest, err := InspectionPolicy(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return digest
	}
	session := NewInspectionSession()
	calls := 0
	inspect := func(context.Context) (string, error) { calls++; return "1", nil }
	ctx := context.Background()
	for _, step := range []struct {
		driver, policy string
		calls          int
	}{
		{"claude", policy(base), 1},
		{"claude", policy(same), 1}, // identical values in a different allocation
		{"omp", policy(same), 2},
		{"claude", policy(&changed), 3},
		{"claude", policy(base), 3},
	} {
		if _, err := session.reuse(ctx, step.driver, binary, step.policy, inspect); err != nil {
			t.Fatal(err)
		}
		if calls != step.calls {
			t.Fatalf("%s/%s: %d inspections, want %d", step.driver, step.policy[:8], calls, step.calls)
		}
	}
}

func TestInspectionFailuresAreNotCached(t *testing.T) {
	t.Parallel()
	binary := filepath.Join(t.TempDir(), "harness")
	if err := testutil.WriteExecutable(binary, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	session := NewInspectionSession()
	calls := 0
	fail := func(context.Context) (string, error) { calls++; return "", errors.New("unqualified") }
	succeed := func(context.Context) (string, error) { calls++; return "1", nil }
	if _, err := session.reuse(ctx, "omp", binary, "p", fail); err == nil {
		t.Fatal("failure accepted")
	}
	if _, err := session.reuse(ctx, "omp", binary, "p", succeed); err != nil || calls != 2 {
		t.Fatalf("failed inspection was retained: calls=%d err=%v", calls, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	other := filepath.Join(t.TempDir(), "other")
	if err := testutil.WriteExecutable(other, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := session.reuse(cancelled, "omp", other, "p", func(ctx context.Context) (string, error) { calls++; return "", ctx.Err() }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := session.reuse(ctx, "omp", other, "p", succeed); err != nil || calls != 4 {
		t.Fatalf("cancelled inspection was retained: calls=%d err=%v", calls, err)
	}
}

func TestInspectionRejectsMissingAndMutatedExecutables(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	session := NewInspectionSession()
	calls := 0
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := session.reuse(ctx, "omp", missing, "p", func(context.Context) (string, error) { calls++; return "1", nil }); err == nil || calls != 0 {
		t.Fatalf("missing executable inspected: calls=%d err=%v", calls, err)
	}
	unreadable := filepath.Join(t.TempDir(), "unreadable")
	if err := testutil.WriteExecutable(unreadable, []byte("#!/bin/sh\n"), 0100); err != nil {
		t.Fatal(err)
	}
	if f, err := os.Open(unreadable); err == nil {
		f.Close()
		t.Log("running with privileges that read mode 0100 files; skipping unreadable case")
	} else if _, err := session.reuse(ctx, "omp", unreadable, "p", func(context.Context) (string, error) { calls++; return "1", nil }); err == nil || calls != 0 {
		t.Fatalf("unreadable executable inspected: calls=%d err=%v", calls, err)
	}
	binary := filepath.Join(t.TempDir(), "harness")
	if err := testutil.WriteExecutable(binary, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	mutate := func(context.Context) (string, error) {
		calls++
		return "1", testutil.WriteExecutable(binary, []byte("#!/bin/sh\nexit 0\n"), 0700)
	}
	if _, err := session.reuse(ctx, "omp", binary, "p", mutate); err == nil || !strings.Contains(err.Error(), "changed during inspection") {
		t.Fatalf("mutation during inspection accepted: %v", err)
	}
	if _, err := session.reuse(ctx, "omp", binary, "p", func(context.Context) (string, error) { calls++; return "1", nil }); err != nil || calls != 2 {
		t.Fatalf("mutated inspection was retained: calls=%d err=%v", calls, err)
	}
}

func TestInspectionConcurrentRequestsShareOneInspection(t *testing.T) {
	t.Parallel()
	binary := filepath.Join(t.TempDir(), "harness")
	if err := testutil.WriteExecutable(binary, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	session := NewInspectionSession()
	var mu sync.Mutex
	calls := 0
	inspect := func(context.Context) (string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		return "1", nil
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := session.reuse(context.Background(), "omp", binary, "p", inspect)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("concurrent requests inspected %d times, want 1", calls)
	}
}

func TestInspectionDelegatedHarnessReuse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	binary := filepath.Join(dir, "omp")
	script := `#!/bin/sh
case "$1" in
--version) printf x >> '` + dir + `/version'; echo "omp/18.12.6" ;;
--help) echo "--mode --no-tools --no-extensions --no-rules --config --no-session" ;;
esac
`
	if err := testutil.WriteExecutable(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	session := NewInspectionSession()
	for range 3 {
		if version, err := session.InspectDelegatedHarness(ctx, "omp", binary, "p"); err != nil || version != "18.12.6" {
			t.Fatal(version, err)
		}
	}
	if _, err := InspectDelegatedHarness(ctx, "omp", binary); err != nil {
		t.Fatal(err)
	}
	if got := invocations(t, dir, "version"); got != 2 {
		t.Fatalf("version inspected %d times, want 2 (one session, one direct)", got)
	}
}

// A run reusing a doctor or preflight inspection must still see the optional
// session flags the harness lists, and only those listed exactly.
func TestInspectionDelegatedHarnessKeepsOptionalFlags(t *testing.T) {
	t.Parallel()
	claudeBase := "--restricted --strict-mcp-config --tools --settings --permission-mode"
	openCodeBase := "--standalone --server --continue --model --agent --format --title"
	for _, tc := range []struct {
		name, driver, version, help, flag string
		listed                            bool
	}{
		{"claude listed", "claude", "2.1.311 (Claude Code)", claudeBase + " --session-id <uuid>", "--session-id", true},
		{"claude absent", "claude", "2.1.311 (Claude Code)", claudeBase, "--session-id", false},
		{"claude prefix only", "claude", "2.1.311 (Claude Code)", claudeBase + " --session-id-file", "--session-id", false},
		{"opencode listed", "opencode", "opencode v2.1.2", openCodeBase + " --session=<id>", "--session", true},
		{"opencode absent", "opencode", "opencode v2.1.2", openCodeBase, "--session", false},
		{"opencode prefix only", "opencode", "opencode v2.1.2", openCodeBase + " --sessions", "--session", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			binary := filepath.Join(dir, tc.driver)
			script := `#!/bin/sh
case "$1" in
--version) printf x >> '` + dir + `/version'; echo "` + tc.version + `" ;;
*) echo "` + tc.help + `" ;;
esac
`
			if err := testutil.WriteExecutable(binary, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			session := NewInspectionSession()
			// Doctor and preflight warm the cache through the version-only method.
			if _, err := session.InspectDelegatedHarness(ctx, tc.driver, binary, "p"); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				result, err := session.inspectDelegated(ctx, tc.driver, binary, "p")
				if err != nil {
					t.Fatal(err)
				}
				if got := helpLists(result.help, tc.flag); got != tc.listed {
					t.Fatalf("cached %s listed = %v, want %v", tc.flag, got, tc.listed)
				}
			}
			if got := invocations(t, dir, "version"); got != 1 {
				t.Fatalf("version inspected %d times, want 1", got)
			}
			var none *InspectionSession
			result, err := none.inspectDelegated(ctx, tc.driver, binary, "p")
			if err != nil {
				t.Fatal(err)
			}
			if got := helpLists(result.help, tc.flag); got != tc.listed {
				t.Fatalf("uncached %s listed = %v, want %v", tc.flag, got, tc.listed)
			}
		})
	}
}

// Optional flags never relax the base interface: a harness missing a required
// flag fails and is not retained, and extra run flags are still required.
func TestInspectionDelegatedHarnessRequiresBaseInterface(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	binary := filepath.Join(dir, "claude")
	help := filepath.Join(dir, "help")
	if err := os.WriteFile(help, []byte("--restricted --strict-mcp-config --tools --settings --session-id\n"), 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
case "$1" in
--version) printf x >> '` + dir + `/version'; echo "2.1.311 (Claude Code)" ;;
*) cat '` + help + `' ;;
esac
`
	if err := testutil.WriteExecutable(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	session := NewInspectionSession()
	if _, err := session.inspectDelegated(ctx, "claude", binary, "p"); err == nil || !strings.Contains(err.Error(), "--permission-mode") {
		t.Fatalf("missing required interface accepted: %v", err)
	}
	if _, err := session.inspectDelegated(ctx, "claude", binary, "p"); err == nil {
		t.Fatal("failed inspection was retained")
	}
	if got := invocations(t, dir, "version"); got != 2 {
		t.Fatalf("version inspected %d times, want 2", got)
	}
	if err := os.WriteFile(help, []byte("--restricted --strict-mcp-config --tools --settings --permission-mode --session-id\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := inspectDelegatedHarness(ctx, "claude", binary, []string{"--effort"}); err == nil || !strings.Contains(err.Error(), "--effort") {
		t.Fatalf("missing extra interface accepted: %v", err)
	}
	if result, err := session.inspectDelegated(ctx, "claude", binary, "p"); err != nil || !helpLists(result.help, "--session-id") {
		t.Fatalf("qualified inspection = %+v, %v", result, err)
	}
}

func TestInspectionValidationPathsStayUncached(t *testing.T) {
	t.Parallel()
	requireLinuxInspection(t)
	dir := t.TempDir()
	binary := countingCodex(t, dir, "codex", "0.163.0")
	ctx := context.Background()
	session := NewInspectionSession()
	if _, err := session.InspectNativeCodex(ctx, binary, "policy"); err != nil {
		t.Fatal(err)
	}
	cfg := &contract.SwarmConfig{DefaultAgent: "codex", Agents: map[string]contract.AgentConfig{"codex": {Binary: binary}}}
	work := t.TempDir()
	checks := []struct {
		name string
		run  func() error
	}{
		{"CheckNativeCodex", func() error { return CheckNativeCodex(ctx, binary) }},
		{"CheckNativeValidation", func() error { return CheckNativeValidation(ctx, cfg) }},
		{"RunNativeValidation", func() error {
			return RunNativeValidation(ctx, work, binary, "true", nil, Dependencies{}, false, io.Discard, io.Discard)
		}},
	}
	for i, check := range checks {
		if err := check.run(); err != nil {
			t.Fatalf("%s: %v", check.name, err)
		}
		if got := invocations(t, dir, "version"); got != i+2 {
			t.Fatalf("%s reused version inspection: %d, want %d", check.name, got, i+2)
		}
		if got := invocations(t, dir, "canary"); got != i+2 {
			t.Fatalf("%s skipped boundary canaries: %d, want %d", check.name, got, i+2)
		}
	}
	// A later interface or canary failure fails validation despite the cache.
	if err := os.WriteFile(filepath.Join(dir, "fail-canary"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, check := range checks {
		if err := check.run(); err == nil {
			t.Fatalf("%s accepted a failing canary", check.name)
		}
	}
	if err := os.Remove(filepath.Join(dir, "fail-canary")); err != nil {
		t.Fatal(err)
	}
	countingCodex(t, dir, "codex", "0.162.0")
	for _, check := range checks {
		if err := check.run(); err == nil || !strings.Contains(err.Error(), "0.162.0") && !strings.Contains(err.Error(), "version") {
			t.Fatalf("%s accepted an unqualified interface: %v", check.name, err)
		}
	}
}
