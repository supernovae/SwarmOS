package cli

import (
	"context"
	"fmt"
	"github.com/supernovae/swarmos/internal/progress"
	"io"
	"time"

	"github.com/supernovae/swarmos/internal/workflow"
)

func workProgressPrinter(out io.Writer) func(workflow.ProgressEvent) {
	return func(event workflow.ProgressEvent) {
		// All text fields come from coordinator-owned enums, never child output.
		fmt.Fprintf(out, "Issue #%d: %s %s (stage %s, elapsed %s)\n",
			event.IssueNumber, event.Stage, event.Status,
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
