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
			args := []string{"--root", f.root, "--backend", "local", "work", n, "--agent", "mock", "--json"}
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
		args := []string{"--root", f.root, "--backend", "local", "work", n, "--agent", "mock", "--json"}
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
	cmd.SetArgs([]string{"--root", f.root, "--backend", "local", "work", n, "--agent", "mock", "--json"})
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
