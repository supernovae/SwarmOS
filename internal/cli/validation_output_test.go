package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
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

func TestIssueStatusShowsPreExistingRepair(t *testing.T) {
	f := newCLIFixture(t)
	policy := "required_checks:\n  - name: test\n    command: go test -count=1 ./...\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**']\n"
	if err := os.WriteFile(filepath.Join(f.root, ".swarm", "validation", "required-checks.yaml"), []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"commit", "-am", "go policy"}} {
		if out, err := gitfixture.Command(t, "", append([]string{"-C", f.root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	// Checks run with the process PATH; this fake go fails TestBroken on the
	// candidate and at the approved base.
	dir := t.TempDir()
	script := "#!/bin/sh\necho '--- FAIL: TestBroken (0.00s)'; echo FAIL; printf 'FAIL\\texample.com/m/broken\\t0.01s\\n'; exit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	issue := f.create(t, "none")
	n := strconv.Itoa(issue)
	f.must(t, "issue", "approve", n)
	doc, err := f.runJSON(t, "issue", "work", n, "--agent", "mock", "--progress=false", "--json")
	assertNextAction(t, doc, err, ReasonPreExistingFailures, true, ExitFailure, "swarm", "--backend", "local", "--root", f.root, "issue", "status", n)
	if report, _ := doc["report"].(map[string]any); report == nil || report["repair"] == nil {
		t.Fatalf("work JSON lacks the repair summary: %v", doc)
	}
	raw := f.must(t, "issue", "status", n, "--json")
	var status struct {
		Issues []struct {
			Repair *validation.RepairSummary `json:"repair"`
		} `json:"issues"`
	}
	if err := json.Unmarshal([]byte(raw), &status); err != nil || len(status.Issues) != 1 {
		t.Fatalf("%v: %s", err, raw)
	}
	repair := status.Issues[0].Repair
	if repair == nil || !repair.AllPreExisting || len(repair.Tests) != 1 || repair.Tests[0].Classification != validation.TestPreExisting || repair.Tests[0].Package != "example.com/m/broken" {
		t.Fatalf("status repair: %s", raw)
	}
	// A forged local brief does not change the recorded evidence.
	if err := os.WriteFile(filepath.Join(f.root, ".swarm", "worktrees", "issue-"+n, ".swarm", "active", validation.RepairFile), []byte("regression: none\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	text := f.must(t, "issue", "status", n)
	for _, want := range []string{"Validation repair: failed checks test", "pre-existing: TestBroken (example.com/m/broken) in test", "Every failure also fails at the approved base"} {
		if !strings.Contains(text, want) {
			t.Fatalf("status text lacks %q:\n%s", want, text)
		}
	}
}

func TestValidationSkipNotice(t *testing.T) {
	for _, tc := range []struct {
		name   string
		report *validation.ValidationReport
		notice bool
	}{
		{"none", nil, false},
		{"no skips", &validation.ValidationReport{Passed: true}, false},
		{"failed", &validation.ValidationReport{SkippedTotal: 1}, false},
		{"passing", &validation.ValidationReport{Passed: true, SkippedTotal: 1, SkippedTests: []validation.SkippedTest{{Package: "example.com/m", Test: "TestSandbox\x1b"}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			var out, diag bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&diag)
			writeValidationSkipNotice(cmd, tc.report)
			if out.Len() != 0 || (diag.Len() > 0) != tc.notice {
				t.Fatalf("stdout=%q stderr=%q", out.String(), diag.String())
			}
			if tc.notice && (!strings.Contains(diag.String(), "TestSandbox\\x1b") || !strings.Contains(diag.String(), "VALIDATION.md") || strings.Count(diag.String(), "\n") != 1) {
				t.Fatal(diag.String())
			}
		})
	}
}

func TestSkipNoticesAcrossCommands(t *testing.T) {
	f := newCLIFixture(t)
	policy := "required_checks:\n  - name: test\n    command: go test ./...\n    timeout: 2m\nprotected_paths: ['.swarm/**', '.github/**']\n"
	for name, data := range map[string]string{
		".swarm/validation/required-checks.yaml": policy,
		"go.mod":                                 "module example.com/skips\n\ngo 1.21\n",
		"skip_test.go":                           "package skips\nimport \"testing\"\nfunc TestPass(t *testing.T) {}\nfunc TestSandbox(t *testing.T) { t.Skip(\"loopback unavailable\") }\n",
	} {
		if err := os.WriteFile(filepath.Join(f.root, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	prGit(t, f.root, "add", ".")
	prGit(t, f.root, "commit", "-m", "skip fixture")
	number := strconv.Itoa(f.create(t, "none"))
	f.must(t, "issue", "approve", number)
	check := func(args ...string) string {
		t.Helper()
		args = append(args, "--progress=false", "--json")
		out, diag, err := runProgressCommand(t, f, args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s\n%s", args, err, out, diag)
		}
		if !json.Valid([]byte(out)) || strings.Count(diag, "Validation passed with 1 skipped test") != 1 || !strings.Contains(diag, "example.com/skips.TestSandbox") || !strings.Contains(diag, "VALIDATION.md") {
			t.Fatalf("%v: stdout=%s stderr=%s", args, out, diag)
		}
		return out
	}
	check("issue", "work", number, "--agent", "mock")
	check("issue", "submit", number)
	n, _ := strconv.Atoi(number)
	p := strconv.Itoa(f.state(t, n).PRNumber)
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "echo revision > revision.txt")
	check("pr", "work", p, "--agent", "mock")
	check("pr", "submit", p)
	// Conflict on the revision file exercises the rebase path that validates.
	if err := os.WriteFile(filepath.Join(f.root, "revision.txt"), []byte("upstream\n"), 0600); err != nil {
		t.Fatal(err)
	}
	prGit(t, f.root, "add", "revision.txt")
	prGit(t, f.root, "commit", "-m", "conflict")
	f.must(t, "issue", "approve", number)
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", resolverScript(t.TempDir()))
	check("pr", "rebase", p, "--resolve", "--agent", "mock")
}
