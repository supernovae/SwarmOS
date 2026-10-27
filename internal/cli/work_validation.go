package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workflow"
)

// failedWorkValidation returns the report of a work run whose required checks
// failed, or nil when the run never validated or passed.
func failedWorkValidation(result *workflow.WorkResult) *validation.ValidationReport {
	if result == nil || result.Validation == nil || result.Validation.Passed {
		return nil
	}
	return result.Validation
}

// writeWorkValidationFailure prints the diagnostics of the failed checks of a
// work run. Work progress already reported passing checks on stderr, so only
// failed outcomes are shown, through the shared validation renderer.
func writeWorkValidationFailure(cmd *cobra.Command, result *workflow.WorkResult) {
	report := failedWorkValidation(result)
	if report == nil {
		return
	}
	failed := *report
	failed.CheckOutcomes = nil
	for _, check := range report.CheckOutcomes {
		if !check.Passed && check.FallbackReason == "" {
			failed.CheckOutcomes = append(failed.CheckOutcomes, check)
		}
	}
	writeValidationChecks(cmd, &failed)
	fmt.Fprint(cmd.OutOrStdout(), validation.RepairStatusText(report.Repair))
}
