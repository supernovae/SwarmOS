package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/validation"
)

func TestValidationOutput(t *testing.T) {
	report := &validation.ValidationReport{CheckOutcomes: []validation.CheckRunOutcome{
		{Name: "ok", Passed: true, Output: "do not display passing output"},
		{Name: "test\x1b", ExitCode: 7, OutputTail: "failure detail", FailingTests: []string{"TestAlpha", "TestAlpha/sub"}},
		{Name: "empty", ExitCode: 1},
	}}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	sentinel := errors.New("original check error")
	if err := writeValidationOutcome(cmd, report, sentinel, false); err != sentinel {
		t.Fatal("error identity changed")
	}
	want := "PASS ok\nFAIL test\\x1b (exit 7)\nfailing tests: TestAlpha, TestAlpha/sub\nfailure detail\nFAIL empty (exit 1)\n"
	if out.String() != want || strings.Contains(out.String(), "passing output") {
		t.Fatalf("%q", out.String())
	}
}

func TestFailedDiagnosticsAcrossCommands(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	policy := "required_checks:\n  - name: trusted-failure\n    command: |\n      if test -f fail-check; then printf '%s\\n' '--- FAIL: TestCommand (0.00s)' '    command_test.go:1: command message'; seq 70; exit 7; fi\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**']\n"
	if err := os.WriteFile(filepath.Join(f.root, ".swarm/validation/required-checks.yaml"), []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
	prGit(t, f.root, "add", ".")
	prGit(t, f.root, "commit", "-m", "conditional failing check")
	n := f.create(t, "none")
	number := strconv.Itoa(n)
	f.must(t, "issue", "approve", number)
	f.must(t, "issue", "work", number, "--agent", "mock", "--progress=false")
	f.must(t, "issue", "submit", number)
	prNumber := strconv.Itoa(f.state(t, n).PRNumber)
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "echo fail > fail-check")
	failing := strconv.Itoa(f.create(t, "none"))
	f.must(t, "issue", "approve", failing)
	if _, err := f.run("issue", "work", failing, "--agent", "mock", "--progress=false"); err == nil {
		t.Fatal("expected failed issue check")
	}
	for _, op := range []string{"validate", "submit"} {
		for _, asJSON := range []bool{false, true} {
			args := []string{"issue", op, failing, "--progress=false"}
			if asJSON {
				args = append(args, "--json")
			}
			out, _, err := runProgressCommand(t, f, args...)
			assertCommandFailureDiagnostics(t, out, err, asJSON)
		}
	}
	if _, err := f.run("pr", "work", prNumber, "--agent", "mock", "--progress=false"); err == nil {
		t.Fatal("expected failed revision check")
	}
	for _, op := range []string{"validate", "submit"} {
		for _, asJSON := range []bool{false, true} {
			args := []string{"pr", op, prNumber, "--progress=false"}
			if asJSON {
				args = append(args, "--json")
			}
			out, _, err := runProgressCommand(t, f, args...)
			assertCommandFailureDiagnostics(t, out, err, asJSON)
		}
	}
	// Repo validation uses the same trusted check directly in the checkout.
	if err := os.WriteFile(filepath.Join(f.root, "fail-check"), []byte("fail"), 0600); err != nil {
		t.Fatal(err)
	}
	base := prGit(t, f.root, "rev-parse", "HEAD")
	for _, asJSON := range []bool{false, true} {
		args := []string{"repo", "validate", "--base", base}
		if asJSON {
			args = append(args, "--json")
		}
		out, _, err := runProgressCommand(t, f, args...)
		assertCommandFailureDiagnostics(t, out, err, asJSON)
	}
}

// commandFailureTail is the FAIL block kept ahead of the trailing noise.
const commandFailureTail = "--- FAIL: TestCommand (0.00s)\n    command_test.go:1: command message\n[swarm: output truncated]\n"

func assertCommandFailureDiagnostics(t *testing.T, out string, err error, asJSON bool) {
	t.Helper()
	if err == nil {
		t.Fatalf("failure accepted: %s", out)
	}
	if asJSON {
		decoder := json.NewDecoder(strings.NewReader(out))
		var value map[string]any
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			t.Fatalf("extra JSON: %v", err)
		}
		tail, _ := json.Marshal(commandFailureTail)
		if !strings.Contains(out, `"output_tail":`+strings.TrimSuffix(string(tail), `"`)) || !strings.Contains(out, `"failing_tests":["TestCommand"]`) {
			t.Fatalf("missing JSON diagnostics: %s", out)
		}
	} else if !strings.Contains(out, "FAIL trusted-failure (exit 7)\nfailing tests: TestCommand\n"+commandFailureTail) || !strings.Contains(out, "\n70\n") {
		t.Fatalf("missing text diagnostics: %s", out)
	}
}
