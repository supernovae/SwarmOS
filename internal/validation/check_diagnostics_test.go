package validation

import (
	"encoding/json"
	"errors"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestCheckCaptureDiagnostics(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, input, want string }{
		{"empty", "", ""}, {"non-Go", "compiler error\n", "compiler error"},
		{"controls", "\x1b[31m\r\t\b\x00\x7f\u0085", "\\x1b[31m\\r\\t\\b\\x00\\x7f\\u0085"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
			t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	repo := setupGitRepoForValidation(t)
	policy := "required_checks:\n  - name: failing-go\n    command: |\n      printf '%s\\n' '--- FAIL: TestHost (0.00s)' '    host_test.go:1: host message'; seq 100; exit 7\n    timeout: 1m\nprotected_paths: ['.swarm/**']\n"
	if err := os.WriteFile(filepath.Join(repo, ".swarm/validation/required-checks.yaml"), []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "failure fixture"}} {
		if out, err := gitfixture.Command(t, "", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
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

func TestJSONSkipCapture(t *testing.T) {
	t.Parallel()
	for _, preserve := range []bool{false, true} {
		c := &checkCapture{jsonMode: true, preserveJSON: preserve}
		event := func(action, pkg, test, output string) {
			b, _ := json.Marshal(map[string]string{"Action": action, "Package": pkg, "Test": test, "Output": output})
			b = append(b, '\n')
			for _, v := range b {
				_, _ = c.Write([]byte{v})
			}
		}
		event("output", "p", "TestSandbox/sub", "    x_test.go:4: nested bwrap unavailable\n")
		event("skip", "p", "TestSandbox/sub", "")
		event("skip", "p", "", "")
		for i := 0; i < 65; i++ {
			event("skip", "p", "Test"+strconv.Itoa(i), "")
		}
		event("output", "p", "", "ok  \tp\t(cached)\n")
		c.finish()
		if c.skipTotal != 66 || len(c.skips) != 60 || !strings.Contains(c.skips[0].Message, "nested bwrap unavailable") {
			t.Fatalf("%+v %d", c.skips, c.skipTotal)
		}
		if strings.Contains(c.raw.String(), `"Action"`) != preserve {
			t.Fatalf("output mode: %s", c.raw.String())
		}
		tail, _ := c.diagnostics()
		if !strings.Contains(tail, "(cached)") {
			t.Fatal(tail)
		}
	}
}

func TestJSONBuildFailure(t *testing.T) {
	t.Parallel()
	c := &checkCapture{jsonMode: true}
	for _, e := range []map[string]string{
		{"Action": "build-output", "ImportPath": "p", "Output": "# p\nx.go:3: undefined: missing\n"},
		{"Action": "build-fail", "ImportPath": "p"},
		{"Action": "fail", "Package": "p", "FailedBuild": "p"},
	} {
		b, _ := json.Marshal(e)
		_, _ = c.Write(append(b, '\n'))
	}
	c.finish()
	tests, pkgs, _ := c.goFailures()
	if len(tests) != 0 || len(pkgs) != 1 || pkgs[0] != "p" {
		t.Fatalf("%v %v", tests, pkgs)
	}
	if !strings.Contains(c.raw.String(), "undefined: missing") || strings.Contains(c.raw.String(), `"Action"`) {
		t.Fatal(c.raw.String())
	}
}

func TestJSONLateSkipAndFailureRetention(t *testing.T) {
	t.Parallel()
	c := &checkCapture{jsonMode: true}
	event := func(e map[string]string) { t.Helper(); b, _ := json.Marshal(e); _, _ = c.Write(append(b, '\n')) }
	event(map[string]string{"Action": "build-output", "ImportPath": "broken", "Output": "broken.go:3: undefined: missing\n"})
	event(map[string]string{"Action": "build-fail", "ImportPath": "broken"})
	event(map[string]string{"Action": "output", "Package": "a", "Test": "TestA", "Output": "    a_test.go:1: failure detail\n--- FAIL: TestA (0.00s)\n"})
	event(map[string]string{"Action": "fail", "Package": "a", "Test": "TestA"})
	event(map[string]string{"Action": "output", "Package": "b", "Output": "ok  \tb\t0.001s\n"})
	event(map[string]string{"Action": "fail", "Package": "a"})
	event(map[string]string{"Action": "fail", "Package": "broken", "FailedBuild": "broken"})
	for i := 0; i < 1500; i++ {
		event(map[string]string{"Action": "output", "Package": "noise", "Output": strings.Repeat("noise", 100) + "\n"})
	}
	event(map[string]string{"Action": "output", "Package": "p", "Test": "TestLate", "Output": "    late_test.go:1: sockets denied\n"})
	// Finish an unterminated JSON event.
	_, _ = c.Write([]byte(`{"Action":"skip","Package":"p","Test":"TestLate"}`))
	c.finish()
	tail, _ := c.diagnostics()
	tests, pkgs, _ := c.goFailures()
	if c.skipTotal != 1 || c.skips[0].Test != "TestLate" || len(tests) != 1 || tests[0].pkg != "a" || len(pkgs) != 1 || pkgs[0] != "broken" {
		t.Fatalf("%+v %v %v", c.skips, tests, pkgs)
	}
	if !strings.Contains(tail, "undefined: missing") || !strings.Contains(tail, "failure detail") {
		t.Fatal(tail)
	}
}

func TestJSONMalformedAndOversized(t *testing.T) {
	t.Parallel()
	c := &checkCapture{jsonMode: true}
	_, _ = c.Write([]byte("not an event\n"))
	_, _ = c.Write([]byte(strings.Repeat("x", (1<<20)+1) + "\n"))
	_, _ = c.Write([]byte(`{"Action":"output","Package":"p","Test":"TestLong","Output":"` + strings.Repeat("a", 4096) + `\n    x_test.go:4: capability denied\n"}` + "\n"))
	_, _ = c.Write([]byte(`{"Action":"skip","Package":"p","Test":"TestLong"}` + "\n"))
	c.finish()
	before := c.raw.String()
	c.finish()
	if c.raw.String() != before || c.skipTotal != 1 || len(c.skips[0].Message) > 2048 || !strings.Contains(c.skips[0].Message, "capability denied") || !strings.Contains(before, "not an event") || !strings.Contains(before, truncationMarker) {
		t.Fatalf("capture=%+v raw length=%d", c.skips, len(before))
	}
}

func TestJSONCapturePlainFallbackAttribution(t *testing.T) {
	t.Parallel()
	c := &checkCapture{jsonMode: true}
	_, _ = c.Write([]byte("--- FAIL: TestBroken (0.00s)\n    x_test.go:1: broken\nFAIL\nFAIL\texample.com/m/broken\t0.01s\n"))
	c.finish()
	tests, pkgs, truncated := c.goFailures()
	if len(tests) != 1 || tests[0].pkg != "example.com/m/broken" || tests[0].name != "TestBroken" || len(pkgs) != 0 || truncated {
		t.Fatalf("%v %v %t", tests, pkgs, truncated)
	}
}

func TestJSONFailureMessagePrefix(t *testing.T) {
	t.Parallel()
	for _, preserve := range []bool{false, true} {
		for _, fragmented := range []bool{false, true} {
			for _, rows := range []int{5, 20} {
				c := &checkCapture{jsonMode: true, preserveJSON: preserve}
				emit := func(action, pkg, name, out string) {
					b, _ := json.Marshal(map[string]string{"Action": action, "Package": pkg, "Test": name, "Output": out})
					b = append(b, '\n')
					if fragmented {
						for _, v := range b {
							_, _ = c.Write([]byte{v})
						}
					} else {
						_, _ = c.Write(b)
					}
				}
				name := "TestRoleRunsShowReasoningTokens"
				emit("run", "p", name, "")
				body := "    role_run_reasoning_test.go:43: role runs lack reasoning\x1b\n"
				for i := 0; i < rows; i++ {
					body += "        row" + strconv.Itoa(i) + strings.Repeat("x", 300) + "\n"
				}
				emit("output", "p", name, body)
				emit("output", "p", name, "--- FAIL: "+name+" (0.00s)\n")
				emit("fail", "p", name, "")
				emit("output", "noise", "", strings.Repeat("noise\n", 4000))
				c.finish()
				got, _ := c.diagnostics()
				again, _ := c.diagnostics()
				if got != again || !strings.Contains(got, "role_run_reasoning_test.go:43: role runs lack reasoning\\x1b") {
					t.Fatalf("rows=%d preserve=%v fragmented=%v: %s", rows, preserve, fragmented, got)
				}
				if len(got) > diagnosticBytes || len(strings.Split(got, "\n")) > diagnosticLines {
					t.Fatal("diagnostic bounds exceeded")
				}
				if c.fails.lines > failSectionLines || c.fails.size > failSectionBytes {
					t.Fatal("section bounds exceeded")
				}
				if len(c.jsonFailures) != 0 {
					t.Fatal("completed contexts retained")
				}
				if !strings.Contains(got, "row0") || strings.Index(got, "row0") > strings.Index(got, "row1") {
					t.Fatal("prefix order lost")
				}
				for _, b := range c.fails.blocks {
					lines, size := b.rendered()
					if lines > failBlockLines || size > failBlockBytes {
						t.Fatal("block bounds exceeded")
					}
					if rows == 20 && !b.cut {
						t.Fatal("missing in-block truncation")
					}
				}
			}
		}
	}
}

func TestJSONSubtestFailureMessages(t *testing.T) {
	t.Parallel()
	for _, preserve := range []bool{false, true} {
		for _, fragmented := range []bool{false, true} {
			c := &checkCapture{jsonMode: true, preserveJSON: preserve}
			emit := func(action, pkg, name, out string) {
				b, _ := json.Marshal(map[string]string{"Action": action, "Package": pkg, "Test": name, "Output": out})
				b = append(b, '\n')
				if fragmented {
					for _, v := range b {
						_, _ = c.Write([]byte{v})
					}
				} else {
					_, _ = c.Write(b)
				}
			}
			for _, pkg := range []string{"a", "b"} {
				for _, sub := range []string{"one", "two"} {
					emit("run", pkg, "Parent/"+sub, "")
					emit("output", pkg, "Parent/"+sub, "    "+pkg+"_"+sub+"_test.go:4: detail\n")
				}
			}
			for _, pkg := range []string{"a", "b"} {
				for _, sub := range []string{"one", "two"} {
					emit("output", pkg, "Parent/"+sub, "    --- FAIL: Parent/"+sub+" (0.00s)\n")
					emit("fail", pkg, "Parent/"+sub, "")
				}
				emit("output", pkg, "Parent", "--- FAIL: Parent (0.00s)\n")
				emit("fail", pkg, "Parent", "")
			}
			emit("output", "noise", "", strings.Repeat("noise\n", 4000))
			c.finish()
			got, _ := c.diagnostics()
			if strings.Count(got, "--- FAIL: Parent/one") != 2 || strings.Count(got, "--- FAIL: Parent/two") != 2 {
				t.Fatal(got)
			}
			for i, b := range c.fails.blocks {
				if i%3 == 2 {
					continue
				}
				pkg := "a"
				if i >= 3 {
					pkg = "b"
				}
				sub := []string{"one", "two"}[i%3]
				if len(b.lines) != 2 || !strings.Contains(b.lines[1], pkg+"_"+sub+"_test.go:4") {
					t.Fatalf("wrong block: %+v", b)
				}
			}
			tests, _, _ := c.goFailures()
			if len(tests) != 6 {
				t.Fatal(tests)
			}
		}
	}
}

func TestRebaseLocationCompilerGrammar(t *testing.T) {
	for _, tc := range []struct {
		line  string
		count int
	}{
		{"internal/p/x.go:3:4: bad", 1}, {"vet: internal/p/x.go:3: bad", 1},
		{"vet: vet: internal/p/x.go:3: bad", 0}, {" other: internal/p/x.go:3: bad", 0},
		{" internal/p/x.go:3: bad", 0}, {"tool: internal/p/x.go:3: bad", 0},
		{"internal/p/x.go:0: bad", 0},
	} {
		h := parseCompilerHints("# example.com/m/internal/p\n" + tc.line + "\n")
		if len(h.Locations) != tc.count {
			t.Errorf("%q: %+v", tc.line, h)
		}
	}
	for _, command := range []string{"go test ./...", "env go build ./...", "go build -v ./...", "go build ./...; true", "go vet ./... | cat", "go build $(echo ./...)"} {
		if compilerCommand(command) != "" {
			t.Errorf("accepted %q", command)
		}
	}
	for _, text := range []string{"# example.com/m/p\np/x.go:2: bad", strings.Repeat("x", diagnosticBytes+1)} {
		if !parseCompilerHints(text).Incomplete {
			t.Errorf("accepted incomplete output")
		}
	}
	for _, header := range []string{"# example.com/m/p [example.com/m/p.test]", "# invalid package", "#", "# /outside"} {
		h := parseCompilerHints("# example.com/m/p\n" + header + "\np/x.go:2: bad\n")
		if len(h.Locations) != 0 {
			t.Errorf("header did not reset: %s", header)
		}
	}
}

func TestRebaseLocationExecutionRoot(t *testing.T) {
	for _, tc := range []struct{ root, p, want string }{
		{"/host/repo", "internal/p.go", "internal/p.go"}, {"/workspace", "./p.go", "p.go"},
		{"/workspace", "/workspace/internal/p.go", "internal/p.go"},
		{"/host/repo", "/workspace/p.go", ""}, {"/workspace", "/host/repo/p.go", ""},
		{"/workspace", "/workspace-other/p.go", ""}, {"/workspace", "/workspace/../p.go", ""},
		{"/workspace", "C:/p.go", ""}, {"/workspace", "a\\p.go", ""}, {"/workspace", "./../p.go", ""},
	} {
		got, ok := diagnosticPath(tc.root, tc.p)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("%+v: %q %t", tc, got, ok)
		}
	}
}
