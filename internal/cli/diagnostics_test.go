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
func useFailingHarness(t *testing.T, f *cliFixture) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell harness fixture")
	}
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", state)
	script := filepath.Join(t.TempDir(), "failing-harness")
	body := "#!/bin/sh\nprintf 'PRIVATE-TRANSCRIPT stdout\\n'\nprintf 'PRIVATE-TRANSCRIPT \\033]52;c;eA==\\007 stderr\\n' >&2\nexit 9\n"
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
	return state
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
	state := useFailingHarness(t, f)
	n := strconv.Itoa(f.create(t, "agent"))
	f.must(t, "issue", "approve", n)
	raw, err := f.run("issue", "work", n, "--agent", "failing", "--headless", "--progress=false", "--json")
	if err == nil || !diagnosticsPointer.MatchString(err.Error()) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("err = %v", err)
	}
	checkJSONDiagnostics(t, raw, state)
}

func TestCLIPRWorkJSONReportsDiagnosticsPath(t *testing.T) {
	f, issue, number := submittedPRFixture(t, "agent")
	state := useFailingHarness(t, f)
	f.must(t, "issue", "approve", strconv.Itoa(issue)) // approve the base that allowlists the harness
	raw, err := f.run("pr", "work", strconv.Itoa(number), "--agent", "failing", "--progress=false", "--json")
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
	state := useFailingHarness(t, f)
	f.must(t, "issue", "approve", strconv.Itoa(issue))
	raw, err := f.run("pr", "rebase", strconv.Itoa(number), "--resolve", "--agent", "failing", "--progress=false", "--json")
	if err == nil {
		t.Fatal("failing resolver succeeded")
	}
	data := jsonDiagnostics(t, raw, state)
	// The resolver runs inside the read-only host sandbox; the saved log then
	// explains a host that cannot start it instead of the harness transcript.
	if strings.HasPrefix(data, "bwrap: ") {
		t.Skipf("host sandbox unavailable; diagnostics recorded %q", data)
	}
	checkTranscript(t, data)
}
