package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"testing"
)

func TestHeadlessWorkProgressKeepsJSONOnStdout(t *testing.T) {
	for _, tc := range []struct {
		name, gate, status, terminal string
		flags                        []string
	}{
		{"plan-only", "agent", "completed", "plan completed", []string{"--phase", "plan"}},
		{"handoff", "agent", "handed-off", "handoff handed-off", []string{"--phase", "plan", "--handoff"}},
		{"human-pause", "human", "paused", "awaiting-approval paused", nil},
		{"full", "agent", "completed", "required-checks completed", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCLIFixture(t)
			n := strconv.Itoa(f.create(t, tc.gate))
			f.must(t, "issue", "approve", n)
			cmd := NewRootCmd()
			var output, diagnostics bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&diagnostics)
			args := []string{"--root", f.root, "--backend", "local", "issue", "work", n, "--agent", "mock", "--json"}
			cmd.SetArgs(append(args, tc.flags...))
			if err := cmd.Execute(); err != nil {
				t.Fatalf("%v\n%s", err, diagnostics.String())
			}
			decoder := json.NewDecoder(&output)
			var outcome struct {
				Status string `json:"status"`
			}
			if err := decoder.Decode(&outcome); err != nil || outcome.Status != tc.status {
				t.Fatalf("invalid outcome: %+v %v", outcome, err)
			}
			var extra any
			if err := decoder.Decode(&extra); err != io.EOF {
				t.Fatalf("stdout contains more than one JSON document: %v", err)
			}
			text := diagnostics.String()
			for _, want := range []string{"preflight started", "workspace started", tc.terminal} {
				if !strings.Contains(text, want) {
					t.Fatalf("missing %q: %s", want, text)
				}
			}
			if tc.name != "full" && strings.Contains(text, "implement started") {
				t.Fatal("partial execution announced implementation")
			}
		})
	}
}

func TestWorkProgressCanBeDisabledAndDryRunIsSilent(t *testing.T) {
	for _, flags := range [][]string{{"--progress=false", "--phase", "plan"}, {"--dry-run"}} {
		f := newCLIFixture(t)
		n := strconv.Itoa(f.create(t, "agent"))
		f.must(t, "issue", "approve", n)
		cmd := NewRootCmd()
		var output, diagnostics bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(&diagnostics)
		args := []string{"--root", f.root, "--backend", "local", "issue", "work", n, "--agent", "mock", "--json"}
		cmd.SetArgs(append(args, flags...))
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if diagnostics.Len() != 0 {
			t.Fatalf("unexpected progress: %s", diagnostics.String())
		}
	}
}

func TestWorkFailureReportsTerminalStageAndJSON(t *testing.T) {
	f := newCLIFixture(t)
	n := strconv.Itoa(f.create(t, "agent")) // Deliberately unapproved.
	cmd := NewRootCmd()
	var output, diagnostics bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&diagnostics)
	cmd.SetArgs([]string{"--root", f.root, "--backend", "local", "issue", "work", n, "--agent", "mock", "--json"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("unapproved work ran")
	}
	var outcome struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(output.Bytes(), &outcome); err != nil || outcome.Status != "failed" {
		t.Fatalf("invalid failure result: %s (%v)", output.String(), err)
	}
	if !strings.Contains(diagnostics.String(), "preflight failed") {
		t.Fatalf("missing terminal status: %s", diagnostics.String())
	}
}

func TestClaimSetupProgressJSON(t *testing.T) {
	for _, next := range []bool{false, true} {
		t.Run(strconv.FormatBool(next), func(t *testing.T) {
			f := newCLIFixture(t)
			n := strconv.Itoa(f.create(t, "agent"))
			f.must(t, "issue", "approve", n)
			cmd := NewRootCmd()
			var output, diagnostics bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&diagnostics)
			target := n
			if next {
				target = "--next"
			}
			cmd.SetArgs([]string{"--root", f.root, "--backend", "local", "issue", "claim", target, "--agent", "mock", "--json"})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("%v: %s", err, diagnostics.String())
			}
			decoder := json.NewDecoder(&output)
			var result map[string]any
			if err := decoder.Decode(&result); err != nil {
				t.Fatal(err)
			}
			var extra any
			if err := decoder.Decode(&extra); err != io.EOF {
				t.Fatalf("extra stdout: %v", err)
			}
			assertSetupProgress(t, diagnostics.String(), 1, 1)
		})
	}
}

func assertSetupProgress(t *testing.T, text string, claims, workspaces int) {
	t.Helper()
	previous := -1
	for _, phase := range []string{"claim", "workspace", "bundle"} {
		index := strings.Index(text, phase+" started")
		if index <= previous {
			t.Fatalf("setup order %s: %s", phase, text)
		}
		previous = index
	}
	for phase, count := range map[string]int{"claim": claims, "workspace": workspaces} {
		if got := strings.Count(text, phase+" started"); got != count {
			t.Fatalf("%s starts=%d want=%d: %s", phase, got, count, text)
		}
	}
}

func TestIssueWorkSetupProgressModes(t *testing.T) {
	for _, headless := range []bool{false, true} {
		for _, next := range []bool{false, true} {
			for _, enabled := range []bool{false, true} {
				t.Run(strconv.FormatBool(headless)+"/"+strconv.FormatBool(next)+"/"+strconv.FormatBool(enabled), func(t *testing.T) {
					f := newCLIFixture(t)
					n := strconv.Itoa(f.create(t, "none"))
					f.must(t, "issue", "approve", n)
					cmd := NewRootCmd()
					var output, diagnostics bytes.Buffer
					cmd.SetOut(&output)
					cmd.SetErr(&diagnostics)
					target := n
					if next {
						target = "--next"
					}
					cmd.SetArgs([]string{"--root", f.root, "--backend", "local", "issue", "work", target, "--agent", "mock", "--phase", "plan", "--headless=" + strconv.FormatBool(headless), "--progress=" + strconv.FormatBool(enabled)})
					if err := cmd.Execute(); err != nil {
						t.Fatalf("%v: %s", err, diagnostics.String())
					}
					if enabled {
						count := 1
						if next {
							count = 2
						}
						assertSetupProgress(t, diagnostics.String(), count, count)
					} else if diagnostics.Len() != 0 {
						t.Fatal(diagnostics.String())
					}
					if strings.Contains(output.String(), " started (") {
						t.Fatal("progress on stdout")
					}
					if !headless && strings.Contains(diagnostics.String(), "plan started") {
						t.Fatal("interactive execution reporting")
					}
				})
			}
		}
	}
}

func TestPRWorkSetupProgressModes(t *testing.T) {
	for _, headless := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			t.Run(strconv.FormatBool(headless)+"/"+strconv.FormatBool(enabled), func(t *testing.T) {
				f, _, number := submittedPRFixture(t, "none")
				// Repeat to cover fresh acquisition followed by persisted claim resumption.
				for range 2 {
					cmd := NewRootCmd()
					var output, diagnostics bytes.Buffer
					cmd.SetOut(&output)
					cmd.SetErr(&diagnostics)
					cmd.SetArgs([]string{"--root", f.root, "--backend", "local", "pr", "work", strconv.Itoa(number), "--agent", "mock", "--phase", "plan", "--headless=" + strconv.FormatBool(headless), "--progress=" + strconv.FormatBool(enabled)})
					if err := cmd.Execute(); err != nil {
						t.Fatalf("%v: %s", err, diagnostics.String())
					}
					if enabled {
						assertSetupProgress(t, diagnostics.String(), 2, 1)
						if headless && !strings.Contains(diagnostics.String(), "workspace-check started") {
							t.Fatal(diagnostics.String())
						}
					} else if diagnostics.Len() != 0 {
						t.Fatal(diagnostics.String())
					}
					if strings.Contains(output.String(), " started (") {
						t.Fatal("progress on stdout")
					}
				}
			})
		}
	}
}

func TestClaimSetupProgressStopsAtFailure(t *testing.T) {
	f := newCLIFixture(t)
	n := strconv.Itoa(f.create(t, "none"))
	f.must(t, "issue", "approve", n)
	f.must(t, "issue", "claim", n, "--agent", "mock")
	cmd := NewRootCmd()
	var output, diagnostics bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&diagnostics)
	cmd.SetArgs([]string{"--root", f.root, "--backend", "local", "issue", "claim", n, "--agent", "mock", "--json"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("acquired an active claim")
	}
	text := diagnostics.String()
	if !strings.Contains(text, "claim started") || strings.Contains(text, "workspace started") || strings.Contains(text, "bundle started") {
		t.Fatalf("failure progress: %s", text)
	}
}
