package cli

import (
	"fmt"
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
