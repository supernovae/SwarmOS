package cli

import (
	"fmt"
	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/lease"
)

func newReconcileCmd(opts *RootOptions) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{Use: "reconcile <issue-number>", Short: "Record the linked PR outcome and repair issue projections", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) (runErr error) {
		var state *lease.IssueState
		defer func() {
			if !asJSON {
				return
			}
			data := map[string]any{"kind": "reconcile", "status": "completed", "state": state}
			if runErr != nil {
				data["status"] = "failed"
				data["error"] = runErr.Error()
			}
			if e := writeJSON(cmd, data); runErr == nil {
				runErr = e
			}
		}()

		n, err := parseNumberArg(args[0], "issue")
		if err != nil {
			return err
		}
		if opts.Fork {
			return fmt.Errorf("reconciliation requires an upstream repository operator")
		}
		_, m, _, err := opts.resolveLeaseManager(cmd)
		if err != nil {
			return err
		}
		state, err = m.ReconcileSubmission(cmd.Context(), n)
		if err != nil {
			return err
		}
		if !asJSON {
			fmt.Fprintf(cmd.OutOrStdout(), "Issue #%d PR #%d: %s\n", n, state.PRNumber, state.PROutcome.Status)
		}
		return nil
	}}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit structured reconciliation results")
	return cmd
}
