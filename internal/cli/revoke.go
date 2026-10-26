package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// newRevokeCmd lets a maintainer end another actor's claim. It never binds a
// holder token, takes a holder-local lock, or forwards to the fork coordinator.
func newRevokeCmd(options *RootOptions) *cobra.Command {
	var reason string
	var asJSON bool
	cmd := &cobra.Command{Use: "revoke <issue-number>", Short: "End an active claim as a maintainer, keeping its checkpoints", Long: "revoke ends an issue's active same-repository or fork-coordinated claim through fenced compare-and-swap. It requires approve-task permission and the github backend, records the maintainer and reason in authoritative state, and posts an audit comment. Approval, artifacts, and checkpoints are retained so the next claimer can resume; the previous holder's claim-fenced writes fail.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) (runErr error) {
		defer func() { reportJSONFailure(cmd, asJSON, "revoke", runErr) }()
		issue, err := parseNumberArg(args[0], "issue")
		if err != nil {
			return err
		}
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("--reason is required")
		}
		if options.Fork {
			return fmt.Errorf("revoke is a maintainer command and is not available in --fork contributor mode")
		}
		tr, manager, _, err := options.resolveLeaseManager(cmd)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(tr.BackendName(), "github") {
			return fmt.Errorf("revoke requires the github backend")
		}
		state, projectionErr, err := manager.Revoke(cmd.Context(), issue, reason)
		if err != nil {
			return err
		}
		revoked := state.ClaimRevocations[len(state.ClaimRevocations)-1]
		if projectionErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: claim revoked, but the issue comment or labels were not updated: %v\n", projectionErr)
		}
		if asJSON {
			result := map[string]any{"kind": "revoke", "issue_number": issue, "revocation": revoked, "state": state}
			if projectionErr != nil {
				result["projection_warning"] = projectionErr.Error()
			}
			return writeJSON(cmd, result)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Issue #%d: revoked claim held by %s; checkpoints retained and the issue can be claimed again\n", issue, revoked.Owner)
		return nil
	}}
	cmd.Flags().StringVar(&reason, "reason", "", "Reason recorded in authoritative state and the audit comment (required)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit a structured revoke outcome")
	return cmd
}
