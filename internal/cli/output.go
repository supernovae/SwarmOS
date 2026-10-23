package cli

import (
	"encoding/json"

	"github.com/spf13/cobra"
)

func writeJSON(cmd *cobra.Command, value any) error {
	return json.NewEncoder(cmd.OutOrStdout()).Encode(value)
}

// reportJSONFailure emits one structured failure document when a --json
// command returns an error, so scripted callers always receive JSON.
func reportJSONFailure(cmd *cobra.Command, asJSON bool, kind string, err error) {
	if asJSON && err != nil {
		_ = writeJSON(cmd, map[string]any{"kind": kind, "status": "failed", "error": err.Error()})
	}
}
