package validation

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	if strings.Join(names, ",") != "TestAlpha,TestAlpha/sub,TestEnd" || !strings.HasPrefix(tail, "--- FAIL: TestAlpha (0.00s)\n    --- FAIL: TestAlpha/sub (0.00s)") || !strings.Contains(tail, "--- FAIL: TestEnd (0.00s)") || !strings.Contains(tail, truncationMarker+"\nnoise") {
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

func TestDiagnosticFailBlocksSurviveTrailingNoise(t *testing.T) {
	input := "--- FAIL: TestAlpha (0.00s)\n" +
		"    --- FAIL: TestAlpha/sub (0.00s)\n" +
		"        alpha_test.go:10: want 1\n" +
		"            got 2\n" +
		"--- FAIL: TestBeta (0.00s)\n" +
		"\tbeta_test.go:7: tab message\n" +
		"unindented ends block\n" +
		"    not a message\n" +
		"FAIL\n" + strings.Repeat("usage: help text\n", 100) + "--- FAIL: TestLast (0.00s)\n    last_test.go:1: unterminated"
	want := "--- FAIL: TestAlpha (0.00s)\n" +
		"    --- FAIL: TestAlpha/sub (0.00s)\n" +
		"        alpha_test.go:10: want 1\n" +
		"            got 2\n" +
		"--- FAIL: TestBeta (0.00s)\n" +
		"\\tbeta_test.go:7: tab message\n" +
		"--- FAIL: TestLast (0.00s)\n" +
		"    last_test.go:1: unterminated\n" +
		truncationMarker + "\n" + strings.Repeat("usage: help text\n", 51)
	want = strings.TrimSuffix(want, "\n")
	for _, size := range []int{len(input), 1, 7} {
		var c checkCapture
		for i := 0; i < len(input); i += size {
			_, _ = c.Write([]byte(input[i:min(i+size, len(input))]))
		}
		for range 2 {
			tail, names := c.diagnostics()
			if tail != want || strings.Join(names, ",") != "TestAlpha,TestAlpha/sub,TestBeta,TestLast" {
				t.Fatalf("chunk %d:\n%s\n%v", size, tail, names)
			}
		}
	}
	// Failures stay visible after the 1 MiB raw capture limit.
	var c checkCapture
	_, _ = c.Write([]byte(strings.Repeat("noise\n", 300000) + "--- FAIL: TestLate (0.00s)\n    late_test.go:3: late message\n" + strings.Repeat("noise\n", 100)))
	tail, _ := c.diagnostics()
	if !strings.HasPrefix(tail, "--- FAIL: TestLate (0.00s)\n    late_test.go:3: late message\n"+truncationMarker+"\nnoise") {
		t.Fatalf("%q", tail)
	}
}

func TestDiagnosticFailBlockBounds(t *testing.T) {
	assertBounds := func(t *testing.T, tail string) {
		t.Helper()
		if len(tail) > diagnosticBytes || strings.Count(tail, "\n")+1 > diagnosticLines || !utf8.ValidString(tail) {
			t.Fatalf("bounds bytes=%d lines=%d", len(tail), strings.Count(tail, "\n")+1)
		}
		for _, r := range tail {
			if r != '\n' && unicode.IsControl(r) {
				t.Fatalf("active control %q", r)
			}
		}
	}
	noise := strings.Repeat("noise\n", 100)
	t.Run("long block", func(t *testing.T) {
		var c checkCapture
		_, _ = c.Write([]byte("--- FAIL: TestLong (0.00s)\n" + strings.Repeat("    message\n", 20) + noise))
		tail, _ := c.diagnostics()
		assertBounds(t, tail)
		want := "--- FAIL: TestLong (0.00s)\n" + strings.Repeat("    message\n", failBlockLines-2) + truncationMarker + "\n" + truncationMarker + "\nnoise"
		if !strings.HasPrefix(tail, want) {
			t.Fatalf("%q", tail)
		}
	})
	t.Run("huge escaped lines", func(t *testing.T) {
		var c checkCapture
		_, _ = c.Write([]byte("--- FAIL: Test" + strings.Repeat("\x1b世", 2000) + "\n    " + strings.Repeat("\x00", 3000) + "\n    " + strings.Repeat("x", 3*diagnosticBytes) + "\n" + noise))
		tail, _ := c.diagnostics()
		assertBounds(t, tail)
		block := tail[:strings.Index(tail, truncationMarker)+len(truncationMarker)]
		if len(block) > failBlockBytes || !strings.HasPrefix(block, "--- FAIL: Test\\x1b\\u4e16") || strings.Count(block, "\n") != 1 {
			t.Fatalf("%q", block)
		}
	})
	t.Run("many failures", func(t *testing.T) {
		var c checkCapture
		for i := range 200 {
			_, _ = c.Write([]byte("--- FAIL: TestMany" + strconv.Itoa(i) + " (0.00s)\n    many_test.go:1: " + strings.Repeat("m", 300) + "\n"))
		}
		_, _ = c.Write([]byte(noise))
		tail, _ := c.diagnostics()
		assertBounds(t, tail)
		section := tail[:strings.Index(tail, "\n"+truncationMarker+"\n")]
		if !strings.HasPrefix(section, "--- FAIL: TestMany0 (0.00s)\n") || len(section) > failSectionBytes || strings.Count(section, "\n")+2 > failSectionLines {
			t.Fatalf("section bytes=%d lines=%d", len(section), strings.Count(section, "\n")+1)
		}
		if !strings.HasSuffix(tail, "noise") || strings.Contains(tail, "TestMany199 ") {
			t.Fatalf("%q", tail)
		}
		if len(c.fails.blocks) > failSectionLines || c.fails.size > failSectionBytes {
			t.Fatal("unbounded block storage")
		}
	})
}

func TestDiagnosticNoFailUnchanged(t *testing.T) {
	lines := func(n int) string {
		var b strings.Builder
		for i := range n {
			b.WriteString("line" + strconv.Itoa(i) + "\n")
		}
		return b.String()
	}
	for _, tc := range []struct{ name, input, want string }{
		{"59 lines", lines(59), strings.TrimSuffix(lines(59), "\n")},
		{"60 lines", lines(60), strings.TrimSuffix(lines(60), "\n")},
		{"61 lines", lines(61), truncationMarker + "\n" + strings.TrimSuffix(lines(61)[len(lines(2)):], "\n")},
		{"exact bytes", strings.Repeat("x", diagnosticBytes), strings.Repeat("x", diagnosticBytes)},
		{"over bytes", strings.Repeat("x", diagnosticBytes+1), truncationMarker + "\n" + strings.Repeat("x", diagnosticBytes-len(truncationMarker)-1)},
		{"escaped overflow", strings.Repeat("\x1b", diagnosticBytes/4+1), truncationMarker + "\n" + strings.Repeat("\\x1b", (diagnosticBytes-len(truncationMarker)-1)/4)},
		{"invalid utf8", "a\xffb\n", "a\\ufffdb"},
		{"short failure", "--- FAIL: TestShort (0.00s)\n    short_test.go:1: boom\nFAIL\n", "--- FAIL: TestShort (0.00s)\n    short_test.go:1: boom\nFAIL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, size := range []int{len(tc.input) + 1, 3} {
				var c checkCapture
				for i := 0; i < len(tc.input); i += size {
					_, _ = c.Write([]byte(tc.input[i:min(i+size, len(tc.input))]))
				}
				if tail, _ := c.diagnostics(); tail != tc.want {
					t.Fatalf("chunk %d: %q", size, tail)
				}
			}
		})
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

func TestCheckOutcomeJSONFailBlocks(t *testing.T) {
	var c checkCapture
	_, _ = c.Write([]byte("--- FAIL: TestJSON (0.00s)\n    json_test.go:1: \x1bboom\n" + strings.Repeat("noise\n", 100)))
	tail, names := c.diagnostics()
	data, err := json.Marshal(CheckRunOutcome{Name: "test", ExitCode: 1, OutputTail: tail, FailingTests: names})
	if err != nil {
		t.Fatal(err)
	}
	var decoded CheckRunOutcome
	if err = json.Unmarshal(data, &decoded); err != nil || decoded.OutputTail != tail || !strings.HasPrefix(decoded.OutputTail, "--- FAIL: TestJSON (0.00s)\n    json_test.go:1: \\x1bboom\n"+truncationMarker) {
		t.Fatalf("%s %v", data, err)
	}
}

func TestHostFailureDiagnostics(t *testing.T) {
	repo := setupGitRepoForValidation(t)
	policy := "required_checks:\n  - name: failing-go\n    command: |\n      printf '%s\\n' '--- FAIL: TestHost (0.00s)' '    host_test.go:1: host message'; seq 100; exit 7\n    timeout: 1m\nprotected_paths: ['.swarm/**']\n"
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
	if check.ExitCode != 7 || strings.Join(check.FailingTests, ",") != "TestHost" || !strings.HasPrefix(check.OutputTail, "--- FAIL: TestHost (0.00s)\n    host_test.go:1: host message\n"+truncationMarker+"\n") || !strings.HasSuffix(check.OutputTail, "\n100") {
		t.Fatalf("%+v", check)
	}
}
