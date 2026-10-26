package cli

import (
	"errors"
	"fmt"
	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/lease"
)

func newCleanupCmd(opts *RootOptions) *cobra.Command {
	var apply, asJSON, recoverReservation bool
	cmd := &cobra.Command{Use: "cleanup <issue-number>", Short: "Preview or retire verified merged task worktrees and branches", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) (runErr error) {
		var preview *lease.CleanupPreview
		defer func() {
			if !asJSON {
				return
			}
			status := "preview"
			if apply {
				status = "completed"
			}
			if runErr != nil {
				status = "failed"
			}
			var output any
			if preview == nil {
				data := map[string]any{"kind": "cleanup", "status": status}
				if runErr != nil {
					data["error"] = runErr.Error()
				}
				output = data
			} else {
				preview.Status = status
				if runErr != nil {
					preview.Error = runErr.Error()
				}
				output = preview
			}
			if e := writeJSON(cmd, output); runErr == nil {
				runErr = e
			}
		}()

		n, err := parseNumberArg(args[0], "issue")
		if err != nil {
			return err
		}
		if opts.Fork {
			return fmt.Errorf("cleanup requires an upstream repository operator")
		}
		tr, m, _, err := opts.resolveLeaseManager(cmd)
		if err != nil {
			return err
		}
		if apply && m.HasWorkerGateway() {
			return fmt.Errorf("cleanup is unavailable to agent workers")
		}
		if recoverReservation {
			if apply {
				return fmt.Errorf("--recover-reservation and --apply are mutually exclusive")
			}
			if err = m.RecoverCleanup(cmd.Context(), n); err != nil {
				return err
			}
		}
		root := opts.resolveRepoRoot()
		preview, err = lease.InspectCleanup(cmd.Context(), root, tr, m, n)
		if err != nil {
			return err
		}
		if !asJSON {
			for _, r := range preview.Resources {
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s: eligible=%t absent=%t expected=%s: %s\n", r.Kind, r.Name, r.Eligible, r.Absent, r.ExpectedHead, r.Reason)
			}
		}
		if !apply {
			return nil
		}
		if err = tr.Authorize(cmd.Context(), "approve-task"); err != nil {
			return err
		}
		if preview.Blocked != "" {
			return fmt.Errorf("cleanup refused: %s", preview.Blocked)
		}
		remote := preview.Resources[0]
		if preview.RemoteApplicable && !remote.Absent && !remote.Eligible {
			return fmt.Errorf("cleanup refused: %s", remote.Reason)
		}
		reservation, err := m.BeginCleanup(cmd.Context(), n, preview.Outcome)
		if err != nil {
			return err
		}
		var actionErr error
		if preview.RemoteApplicable {
			actionErr = m.DeleteCleanupBranch(cmd.Context(), n, reservation.OperationID)
			if actionErr == nil && !remote.Absent {
				preview.RemoteDeleted = true
				preview.Resources[0].Retired = true
				preview.Resources[0].Eligible = false
				preview.Resources[0].Reason = "retired"
			}
		}
		if actionErr == nil {
			actionErr = m.RetireCleanupLocalResources(cmd.Context(), n, reservation.OperationID, preview)
		}
		finishErr := m.FinishCleanup(cmd.Context(), n, reservation.OperationID)
		if !asJSON {
			for _, r := range preview.Resources {
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s: %s\n", r.Kind, r.Name, r.Reason)
			}
		}
		if actionErr != nil || finishErr != nil {
			return fmt.Errorf("cleanup incomplete; inspect resources before retry; recover reservation if necessary: %w", errors.Join(actionErr, finishErr))
		}
		return nil
	}}
	cmd.Flags().BoolVar(&recoverReservation, "recover-reservation", false, "Fence and release an interrupted cleanup before previewing again")
	cmd.Flags().BoolVar(&apply, "apply", false, "Retire eligible merged task worktrees and code branches; retain ledgers")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit structured cleanup preview")
	return cmd
}
