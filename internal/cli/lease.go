package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/lease"
)

func newRenewCmd(options *RootOptions) *cobra.Command   { return newLeaseMutationCmd(options, true) }
func newReleaseCmd(options *RootOptions) *cobra.Command { return newLeaseMutationCmd(options, false) }

func newLeaseMutationCmd(options *RootOptions, renew bool) *cobra.Command {
	name, summary := "release", "Release this machine's fenced claim back to the queue"
	if renew {
		name, summary = "renew", "Renew this machine's fenced claim under approved policy"
	}
	var durationString, reason string
	var asJSON bool
	cmd := &cobra.Command{Use: name + " <issue-number>", Short: summary, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) (runErr error) {
		defer func() { reportJSONFailure(cmd, asJSON, name, runErr) }()
		issue, err := parseNumberArg(args[0], "issue")
		if err != nil {
			return err
		}
		var duration time.Duration
		if durationString != "" {
			duration, err = time.ParseDuration(durationString)
			if err != nil || duration <= 0 {
				return fmt.Errorf("--duration must be positive")
			}
		}
		tr, manager, _, err := options.resolveLeaseManager(cmd)
		if err != nil {
			return err
		}
		owner, _, err := bindPersistedClaim(cmd.Context(), options.resolveRepoRoot(), tr, manager, issue)
		if err != nil {
			return err
		}
		state, err := manager.GetCurrentState(cmd.Context(), issue)
		if err != nil {
			return err
		}
		if renew {
			cfg, approved, loadErr := approvedLeasePolicy(options.resolveRepoRoot(), manager.UpstreamRemote(), state.BaseSHA)
			if loadErr != nil {
				return loadErr
			}
			if !cfg.Lease.Renewable {
				return fmt.Errorf("approved policy disallows renewal")
			}
			if duration == 0 {
				duration = approved
			}
			state, err = manager.Renew(cmd.Context(), issue, owner, duration)
		} else {
			lock, lockErr := lease.LockClaim(options.resolveRepoRoot(), issue)
			if lockErr != nil {
				return lockErr
			}
			defer lease.UnlockClaim(lock)
			state, err = manager.Release(cmd.Context(), issue, owner, reason)
		}
		if err != nil {
			return err
		}
		if asJSON {
			return writeJSON(cmd, map[string]any{"kind": name, "issue_number": issue, "state": state})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Issue #%d: %s (expires %s)\n", issue, state.Lease.Status, state.Lease.ExpiresAt.Format(time.RFC3339))
		return nil
	}}
	if renew {
		cmd.Flags().StringVar(&durationString, "duration", "", "Positive renewal duration (approved policy default)")
	} else {
		cmd.Flags().StringVar(&reason, "reason", "", "Reason for release")
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit a structured lease outcome")
	return cmd
}
