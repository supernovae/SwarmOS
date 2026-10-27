package cli

import (
	"fmt"

	"github.com/supernovae/swarmos/internal/workflow"
)

// splitGuidance explains a run that stopped because the recorded plan
// recommends a split, and records the decompose command on the result.
// Decomposition always targets the linked issue, never a PR number.
func splitGuidance(opts *RootOptions, result *workflow.WorkResult, issue int) string {
	if result == nil || !result.SplitRecommended {
		return ""
	}
	result.DecomposeCommand = swarmCommand(opts) + fmt.Sprintf(" issue decompose %d", issue)
	return fmt.Sprintf("The planner recommends a split: issue #%d cannot finish within one claim. Review the proposed child WorkUnits in the recorded PLAN.md, then run: %s", issue, result.DecomposeCommand)
}
