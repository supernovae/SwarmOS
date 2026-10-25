package cli

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/fork"
	"github.com/supernovae/swarmos/internal/workflow"
)

func TestForkProgressPrinter(t *testing.T) {
	var out bytes.Buffer
	print := forkProgressPrinter(&out)
	for _, status := range []string{"posted", "waiting", "accepted", "rejected"} {
		print(fork.ProgressEvent{Operation: "acquire", RequestID: 100, Status: status, Elapsed: 30500 * time.Millisecond})
	}
	want := "Fork acquire request 100: waiting on the upstream fork coordinator Action\nFork acquire request 100: waiting on the upstream fork coordinator Action (elapsed 30s)\nFork acquire request 100: accepted (elapsed 30s)\nFork acquire request 100: rejected (elapsed 30s)\n"
	if out.String() != want {
		t.Fatalf("%q", out.String())
	}
	out.Reset()
	print(fork.ProgressEvent{Operation: "secret\n\x1btoken", RequestID: 100, Status: "posted"})
	print(fork.ProgressEvent{Operation: "acquire", RequestID: 100, Status: "secret\n\x1btoken"})
	if strings.Contains(out.String(), "secret") || out.String() != "Fork unknown request 100: waiting on the upstream fork coordinator Action\n" {
		t.Fatal(out.String())
	}
}

func TestForkProgressConcurrentWorkflowOutput(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetErr(&out)
	w := progressWriter(cmd)
	if w != progressWriter(cmd) {
		t.Fatal("writer not shared")
	}
	fp, wp := forkProgressPrinter(w), workProgressPrinter(progressWriter(cmd))
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 100 {
			fp(fork.ProgressEvent{Operation: "renew", RequestID: 100, Status: "waiting"})
		}
	})
	wg.Go(func() {
		for range 100 {
			wp(workflow.ProgressEvent{IssueNumber: 42, Stage: "plan", Status: "running"})
		}
	})
	wg.Wait()
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 200 {
		t.Fatal(len(lines))
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "Fork renew request 100:") && !strings.HasPrefix(line, "Issue #42: plan running") {
			t.Fatal(line)
		}
	}
}
