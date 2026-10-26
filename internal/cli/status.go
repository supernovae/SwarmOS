package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
)

type issueStatus struct {
	ClaimBudget *lease.ClaimBudget `json:"claim_budget,omitempty"`
	// Set for a fork-mode claim whose upstream coordinator predates budgets.
	ClaimBudgetUnavailable bool                       `json:"claim_budget_unavailable,omitempty"`
	PlanReplanLimit        int                        `json:"plan_replan_limit"`
	Checkpoint             *workspace.CheckpointState `json:"checkpoint,omitempty"`
	Repository             string                     `json:"repository"`
	Issue                  *tracker.Issue             `json:"issue"`
	State                  *lease.IssueState          `json:"state"`
	Provenance             []contract.RoleAttestation `json:"provenance"`
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
				entry := issueStatus{Repository: target.Repo, Issue: issue, State: state, Provenance: provenance}
				if state != nil {
					entry.ClaimBudget, err = manager.Budget(state)
					if err != nil {
						return err
					}
					entry.ClaimBudgetUnavailable = entry.ClaimBudget == nil && manager.HasWorkerGateway() && !state.Lease.ClaimedAt.IsZero()
					entry.PlanReplanLimit, err = manager.PlanReplanLimit(state)
					if err != nil {
						return err
					}
				}
				if issueNumber != 0 && state != nil && state.Branch != "" {
					remote := manager.UpstreamRemote()
					if options.Fork {
						remote = options.effectiveForkRemote()
					}
					observed := workspace.InspectCheckpoint(cmd.Context(), target.Path, remote, issueNumber, state.Branch, state.HeadSHA)
					entry.Checkpoint = &observed
				}
				entries = append(entries, entry)
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
			if b := entry.ClaimBudget; b != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "Claim budget: %d renewals used, %d remaining; total allowance left=%s deadline=%s; available=%s current expiry=%s (active=%t)\n", b.RenewalsUsed, b.RenewalsRemaining, b.Remaining, b.Deadline.Format(time.RFC3339), b.Available, state.Lease.ExpiresAt.Format(time.RFC3339), b.Active)
			} else if entry.ClaimBudgetUnavailable {
				fmt.Fprintln(cmd.OutOrStdout(), "claim budget: unavailable from upstream coordinator")
			}
			if o := state.PROutcome; o != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "PR outcome: %s (merge=%s)\n", o.Status, o.MergeCommitSHA)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Automatic plan retries: %d/%d\n", state.PlanReplanCount, entry.PlanReplanLimit)
			if state.NextPhaseReason != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Next phase: %s (%s)\n", state.Lease.NextPhase, state.NextPhaseReason)
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
				fmt.Fprint(cmd.OutOrStdout(), lease.PlanSummary(state, false))
				if entry.Checkpoint != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "Checkpoint: %s\n", entry.Checkpoint.String())
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Base: %s\nTask: %s\nApproved by: %s\n", state.BaseSHA, state.TaskDigest, state.ApprovedBy)
				if fs := state.FreshStart; fs != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "Fresh start: next claim begins on %s at the approved base\n", fs.Branch)
				}
				for _, d := range state.DiscardedCheckpoints {
					fmt.Fprintf(cmd.OutOrStdout(), "Discarded checkpoint: %s (branch %s, base %s) by %s at %s; retained at %s\n", d.HeadSHA, d.Branch, d.BaseSHA, d.DiscardedBy, d.DiscardedAt.Format(time.RFC3339), d.RetentionRef)
				}
				for _, artifact := range state.Artifacts {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s: %s (%s)\n", artifact.Type, artifact.Verdict, artifact.Producer)
					fmt.Fprintf(cmd.OutOrStdout(), "    %s\n", artifact.Summary)
					if artifact.Metadata["operator_steered"] == "true" {
						fmt.Fprintf(cmd.OutOrStdout(), "    Operator-steered: %s messages; SHA-256 %s\n", artifact.Metadata["guidance_count"], artifact.Metadata["guidance_sha256"])
					}
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
