package cli

import (
	"encoding/json"
	"errors"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/validation"
)

func writeJSON(cmd *cobra.Command, value any) error {
	return json.NewEncoder(cmd.OutOrStdout()).Encode(value)
}

// reportJSONFailure emits one structured failure document when a --json
// command returns an error, so scripted callers always receive JSON.
func reportJSONFailure(cmd *cobra.Command, asJSON bool, kind string, err error) {
	reportJSONFailureWithReport(cmd, asJSON, kind, err, nil)
}

// reportJSONFailureWithReport is reportJSONFailure with the failed validation
// report, when there is one, under the document's report key.
func reportJSONFailureWithReport(cmd *cobra.Command, asJSON bool, kind string, err error, report *validation.ValidationReport) {
	if asJSON && err != nil {
		outcome := map[string]any{"kind": kind, "status": "failed", "error": err.Error()}
		if report != nil {
			outcome["report"] = report
		}
		var recovery *planRecoveryError
		if errors.As(err, &recovery) {
			outcome["recovery"] = recovery.recovery
		}
		if action := nextActionOf(err); action != nil {
			outcome["next_action"] = action
		}
		_ = writeJSON(cmd, outcome)
	}
}
