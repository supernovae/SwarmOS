package cli

import (
	"context"
	"fmt"
	"github.com/supernovae/swarmos/internal/progress"
	"io"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/fork"
	"time"

	"github.com/supernovae/swarmos/internal/workflow"
)

func workProgressPrinter(out io.Writer) func(workflow.ProgressEvent) {
	var mu sync.Mutex
	return func(event workflow.ProgressEvent) {
		mu.Lock()
		defer mu.Unlock()
		// Stage and status come from coordinator-owned enums, never child
		// output; check names come from the trusted policy and are quoted.
		stage := event.Stage
		if event.RepairLimit > 0 {
			stage = fmt.Sprintf("repair %d/%d %s", event.Repair, event.RepairLimit, stage)
		}
		if event.Check != "" {
			fmt.Fprintf(out, "Issue #%d: %s check %+q %s (check %s, elapsed %s)\n",
				event.IssueNumber, stage, event.Check, event.Status,
				event.StageElapsed.Truncate(time.Second), event.Elapsed.Truncate(time.Second))
			return
		}
		targets := ""
		if event.Repair > 0 && event.Stage == "implement" && event.Status == "started" && len(event.Targets) > 0 {
			// Targets are diagnostics from check output; quote each one.
			quoted := make([]string, len(event.Targets))
			for i, target := range event.Targets {
				quoted[i] = fmt.Sprintf("%+q", target)
			}
			targets = " targeting " + strings.Join(quoted, ", ")
		}
		fmt.Fprintf(out, "Issue #%d: %s %s%s (stage %s, elapsed %s)\n",
			event.IssueNumber, stage, event.Status, targets,
			event.StageElapsed.Truncate(time.Second), event.Elapsed.Truncate(time.Second))
	}
}

func operationProgress(ctx context.Context, out io.Writer, enabled bool) *progress.Session {
	var observer func(progress.Event)
	if enabled {
		observer = operationProgressPrinter(out)
	}
	return progress.Start(ctx, 0, 0, observer)
}
func operationProgressPrinter(out io.Writer) func(progress.Event) {
	return func(e progress.Event) {
		if e.Check != "" {
			fmt.Fprintf(out, "Check %s %s (elapsed %s)\n", fmt.Sprintf("%+q", e.Check), e.Status, e.StageElapsed.Truncate(time.Second))
		} else {
			fmt.Fprintf(out, "%s %s (elapsed %s)\n", e.Stage, e.Status, e.StageElapsed.Truncate(time.Second))
		}
	}
}

// diagnosticWriter is shared by all reporters attached to one command.
type diagnosticWriter struct {
	mu  sync.Mutex
	out io.Writer
}

func (w *diagnosticWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.Write(b)
}
func progressWriter(cmd *cobra.Command) io.Writer {
	if w, ok := cmd.ErrOrStderr().(*diagnosticWriter); ok {
		return w
	}
	w := &diagnosticWriter{out: cmd.ErrOrStderr()}
	cmd.SetErr(w)
	return w
}

func forkProgressPrinter(out io.Writer) func(fork.ProgressEvent) {
	return func(event fork.ProgressEvent) {
		operation := "unknown"
		for _, allowed := range fork.Operations {
			if event.Operation == allowed {
				operation = allowed
				break
			}
		}
		elapsed := event.Elapsed.Truncate(time.Second)
		switch event.Status {
		case "posted":
			fmt.Fprintf(out, "Fork %s request %d: waiting on the upstream fork coordinator Action\n", operation, event.RequestID)
		case "waiting":
			fmt.Fprintf(out, "Fork %s request %d: waiting on the upstream fork coordinator Action (elapsed %s)\n", operation, event.RequestID, elapsed)
		case "accepted", "rejected":
			fmt.Fprintf(out, "Fork %s request %d: %s (elapsed %s)\n", operation, event.RequestID, event.Status, elapsed)
		}
	}
}

func baseChangePrinter(out io.Writer) func(int, string, string) {
	return func(n int, old, base string) {
		fmt.Fprintf(out, "Issue #%d: base %s -> %s\n", n, old, base)
	}
}
