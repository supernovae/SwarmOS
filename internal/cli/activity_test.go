package cli

import (
	"bytes"
	"encoding/json"
	"github.com/supernovae/swarmos/internal/progress"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWorkProgressPrintsCoordinatorOperation(t *testing.T) {
	for _, printer := range []func(*bytes.Buffer) func(progress.Event){func(b *bytes.Buffer) func(progress.Event) { return workProgressPrinter(b) }, func(b *bytes.Buffer) func(progress.Event) { return operationProgressPrinter(b) }} {
		for _, tc := range []struct {
			e    progress.Event
			want string
		}{
			{progress.Event{Role: "reviewer", Tool: "claude", ActivityKnown: true, ActivityAge: 12 * time.Second}, "; reviewer via claude, last activity 12s ago"},
			{progress.Event{Role: "reviewer", Tool: "mock"}, "; reviewer via mock, last activity unknown"},
			{progress.Event{Operation: "state read", OperationElapsed: 45 * time.Second}, "; coordinator state read for 45s"},
		} {
			var b bytes.Buffer
			tc.e.Status = "running"
			printer(&b)(tc.e)
			if !strings.Contains(b.String(), tc.want) {
				t.Fatal(b.String())
			}
			tc.e.Status = "completed"
			if activitySuffix(tc.e) != "" {
				t.Fatal("transition suffix")
			}
		}
		for _, bad := range []string{"reviewer --token=ghp_x", "../../etc", "\n\x1b[31m", "git push https://x:tok@h"} {
			var b bytes.Buffer
			printer(&b)(progress.Event{Status: "running", Role: bad, Tool: bad, Operation: bad})
			if strings.Contains(b.String(), bad) {
				t.Fatal(b.String())
			}
		}
	}
}

func TestReviewPlanProgressKeepsJSONOnStdout(t *testing.T) {
	f := newCLIFixture(t)
	n := strconv.Itoa(f.create(t, "agent"))
	f.must(t, "issue", "approve", n)
	f.must(t, "issue", "work", n, "--agent", "mock", "--phase", "plan")
	cmd := f.command(nil)
	var output, diag bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&diag)
	cmd.SetArgs([]string{"--root", f.root, "--backend", "local", "issue", "review-plan", n, "--agent", "mock", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%v: %s", err, diag.String())
	}
	dec := json.NewDecoder(&output)
	var document map[string]any
	if err := dec.Decode(&document); err != nil {
		t.Fatal(err)
	}
	if document["status"] != "completed" {
		t.Fatal(document)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		t.Fatalf("extra stdout: %v", err)
	}
	if !strings.Contains(diag.String(), "review started") || !strings.Contains(diag.String(), "review completed") {
		t.Fatal(diag.String())
	}
}
