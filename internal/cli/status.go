package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
)

type issueStatus struct {
	Repository string            `json:"repository"`
	Issue      *tracker.Issue    `json:"issue"`
	State      *lease.IssueState `json:"state"`
}

func newStatusCmd(options *RootOptions) *cobra.Command {
	var all, asJSON bool
	cmd := &cobra.Command{Use: "status [<issue-number>]", Short: "Inspect authoritative task approvals, claims, artifacts, and submissions", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if all && len(args) != 0 {
			return fmt.Errorf("--all and an issue number are mutually exclusive")
		}
		var issueNumber int
		if len(args) != 0 {
			var err error
			issueNumber, err = strconv.Atoi(strings.TrimPrefix(args[0], "#"))
			if err != nil || issueNumber <= 0 {
				return fmt.Errorf("invalid issue number %q", args[0])
			}
		}
		targets, err := options.resolveTargets(all)
		if err != nil {
			return err
		}
		entries := make([]issueStatus, 0)
		for _, target := range targets {
			tr, manager, _, err := options.resolveTargetLeaseManager(cmd, target)
			if err != nil {
				return err
			}
			var issues []*tracker.Issue
			if issueNumber != 0 {
				issue, err := tr.GetIssue(cmd.Context(), issueNumber)
				if err != nil {
					return err
				}
				issues = []*tracker.Issue{issue}
			} else {
				issues, err = tr.ListIssues(cmd.Context(), tracker.IssueFilter{State: "open"})
				if err != nil {
					return err
				}
			}
			for _, issue := range issues {
				state, err := manager.GetCurrentState(cmd.Context(), issue.Number)
				if err != nil && !errors.Is(err, coordination.ErrNotFound) {
					return fmt.Errorf("read %s #%d state: %w", target.Repo, issue.Number, err)
				}
				entries = append(entries, issueStatus{Repository: target.Repo, Issue: issue, State: state})
			}
		}
		if asJSON {
			return writeJSON(cmd, map[string]any{"kind": "status", "issues": entries})
		}
		for _, entry := range entries {
			if entry.State == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "%s #%d draft/unapproved: %s\n", entry.Repository, entry.Issue.Number, entry.Issue.Title)
				continue
			}
			state := entry.State
			status := state.Lease.Status
			if status == "" {
				status = "approved"
			}
			if status == "active" && !state.Lease.ExpiresAt.After(options.now()) {
				status = "expired"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s #%d %s: owner=%s phase=%s gate=%s\n", entry.Repository, entry.Issue.Number, status, state.Lease.Owner, state.Lease.Phase, state.EffectiveGate)
			if issueNumber != 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "Base: %s\nTask: %s\nApproved by: %s\n", state.BaseSHA, state.TaskDigest, state.ApprovedBy)
				for _, artifact := range state.Artifacts {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s: %s (%s)\n", artifact.Type, artifact.Verdict, artifact.Producer)
				}
				if state.PRNumber != 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "PR #%d: %s\n", state.PRNumber, state.PRURL)
				}
			}
		}
		if len(entries) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No open tasks.")
		}
		return nil
	}}
	cmd.Flags().BoolVar(&all, "all", false, "Inspect all registered repositories")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit structured authoritative status")
	return cmd
}

func newRenewCmd(options *RootOptions) *cobra.Command   { return newLeaseMutationCmd(options, true) }
func newReleaseCmd(options *RootOptions) *cobra.Command { return newLeaseMutationCmd(options, false) }

func newLeaseMutationCmd(options *RootOptions, renew bool) *cobra.Command {
	name, summary := "release", "Release this machine's fenced claim back to the queue"
	if renew {
		name, summary = "renew", "Renew this machine's fenced claim under approved policy"
	}
	var durationString, reason string
	var asJSON bool
	cmd := &cobra.Command{Use: name + " <issue-number>", Short: summary, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		issue, err := strconv.Atoi(strings.TrimPrefix(args[0], "#"))
		if err != nil || issue <= 0 {
			return fmt.Errorf("invalid issue number %q", args[0])
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
		owner, err := tr.CurrentUser(cmd.Context())
		if err != nil {
			return err
		}
		claim, err := loadClaimToken(options.resolveRepoRoot(), issue)
		if err != nil {
			return err
		}
		manager.BindClaim(issue, claim)
		state, err := manager.GetCurrentState(cmd.Context(), issue)
		if err != nil {
			return err
		}
		if renew {
			_ = workspace.FetchCommit(options.resolveRepoRoot(), "origin", state.BaseSHA)
			cfg, loadErr := harness.LoadConfigAt(options.resolveRepoRoot(), state.BaseSHA)
			if loadErr != nil {
				return loadErr
			}
			if !cfg.Lease.Renewable {
				return fmt.Errorf("approved policy disallows renewal")
			}
			if duration == 0 {
				duration, err = time.ParseDuration(cfg.Lease.Duration)
				if err != nil {
					return err
				}
			}
			state, err = manager.Renew(cmd.Context(), issue, owner, duration)
		} else {
			lock, lockErr := lockClaim(options.resolveRepoRoot(), issue)
			if lockErr != nil {
				return lockErr
			}
			defer unlockClaim(lock)
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
