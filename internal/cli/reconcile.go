package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/lease"
)

func newReconcileCmd(opts *RootOptions) *cobra.Command {
	var asJSON, checkpoint, dryRun bool
	var expected, candidate string
	var reconciliation *lease.CheckpointReconciliation
	cmd := &cobra.Command{Use: "reconcile <issue-number>", Short: "Repair PR outcomes or explicitly reconcile a task checkpoint", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) (runErr error) {
		var state *lease.IssueState
		defer func() {
			if !asJSON {
				return
			}
			data := map[string]any{"kind": "reconcile", "status": "completed", "state": state}
			if reconciliation != nil {
				data["checkpoint_reconciliation"] = reconciliation
			}
			if runErr != nil {
				data["status"] = "failed"
				data["error"] = runErr.Error()
			}
			if e := writeJSON(cmd, data); runErr == nil {
				runErr = e
			}
		}()

		if !checkpoint && (expected != "" || candidate != "" || dryRun) {
			return fmt.Errorf("--expected-head, --candidate-head and --dry-run require --checkpoint")
		}
		if checkpoint && (expected == "" || candidate == "") {
			return fmt.Errorf("--checkpoint requires --expected-head and --candidate-head full SHAs")
		}
		n, err := parseNumberArg(args[0], "issue")
		if err != nil {
			return err
		}
		if opts.Fork {
			return fmt.Errorf("reconciliation requires an upstream repository operator; use the fork PR revision workflow")
		}
		tr, m, _, err := opts.resolveLeaseManager(cmd)
		if err != nil {
			return err
		}
		if checkpoint {
			root := opts.resolveRepoRoot()
			if !dryRun {
				lock, e := lease.LockClaim(root, n)
				if e != nil {
					return e
				}
				defer lease.UnlockClaim(lock)
			}
			// Dry run reads the persisted claim without locking or executing checks.
			// The manager verifies the live claim fence in either mode.
			owner, _, e := bindPersistedClaim(cmd.Context(), root, tr, m, n)
			if e != nil {
				return e
			}
			reconciliation, err = m.ReconcileCheckpoint(cmd.Context(), n, owner, expected, candidate, dryRun)
			if reconciliation != nil {
				state = reconciliation.State
			}
			if err != nil {
				return err
			}
			if !asJSON {
				if !dryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "Issue #%d checkpoint reconciled: %s -> %s\n", n, expected, candidate)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\nDry run: %t; validation required: %t; fresh plan review required: %t\n", reconciliation.Observation.String(), dryRun, reconciliation.ValidationRequired, reconciliation.ReviewRequired)
			}
			return nil
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
	cmd.Flags().BoolVar(&checkpoint, "checkpoint", false, "Explicitly reconcile a local task checkpoint with authoritative state")
	cmd.Flags().StringVar(&expected, "expected-head", "", "Full authoritative predecessor checkpoint SHA")
	cmd.Flags().StringVar(&candidate, "candidate-head", "", "Full clean local task candidate SHA")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Inspect checkpoint preconditions without executing checks or publishing")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit structured reconciliation results")
	return cmd
}
