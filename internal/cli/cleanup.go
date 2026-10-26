package cli

import (
	"context"
	"errors"
	"fmt"
	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
)

func newCleanupCmd(opts *RootOptions) *cobra.Command {
	var apply, asJSON, recoverReservation, allMerged bool
	cmd := &cobra.Command{Use: "cleanup <issue-number> | --all-merged", Short: "Preview or retire verified merged task worktrees and branches", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) (runErr error) {
		if allMerged {
			return runBulkCleanup(cmd, opts, args, apply, asJSON, recoverReservation)
		}
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

		if len(args) != 1 {
			return fmt.Errorf("cleanup requires an issue number or --all-merged")
		}
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
		started, err := applyIssueCleanup(cmd.Context(), tr, m, n, preview)
		if started && !asJSON {
			for _, r := range preview.Resources {
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s: %s\n", r.Kind, r.Name, r.Reason)
			}
		}
		return err
	}}
	cmd.Flags().BoolVar(&recoverReservation, "recover-reservation", false, "Fence and release an interrupted cleanup before previewing again")
	cmd.Flags().BoolVar(&apply, "apply", false, "Retire eligible merged task worktrees and code branches; retain ledgers")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit structured cleanup preview")
	cmd.Flags().BoolVar(&allMerged, "all-merged", false, "Preview or retire every local issue and PR worktree's merged task resources")
	return cmd
}

// applyIssueCleanup retires the eligible resources of a fresh preview under a
// cleanup reservation and records the outcome in preview. started reports
// whether the reservation was taken, after which resources may have changed.
func applyIssueCleanup(ctx context.Context, tr tracker.Tracker, m *lease.Manager, n int, preview *lease.CleanupPreview) (started bool, err error) {
	if err = tr.Authorize(ctx, "approve-task"); err != nil {
		return false, err
	}
	if preview.Blocked != "" {
		return false, fmt.Errorf("cleanup refused: %s", preview.Blocked)
	}
	remote := preview.Resources[0]
	if preview.RemoteApplicable && !remote.Absent && !remote.Eligible {
		return false, fmt.Errorf("cleanup refused: %s", remote.Reason)
	}
	reservation, err := m.BeginCleanup(ctx, n, preview.Outcome)
	if err != nil {
		return false, err
	}
	var actionErr error
	if preview.RemoteApplicable {
		actionErr = m.DeleteCleanupBranch(ctx, n, reservation.OperationID)
		if actionErr == nil && !remote.Absent {
			preview.RemoteDeleted = true
			preview.Resources[0].Retired = true
			preview.Resources[0].Eligible = false
			preview.Resources[0].Reason = "retired"
		}
	}
	if actionErr == nil {
		actionErr = m.RetireCleanupLocalResources(ctx, n, reservation.OperationID, preview)
	}
	finishErr := m.FinishCleanup(ctx, n, reservation.OperationID)
	if actionErr != nil || finishErr != nil {
		return true, fmt.Errorf("cleanup incomplete; inspect resources before retry; recover reservation if necessary: %w", errors.Join(actionErr, finishErr))
	}
	return true, nil
}
