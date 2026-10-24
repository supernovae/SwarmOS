package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
)

type issueStatus struct {
	Repository string                     `json:"repository"`
	Issue      *tracker.Issue             `json:"issue"`
	State      *lease.IssueState          `json:"state"`
	Provenance []contract.RoleAttestation `json:"provenance"`
}

func newStatusCmd(options *RootOptions) *cobra.Command {
	var all, asJSON bool
	cmd := &cobra.Command{Use: "status [<issue-number>]", Short: "Inspect authoritative task approvals, claims, artifacts, and submissions", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) (runErr error) {
		defer func() { reportJSONFailure(cmd, asJSON, "status", runErr) }()
		if all && len(args) != 0 {
			return fmt.Errorf("--all and an issue number are mutually exclusive")
		}
		var issueNumber int
		if len(args) != 0 {
			var err error
			issueNumber, err = parseNumberArg(args[0], "issue")
			if err != nil {
				return err
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
				provenance := make([]contract.RoleAttestation, 0)
				if state != nil {
					provenance = append(provenance, state.Attestations...)
				}
				entries = append(entries, issueStatus{Repository: target.Repo, Issue: issue, State: state, Provenance: provenance})
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
			if o := state.PROutcome; o != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "PR outcome: %s (merge=%s)\n", o.Status, o.MergeCommitSHA)
			}
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
				if len(entry.Provenance) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "Provenance: none recorded")
				} else {
					fmt.Fprintln(cmd.OutOrStdout(), "Provenance:")
					for _, record := range entry.Provenance {
						value := func(s string) string {
							if s == "" {
								return "-"
							}
							return s
						}
						fmt.Fprintf(cmd.OutOrStdout(), "  %s: Agent: %s; Harness: %s; Version: %s; Model: %s; Sandbox boundary: %s; Artifact type: %s; Artifact SHA-256: %s\n",
							record.Role, value(record.Agent), value(record.Harness), value(record.HarnessVersion), value(record.Model), value(record.Sandbox), value(record.ArtifactType), value(record.ArtifactDigest))
					}
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
