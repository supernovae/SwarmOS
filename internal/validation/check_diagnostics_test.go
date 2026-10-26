package validation

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestCheckCaptureDiagnostics(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"empty", "", ""}, {"non-Go", "compiler error\n", "compiler error"},
		{"controls", "\x1b[31m\r\t\b\x00\x7f\u0085", "\\x1b[31m\\r\\t\\b\\x00\\x7f\\u0085"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c checkCapture
			_, _ = c.Write([]byte(tc.input))
			tail, names := c.diagnostics()
			if tail != tc.want || len(names) != 0 {
				t.Fatalf("%q %v", tail, names)
			}
		})
	}
	var c checkCapture
	for _, s := range []string{"--- FA", "IL: TestAlpha (0.00s)\n    --- FAIL: TestAlpha/sub (0.00s)\n--- FAIL: TestAlpha (0.00s)\n", strings.Repeat("noise\n", 300000), "--- FAIL: TestEnd (0.00s)"} {
		_, _ = c.Write([]byte(s))
	}
	tail, names := c.diagnostics()
	if strings.Join(names, ",") != "TestAlpha,TestAlpha/sub,TestEnd" || !strings.HasSuffix(tail, "--- FAIL: TestEnd (0.00s)") || !strings.HasPrefix(tail, truncationMarker) {
		t.Fatalf("%q %v", tail, names)
	}
	if !strings.Contains(c.raw.String(), "1 MiB") {
		t.Fatal("raw capture compatibility lost")
	}
}

func TestDiagnosticBounds(t *testing.T) {
	for _, input := range []string{strings.Repeat("x\n", 59), strings.Repeat("x\n", 60), strings.Repeat("x\n", 61), strings.Repeat("x", diagnosticBytes), strings.Repeat("x", diagnosticBytes+1), strings.Repeat("\x1b", diagnosticBytes), strings.Repeat("世", diagnosticBytes), strings.Repeat("x", 2<<20)} {
		var c checkCapture
		_, _ = c.Write([]byte(input))
		tail, _ := c.diagnostics()
		if len(tail) > diagnosticBytes || strings.Count(tail, "\n")+1 > diagnosticLines || !utf8.ValidString(tail) {
			t.Fatalf("bounds bytes=%d lines=%d", len(tail), strings.Count(tail, "\n")+1)
		}
		for _, r := range tail {
			if r != '\n' && unicode.IsControl(r) {
				t.Fatalf("active control %q", r)
			}
		}
		if len(input) > diagnosticBytes && !strings.Contains(tail, truncationMarker) {
			t.Fatal("missing truncation marker")
		}
		if len(c.pending) > diagnosticBytes {
			t.Fatal("unbounded parser")
		}
	}
}

func TestDiagnosticConcurrentCaptureAndNameLimit(t *testing.T) {
	var c checkCapture
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				_, _ = c.Write([]byte("--- FAIL: TestConcurrent (0.00s)\n"))
			}
		}()
	}
	wg.Wait()
	_, names := c.diagnostics()
	if len(names) != 1 {
		t.Fatal(names)
	}
	for i := range 100 {
		_, _ = c.Write([]byte("--- FAIL: Test" + strings.Repeat("x", i+1) + " (0.00s)\n"))
	}
	_, names = c.diagnostics()
	if names[len(names)-1] != truncationMarker {
		t.Fatal("missing name marker")
	}
}

func TestCheckOutcomeJSON(t *testing.T) {
	for _, passed := range []bool{true, false} {
		for _, names := range [][]string{nil, {"TestAlpha"}} {
			original := CheckRunOutcome{Name: "test", Output: "raw", Passed: passed, OutputTail: "tail", FailingTests: names}
			data, err := json.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			var value map[string]any
			if err = json.Unmarshal(data, &value); err != nil {
				t.Fatal(err)
			}
			_, tail := value["output_tail"]
			_, tests := value["failing_tests"]
			if tail == passed || tests == passed {
				t.Fatalf("shape: %s", data)
			}
			if !passed {
				if _, ok := value["failing_tests"].([]any); !ok {
					t.Fatalf("not array: %s", data)
				}
			}
			var decoded CheckRunOutcome
			if err = json.Unmarshal(data, &decoded); err != nil || decoded.Output != "raw" || (!passed && decoded.OutputTail != "tail") {
				t.Fatalf("round trip: %+v %v", decoded, err)
			}
		}
	}
}

func TestHostFailureDiagnostics(t *testing.T) {
	repo := setupGitRepoForValidation(t)
	policy := "required_checks:\n  - name: failing-go\n    command: |\n      printf '%s\\n' '--- FAIL: TestHost (0.00s)'; exit 7\n    timeout: 1m\nprotected_paths: ['.swarm/**']\n"
	if err := os.WriteFile(filepath.Join(repo, ".swarm/validation/required-checks.yaml"), []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "failure fixture"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	report, err := Validate(t.Context(), repo, nil, ValidateOptions{CIMode: true, RunnerIsolated: true, BaseSHA: gitRevision(t, repo, "HEAD")})
	if !errors.Is(err, ErrRequiredCheckFailed) || report == nil {
		t.Fatalf("%+v %v", report, err)
	}
	check := report.CheckOutcomes[0]
	if check.ExitCode != 7 || strings.Join(check.FailingTests, ",") != "TestHost" || !strings.Contains(check.OutputTail, "TestHost") {
		t.Fatalf("%+v", check)
	}
}
