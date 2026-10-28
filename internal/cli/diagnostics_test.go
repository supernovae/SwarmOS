package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

var diagnosticsPointer = regexp.MustCompile(`diagnostics saved to (/\S+) \((\d+) bytes\)`)

// useFailingHarness allowlists a trusted-local "claude" harness that prints a
// private transcript, including a terminal escape, and exits non-zero.
func useFailingHarness(t *testing.T, f *cliFixture, patterns ...string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell harness fixture")
	}
	script := filepath.Join(t.TempDir(), "failing-harness")
	body := "#!/bin/sh\nprintf 'PRIVATE-TRANSCRIPT stdout\\n'\nprintf 'PRIVATE-TRANSCRIPT \\033]52;c;eA==\\007 stderr\\n' >&2\nexit 9\n"
	if len(patterns) > 0 {
		body = strings.Replace(body, "exit 9", "printf '%s\\n' '"+patterns[0]+"' >&2\nexit 9", 1)
	}
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.root, ".swarm", "config.yaml")
	cfg, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg = append(cfg, "\n  failing:\n    binary: "+strconv.Quote(script)+"\n    harness: claude\n"...)
	if err := os.WriteFile(path, cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	prGit(t, f.root, "add", path)
	prGit(t, f.root, "commit", "-m", "Allowlist failing harness")
}

// inheritState simulates the state setting the test binary inherited, such as
// a hardened check environment that sets none or one that is unusable.
func inheritState(t *testing.T, f *cliFixture, mode string) {
	t.Helper()
	switch mode {
	case "unset":
		t.Setenv("XDG_STATE_HOME", "")
		os.Unsetenv("XDG_STATE_HOME")
	case "file":
		blocker := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(blocker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("XDG_STATE_HOME", blocker)
	case "inside fixture":
		t.Setenv("XDG_STATE_HOME", filepath.Join(f.root, ".state"))
	default:
		t.Fatalf("unknown inherited state %q", mode)
	}
}

// useDiagnosticsState selects a fresh, symlink-free state root outside the
// fixture after all setup, so where a failed run is saved never depends on the
// inherited environment. Run it immediately before the failing command.
func useDiagnosticsState(t *testing.T, f *cliFixture) string {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(parent, "state")
	for _, root := range []string{f.root, mustEvalSymlinks(t, f.root)} {
		if rel, err := filepath.Rel(root, state); !filepath.IsAbs(state) || err != nil || filepath.IsLocal(rel) {
			t.Fatalf("state root %s is not outside fixture %s", state, root)
		}
	}
	t.Setenv("XDG_STATE_HOME", state)
	if got := os.Getenv("XDG_STATE_HOME"); got != state {
		t.Fatalf("XDG_STATE_HOME = %q, want %q", got, state)
	}
	return state
}

func mustEvalSymlinks(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// checkJSONDiagnostics decodes one failure document and checks that it points
// at the saved transcript without containing any of it.
func checkJSONDiagnostics(t *testing.T, raw, state string) {
	t.Helper()
	checkTranscript(t, jsonDiagnostics(t, raw, state))
}

func jsonDiagnostics(t *testing.T, raw, state string) string {
	t.Helper()
	var outcome struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.NewDecoder(strings.NewReader(raw)).Decode(&outcome); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, raw)
	}
	match := diagnosticsPointer.FindStringSubmatch(outcome.Error)
	if outcome.Status != "failed" || match == nil || !strings.HasPrefix(match[1], filepath.Join(state, "swarm", "diagnostics")+string(filepath.Separator)) {
		t.Fatalf("missing diagnostics pointer: %s", raw)
	}
	if strings.Contains(raw, "PRIVATE-TRANSCRIPT") || strings.Contains(raw, "52;c") || strings.Contains(raw, "\\u001b") {
		t.Fatalf("JSON leaked harness output: %s", raw)
	}
	data, err := os.ReadFile(match[1])
	if err != nil || strconv.Itoa(len(data)) != match[2] {
		t.Fatalf("saved diagnostics: %d bytes, %v", len(data), err)
	}
	return string(data)
}

func checkTranscript(t *testing.T, data string) {
	t.Helper()
	if !strings.Contains(data, "PRIVATE-TRANSCRIPT stdout") || !strings.Contains(data, `\x1b]52;c;eA==\x07`) || strings.ContainsRune(data, '\x1b') {
		t.Fatalf("saved diagnostics = %q", data)
	}
}

func TestCLIIssueWorkJSONReportsDiagnosticsPath(t *testing.T) {
	f := newCLIFixture(t)
	useFailingHarness(t, f)
	n := strconv.Itoa(f.create(t, "agent"))
	f.must(t, "issue", "approve", n)
	inheritState(t, f, "file")
	state := useDiagnosticsState(t, f)
	raw, err := f.run("issue", "work", n, "--agent", "mock", "--planner-agent", "failing", "--headless", "--progress=false", "--json")
	if err == nil || !diagnosticsPointer.MatchString(err.Error()) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("err = %v", err)
	}
	checkJSONDiagnostics(t, raw, state)
}

func TestCLIPRWorkJSONReportsDiagnosticsPath(t *testing.T) {
	f, issue, number := submittedPRFixture(t, "agent")
	useFailingHarness(t, f)
	f.must(t, "issue", "approve", strconv.Itoa(issue)) // approve the base that allowlists the harness
	inheritState(t, f, "unset")
	state := useDiagnosticsState(t, f)
	raw, err := f.run("pr", "work", strconv.Itoa(number), "--agent", "mock", "--planner-agent", "failing", "--progress=false", "--json")
	if err == nil {
		t.Fatal("failing harness succeeded")
	}
	checkJSONDiagnostics(t, raw, state)
}

func TestCLIPRRebaseResolveJSONReportsDiagnosticsPath(t *testing.T) {
	f, issue, number := submittedPRFixture(t, "agent")
	st := f.state(t, issue)
	conflicting := strings.Fields(prGit(t, f.root, "diff", "--name-only", st.BaseSHA, st.HeadSHA))[0]
	if err := os.WriteFile(filepath.Join(f.root, conflicting), []byte("upstream content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prGit(t, f.root, "add", conflicting)
	prGit(t, f.root, "commit", "-m", "Conflicting upstream change")
	useFailingHarness(t, f)
	f.must(t, "issue", "approve", strconv.Itoa(issue))
	inheritState(t, f, "inside fixture")
	state := useDiagnosticsState(t, f)
	raw, err := f.run("pr", "rebase", strconv.Itoa(number), "--resolve", "--agent", "mock", "--developer-agent", "failing", "--progress=false", "--json")
	if err == nil {
		t.Fatal("failing resolver succeeded")
	}
	checkJSONDiagnostics(t, raw, state)
}

func TestCLIIssueWorkJSONReportsDiagnosticsUnavailable(t *testing.T) {
	f := newCLIFixture(t)
	useFailingHarness(t, f)
	n := strconv.Itoa(f.create(t, "agent"))
	f.must(t, "issue", "approve", n)
	inheritState(t, f, "file")
	blocked := os.Getenv("XDG_STATE_HOME")
	raw, err := f.run("issue", "work", n, "--agent", "mock", "--planner-agent", "failing", "--headless", "--progress=false", "--json")
	if err == nil {
		t.Fatal("failing harness succeeded")
	}
	var outcome struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.NewDecoder(strings.NewReader(raw)).Decode(&outcome); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, raw)
	}
	if outcome.Status != "failed" || !strings.HasSuffix(outcome.Error, "; diagnostics unavailable: unsafe state directory") {
		t.Fatalf("missing unavailable reason: %s", raw)
	}
	for _, leaked := range []string{"PRIVATE-TRANSCRIPT", "52;c", "\\u001b", "diagnostics saved", blocked} {
		if strings.Contains(raw, leaked) {
			t.Fatalf("JSON leaked %q: %s", leaked, raw)
		}
	}
}

func TestCLIProviderFailureStatus(t *testing.T) {
	f := newCLIFixture(t)
	useFailingHarness(t, f, "model is overloaded synthetic-private-token")
	number := f.create(t, "agent")
	n := strconv.Itoa(number)
	f.must(t, "issue", "approve", n)
	useDiagnosticsState(t, f)
	raw, err := f.run("issue", "work", n, "--agent", "mock", "--planner-agent", "failing", "--headless", "--progress=false", "--json")
	if err == nil {
		t.Fatal("fixture succeeded")
	}
	for _, text := range []string{raw, err.Error()} {
		if !strings.Contains(text, "provider model at capacity; resume: swarm issue work "+n+" --phase plan") {
			t.Fatal(text)
		}
		if strings.Contains(text, "synthetic-private-token") || strings.Contains(text, "model is overloaded") || strings.Contains(text, "PRIVATE-TRANSCRIPT") {
			t.Fatal("diagnostic leaked")
		}
	}
	st := f.state(t, number)
	if len(st.RoleRuns) != 1 || st.RoleRuns[0].FailureClass != "capacity" || len(st.Artifacts) != 0 {
		t.Fatalf("failed history: %+v", st)
	}
	for _, args := range [][]string{{"issue", "status", n}, {"issue", "status", n, "--json"}} {
		status := f.must(t, args...)
		if !strings.Contains(status, "capacity") || strings.Contains(status, "synthetic-private-token") {
			t.Fatal(status)
		}
	}
}
