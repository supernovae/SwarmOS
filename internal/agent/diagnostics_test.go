package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/process"
)

const diagnosticsTestToken = "ghp_test_diagnostics_secret"

var diagnosticsTestBasic = base64.StdEncoding.EncodeToString([]byte("x-access-token:" + diagnosticsTestToken))

// TestMain keeps every failed run in this package away from the operator's
// real state directory and primes the coordinator token used for redaction.
func TestMain(m *testing.M) {
	// Helper subprocesses exit inside m.Run, so the state directory is created
	// lazily by the first saved log rather than here.
	state := filepath.Join(os.TempDir(), fmt.Sprintf("swarm-agent-test-state-%d", os.Getpid()))
	os.Setenv("XDG_STATE_HOME", state)
	os.Setenv("SWARM_GITHUB_TOKEN", diagnosticsTestToken)
	os.Unsetenv(gitcredential.TokenCommandEnv)
	if _, err := gitcredential.NewRedaction(); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(state)
	os.Exit(code)
}

// useDiagnosticsState points diagnostics at a fresh, symlink-free state root.
func useDiagnosticsState(t *testing.T) string {
	t.Helper()
	state, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state = filepath.Join(state, "state")
	previous := diagnosticsStateDir
	diagnosticsStateDir = func() (string, error) { return state, nil }
	t.Cleanup(func() { diagnosticsStateDir = previous })
	return state
}

func fixedDiagnosticsName(t *testing.T, name string) {
	t.Helper()
	previous := diagnosticsNewName
	diagnosticsNewName = func() string { return name }
	t.Cleanup(func() { diagnosticsNewName = previous })
}

var savedNote = regexp.MustCompile(`diagnostics saved to (/\S+) \((\d+) bytes\)$`)

func savedDiagnosticsPath(t *testing.T, text string) (string, []byte) {
	t.Helper()
	match := savedNote.FindStringSubmatch(text)
	if match == nil {
		t.Fatalf("no diagnostics pointer in %q", text)
	}
	data, err := os.ReadFile(match[1])
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(len(data)) != match[2] {
		t.Fatalf("advertised %s bytes, file has %d", match[2], len(data))
	}
	return match[1], data
}

func TestDiagnosticsOutputHelper(t *testing.T) {
	if os.Getenv("SWARM_DIAGNOSTICS_HELPER") == "" {
		return
	}
	switch os.Getenv("SWARM_DIAGNOSTICS_HELPER") {
	case "fail":
		fmt.Fprint(os.Stdout, "EARLY-PREFIX-MARKER\n")
		fmt.Fprint(os.Stdout, strings.Repeat("filler line\n", 20000))
		fmt.Fprint(os.Stdout, "stdout token "+diagnosticsTestToken+"\n")
		fmt.Fprint(os.Stderr, "stderr basic "+diagnosticsTestBasic+"\n")
		fmt.Fprint(os.Stderr, "escape \x1b[31mred\x1b]52;c;clip\a\r\n")
		fmt.Fprint(os.Stdout, "FINAL-FAILURE-SENTINEL\n")
		os.Exit(7)
	case "silent":
		os.Exit(3)
	case "ok":
		fmt.Fprint(os.Stdout, "fine\n")
	}
	os.Exit(0)
}

func diagnosticsAdapter() *CLIAdapter {
	return &CLIAdapter{agentName: "fake", binary: os.Args[0], prefixArgs: []string{"-test.run=^TestDiagnosticsOutputHelper$", "--"}, envAllow: []string{"SWARM_DIAGNOSTICS_HELPER"}}
}

func diagnosticsFiles(t *testing.T, state string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(state, "swarm", "diagnostics"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if diagnosticsFileName.MatchString(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	return names
}

func TestFailingHarnessSavesBoundedRedactedDiagnostics(t *testing.T) {
	state := useDiagnosticsState(t)
	t.Setenv("SWARM_DIAGNOSTICS_HELPER", "fail")
	worktree := t.TempDir()
	result, err := diagnosticsAdapter().Run(t.Context(), RunRequest{Role: "developer", Mode: "headless", WorktreePath: worktree, TaskPrompt: "private-prompt"})
	if err == nil || result == nil || result.ExitCode != 7 {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	if !strings.HasPrefix(err.Error(), "developer agent failed (exit 7; harness diagnostics withheld); diagnostics saved to ") {
		t.Fatal(err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatal("exit cause was lost")
	}
	path, data := savedDiagnosticsPath(t, err.Error())
	if filepath.Dir(path) != filepath.Join(state, "swarm", "diagnostics") || strings.HasPrefix(path, worktree) {
		t.Fatalf("diagnostics saved at %s", path)
	}
	text := string(data)
	if len(data) > diagnosticsMaxBytes || !strings.HasPrefix(text, diagnosticsTruncated) {
		t.Fatalf("file is %d bytes, header %q", len(data), text[:min(len(text), 40)])
	}
	for _, want := range []string{"FINAL-FAILURE-SENTINEL", "stdout token [REDACTED]", "stderr basic [REDACTED]", `escape \x1b[31mred\x1b]52;c;clip\x07\x0d`} {
		if !strings.Contains(text, want) {
			t.Fatalf("diagnostics lack %q", want)
		}
	}
	for _, leaked := range []string{"EARLY-PREFIX-MARKER", diagnosticsTestToken, diagnosticsTestBasic, "\x1b", "\a", "\r"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("diagnostics retained %q", leaked)
		}
	}
	encoded, _ := json.Marshal(result)
	for _, text := range []string{err.Error(), string(encoded)} {
		for _, leaked := range []string{"FINAL-FAILURE-SENTINEL", "filler", "private-prompt", diagnosticsTestToken, "\x1b"} {
			if strings.Contains(text, leaked) {
				t.Fatalf("caller-visible output leaked %q", leaked)
			}
		}
	}
	if result.Output != "" || result.Command != nil || result.transcript != nil {
		t.Fatal("failed result retained a transcript")
	}
}

func TestDiagnosticsWithoutOutputOrFailure(t *testing.T) {
	state := useDiagnosticsState(t)
	t.Setenv("SWARM_DIAGNOSTICS_HELPER", "silent")
	_, err := diagnosticsAdapter().Run(t.Context(), RunRequest{Role: "planner", Mode: "headless", WorktreePath: t.TempDir()})
	if err == nil || err.Error() != "planner agent failed (exit 3; harness diagnostics withheld); no harness output to save" {
		t.Fatal(err)
	}
	t.Setenv("SWARM_DIAGNOSTICS_HELPER", "ok")
	if _, err := diagnosticsAdapter().Run(t.Context(), RunRequest{Role: "planner", Mode: "headless", WorktreePath: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if files := diagnosticsFiles(t, state); len(files) != 0 {
		t.Fatalf("logs saved without failing output: %v", files)
	}
}

// Interactive runs keep streaming harness output live to the operator's own
// terminal; only the coordinator error is restricted to the saved path.
func TestInteractiveFailureStreamsAndSavesDiagnostics(t *testing.T) {
	useDiagnosticsState(t)
	t.Setenv("SWARM_DIAGNOSTICS_HELPER", "fail")
	capture := func(target **os.File) func() string {
		read, write, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		previous := *target
		*target = write
		streamed := make(chan []byte)
		go func() {
			data, _ := io.ReadAll(read)
			streamed <- data
		}()
		return func() string {
			*target = previous
			write.Close()
			return string(<-streamed)
		}
	}
	stdout, stderr := capture(&os.Stdout), capture(&os.Stderr)
	_, runErr := diagnosticsAdapter().Run(t.Context(), RunRequest{Role: "developer", Mode: "interactive", WorktreePath: t.TempDir()})
	liveOut, liveErr := stdout(), stderr()
	if !strings.Contains(liveOut, "EARLY-PREFIX-MARKER") || !strings.HasSuffix(liveOut, "FINAL-FAILURE-SENTINEL\n") {
		t.Fatal("interactive stdout was not streamed unchanged")
	}
	if liveErr != "stderr basic "+diagnosticsTestBasic+"\nescape \x1b[31mred\x1b]52;c;clip\a\r\n" {
		t.Fatalf("interactive stderr was not streamed unchanged: %q", liveErr)
	}
	_, data := savedDiagnosticsPath(t, runErr.Error())
	if !strings.Contains(string(data), "FINAL-FAILURE-SENTINEL") || strings.Contains(runErr.Error(), "SENTINEL") {
		t.Fatal(runErr)
	}
}

func TestEscapeDiagnostics(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain\ttext\nnext", "plain\ttext\nnext"},
		{"csi \x1b[2J", `csi \x1b[2J`},
		{"osc52 \x1b]52;c;aGk=\a", `osc52 \x1b]52;c;aGk=\x07`},
		{"cr\roverwrite", `cr\x0doverwrite`},
		{"nul\x00del\x7f", `nul\x00del\x7f`},
		{"invalid \xff\xfe", `invalid \xff\xfe`},
		{"bidi \u202eevil\u2066", "bidi \\u202eevil\\u2066"},
		{"c1 \u009b", `c1 \u009b`},
		{`back\slash`, `back\\slash`},
		{"unicode \u00e9 \u2713", "unicode \u00e9 \u2713"},
	}
	for _, test := range tests {
		if got := escapeDiagnostics(test.in); got != test.want {
			t.Errorf("escapeDiagnostics(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}

func TestRenderDiagnosticsBounds(t *testing.T) {
	redaction, err := gitcredential.NewRedaction()
	if err != nil {
		t.Fatal(err)
	}
	// A credential cut by the tail boundary leaves only an unmatched suffix.
	split := []byte(diagnosticsTestBasic[5:] + " rest\nkept line\n")
	got := string(renderDiagnostics(split, true, redaction))
	if got != diagnosticsTruncated+"kept line\n" {
		t.Fatalf("split secret render = %q", got)
	}
	// Escape expansion beyond the cap keeps the newest bytes within the limit.
	got = string(renderDiagnostics([]byte(strings.Repeat("\x1b", diagnosticsMaxBytes)+"END"), false, redaction))
	if len(got) != diagnosticsMaxBytes || !strings.HasPrefix(got, diagnosticsTruncated) || !strings.HasSuffix(got, `\x1bEND`) {
		t.Fatalf("expanded render len %d", len(got))
	}
	got = string(renderDiagnostics([]byte("first line intact\n"), false, redaction))
	if got != "first line intact\n" {
		t.Fatalf("untruncated render = %q", got)
	}
}

func tailWith(text string) *process.Tail {
	tail := newDiagnosticsTail()
	_, _ = tail.Write([]byte(text))
	return tail
}

func TestDiagnosticsFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	state := useDiagnosticsState(t)
	path, _, err := saveDiagnostics(t.TempDir(), tailWith("output\n"))
	if err != nil {
		t.Fatal(err)
	}
	for dir := filepath.Dir(path); dir != filepath.Dir(state); dir = filepath.Dir(dir) {
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s: mode %v, %v", dir, info.Mode().Perm(), err)
		}
	}
	for _, file := range []string{path, filepath.Join(filepath.Dir(path), ".lock")} {
		info, err := os.Lstat(file)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v, %v", file, info.Mode(), err)
		}
	}
}

// errDiagnosticsSecret carries text that must never reach coordinator output.
var errDiagnosticsSecret = errors.New("PRIVATE-STORAGE-ERROR " + diagnosticsTestToken + " \x1b]52;c;eA==\a")

func TestDiagnosticsRefusesUnsafeLocations(t *testing.T) {
	const name = "failure-0123456789abcdef0123456789abcdef.log"
	tests := map[string]struct {
		want  diagnosticsUnavailable
		setup func(t *testing.T, state, worktree string) string
	}{
		"state lookup error": {unavailableStateDir, func(t *testing.T, _, _ string) string {
			diagnosticsStateDir = func() (string, error) { return "", errDiagnosticsSecret }
			return ""
		}},
		"symlink at target": {unavailableWrite, func(t *testing.T, state, _ string) string {
			dir := filepath.Join(state, "swarm", "diagnostics")
			mustMkdir(t, dir)
			outside := filepath.Join(t.TempDir(), "sentinel")
			mustWrite(t, outside, "sentinel")
			mustSymlink(t, outside, filepath.Join(dir, name))
			return outside
		}},
		"symlinked diagnostics directory": {unavailablePrivate, func(t *testing.T, state, _ string) string {
			mustMkdir(t, filepath.Join(state, "swarm"))
			outside := t.TempDir()
			mustSymlink(t, outside, filepath.Join(state, "swarm", "diagnostics"))
			return filepath.Join(outside, name)
		}},
		"fifo at target": {unavailableWrite, func(t *testing.T, state, _ string) string {
			if runtime.GOOS == "windows" {
				t.Skip("no FIFOs")
			}
			dir := filepath.Join(state, "swarm", "diagnostics")
			mustMkdir(t, dir)
			if err := syscall.Mkfifo(filepath.Join(dir, name), 0o600); err != nil {
				t.Fatal(err)
			}
			return ""
		}},
		"invalid generated name": {unavailableWrite, func(t *testing.T, _, _ string) string {
			diagnosticsNewName = func() string { return "../escape.log" }
			return ""
		}},
		"shared directory": {unavailablePrivate, func(t *testing.T, state, _ string) string {
			if runtime.GOOS == "windows" {
				t.Skip("POSIX modes")
			}
			dir := filepath.Join(state, "swarm", "diagnostics")
			mustMkdir(t, dir)
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			return ""
		}},
		"inside worktree": {unavailableUnsafe, func(t *testing.T, _, worktree string) string {
			diagnosticsStateDir = func() (string, error) { return filepath.Join(worktree, ".state"), nil }
			return ""
		}},
		"relative state": {unavailableUnsafe, func(t *testing.T, _, _ string) string {
			diagnosticsStateDir = func() (string, error) { return "relative/state", nil }
			return ""
		}},
		"control characters in path": {unavailableUnsafe, func(t *testing.T, state, _ string) string {
			diagnosticsStateDir = func() (string, error) { return state + "\x1b[2J", nil }
			return ""
		}},
		"state blocked by a file": {unavailableUnsafe, func(t *testing.T, state, _ string) string {
			blocker := filepath.Join(state, "blocker")
			mustWrite(t, blocker, "sentinel")
			diagnosticsStateDir = func() (string, error) { return filepath.Join(blocker, "state"), nil }
			return blocker
		}},
	}
	for label, tc := range tests {
		t.Run(label, func(t *testing.T) {
			state := useDiagnosticsState(t)
			fixedDiagnosticsName(t, name)
			mustMkdir(t, state)
			worktree := t.TempDir()
			sentinel := tc.setup(t, state, worktree)
			note := failureDiagnosticsNote(worktree, tailWith("secret transcript\n"))
			if want := "diagnostics unavailable: " + string(tc.want); note != want {
				t.Fatalf("note = %q, want %q", note, want)
			}
			if sentinel != "" {
				if data, err := os.ReadFile(sentinel); err == nil && string(data) != "sentinel" {
					t.Fatal("wrote through a symlink")
				}
			}
			if _, err := os.Stat(filepath.Join(worktree, ".state")); err == nil {
				t.Fatal("created diagnostics inside the worktree")
			}
		})
	}
}

func TestDiagnosticsUnavailableKeepsRunFailure(t *testing.T) {
	previous := diagnosticsStateDir
	diagnosticsStateDir = func() (string, error) { return "", errDiagnosticsSecret }
	t.Cleanup(func() { diagnosticsStateDir = previous })
	cause := &exec.ExitError{}
	result := &RunResult{Output: "PRIVATE-TRANSCRIPT", Command: []string{"harness"}, transcript: tailWith("PRIVATE-TRANSCRIPT\n")}
	err := safeRunFailure(t.Context(), "planner", t.TempDir(), result, cause)
	if !strings.HasSuffix(err.Error(), "; diagnostics unavailable: state directory unavailable") || !errors.Is(err, cause) {
		t.Fatal(err)
	}
	for _, leaked := range []string{"PRIVATE", diagnosticsTestToken, "\x1b", "52;c", "diagnostics saved"} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("error leaked %q: %q", leaked, err)
		}
	}
	if result.Output != "" || result.Command != nil || result.transcript != nil {
		t.Fatalf("failed result not sanitized: %+v", result)
	}
}

func TestDiagnosticsRedactionFailureFailsClosed(t *testing.T) {
	state := useDiagnosticsState(t)
	previous := diagnosticsRedaction
	diagnosticsRedaction = func() (*gitcredential.Redaction, error) { return nil, errDiagnosticsSecret }
	t.Cleanup(func() { diagnosticsRedaction = previous })
	cause := &exec.ExitError{}
	err := safeRunFailure(t.Context(), "tester", t.TempDir(), &RunResult{transcript: tailWith("x\n")}, cause)
	if !strings.HasSuffix(err.Error(), "; diagnostics unavailable: credential redaction unavailable") || strings.Contains(err.Error(), "PRIVATE") || !errors.Is(err, cause) {
		t.Fatal(err)
	}
	if files := diagnosticsFiles(t, state); len(files) != 0 {
		t.Fatal("saved without redaction")
	}
}

func TestTimeoutFailureIncludesDiagnosticsPointer(t *testing.T) {
	useDiagnosticsState(t)
	result := &RunResult{transcript: tailWith("late output\n")}
	err := safeRunFailure(t.Context(), "developer", t.TempDir(), result, fmt.Errorf("private: %w", context.DeadlineExceeded))
	if !errors.Is(err, context.DeadlineExceeded) || !strings.HasPrefix(err.Error(), "developer agent timed out (harness diagnostics withheld); diagnostics saved to ") {
		t.Fatal(err)
	}
	// Sanitizing again (nested wrappers) must not save a second log.
	if again := safeRunFailure(t.Context(), "developer", "", result, errors.New("x")); strings.Contains(again.Error(), "diagnostics saved") {
		t.Fatal("saved twice")
	}
}

func TestPruneDiagnostics(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	previousNow := diagnosticsNow
	diagnosticsNow = func() time.Time { return now }
	t.Cleanup(func() { diagnosticsNow = previousNow })
	state := useDiagnosticsState(t)
	dir := filepath.Join(state, "swarm", "diagnostics")
	mustMkdir(t, dir)
	logName := func(i int) string { return fmt.Sprintf("failure-%032x.log", i) }
	// 5 expired logs, one exactly at the age limit, and 24 recent logs.
	for i := range 30 {
		age := time.Duration(i) * time.Minute
		switch {
		case i < 5:
			age = diagnosticsMaxAge + time.Hour
		case i == 5:
			age = diagnosticsMaxAge
		}
		path := filepath.Join(dir, logName(i))
		mustWrite(t, path, "old")
		if err := os.Chtimes(path, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	// Equal timestamps break ties by name, so the result is deterministic.
	tie := now.Add(-time.Hour)
	for _, i := range []int{6, 7} {
		if err := os.Chtimes(filepath.Join(dir, logName(i)), tie, tie); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside")
	mustWrite(t, outside, "sentinel")
	if err := os.Chtimes(outside, now.Add(-30*24*time.Hour), now.Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, outside, filepath.Join(dir, logName(99)))
	mustWrite(t, filepath.Join(dir, "notes.txt"), "keep")
	mustMkdir(t, filepath.Join(dir, logName(98)))

	path, _, err := saveDiagnostics(t.TempDir(), tailWith("new\n"))
	if err != nil {
		t.Fatal(err)
	}
	files := diagnosticsFiles(t, state)
	if len(files) != diagnosticsMaxCount+2 { // plus the symlink and directory, which are never touched
		t.Fatalf("kept %d entries: %v", len(files), files)
	}
	present := map[string]bool{}
	for _, name := range files {
		present[name] = true
	}
	if !present[filepath.Base(path)] || !present[logName(99)] || !present[logName(98)] {
		t.Fatal("pruned the new log or a non-regular entry")
	}
	for i := range 6 {
		if present[logName(i)] {
			t.Fatalf("expired log %d survived", i)
		}
	}
	// Recent logs 8..29 (22) plus tied 6 and 7: the newest 19 are 8..26.
	for i := 8; i <= 26; i++ {
		if !present[logName(i)] {
			t.Fatalf("recent log %d pruned", i)
		}
	}
	for _, i := range []int{6, 7, 27, 28, 29} {
		if present[logName(i)] {
			t.Fatalf("excess log %d survived", i)
		}
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "sentinel" {
		t.Fatal("followed a symlink while pruning")
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Fatal("removed an unrelated file")
	}
}

func TestConcurrentDiagnosticsRespectCount(t *testing.T) {
	state := useDiagnosticsState(t)
	var wg sync.WaitGroup
	paths := make(chan string, 2*diagnosticsMaxCount)
	for range 2 * diagnosticsMaxCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path, _, err := saveDiagnostics(t.TempDir(), tailWith("concurrent\n"))
			if err != nil {
				t.Error(err)
			}
			paths <- path
		}()
	}
	wg.Wait()
	close(paths)
	if files := diagnosticsFiles(t, state); len(files) != diagnosticsMaxCount {
		t.Fatalf("kept %d logs", len(files))
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}
