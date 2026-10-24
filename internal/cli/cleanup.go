package cli

import (
	"errors"
	"fmt"
	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/lease"
)

func newCleanupCmd(opts *RootOptions) *cobra.Command {
	var apply, asJSON, recoverReservation bool
	cmd := &cobra.Command{Use: "cleanup <issue-number>", Short: "Preview retained task resources or retire a verified merged remote branch", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) (runErr error) {
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
		remote := preview.Resources[0]
		if remote.Absent {
			return nil
		}
		if !remote.Eligible {
			return fmt.Errorf("cleanup refused: %s", remote.Reason)
		}
		reservation, err := m.BeginCleanup(cmd.Context(), n, preview.Outcome)
		if err != nil {
			return err
		}
		deleteErr := m.DeleteCleanupBranch(cmd.Context(), n, reservation.OperationID)
		// The remote action is a synchronous atomic transaction. Once it returns,
		// releasing this token cannot let a delayed action publish over a successor.
		finishErr := m.FinishCleanup(cmd.Context(), n, reservation.OperationID)
		if deleteErr != nil {
			return fmt.Errorf("remote cleanup failed; inspect resources before retry: %w", errors.Join(deleteErr, finishErr))
		}
		preview.RemoteDeleted = true
		if finishErr != nil {
			return fmt.Errorf("remote cleanup completed; reservation release failed: %w", finishErr)
		}
		if !asJSON {
			fmt.Fprintln(cmd.OutOrStdout(), "Remote task branch retired; local resources and provenance retained.")
		}
		return nil
	}}
	cmd.Flags().BoolVar(&recoverReservation, "recover-reservation", false, "Fence and release an interrupted remote cleanup before previewing again")
	cmd.Flags().BoolVar(&apply, "apply", false, "Retire the eligible remote task branch; retain local resources and ledgers")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit structured cleanup preview")
	return cmd
}
