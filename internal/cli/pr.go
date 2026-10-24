package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/pr"
	"github.com/supernovae/swarmos/internal/tracker"
)

func newPRCmd(rootOpts *RootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pr",
		Short: "Develop, stage, rebase, validate, and update approved WorkUnit pull requests",
		Long: `Continue an existing PR linked to an approved WorkUnit. All arguments are
PR numbers, not issue numbers. work develops the implementation, including test
fixes and review feedback; rebase changes its branch ancestry. Both prepare local
changes in a separate PR worktree. For interactive plans, write PLAN.md and
TEST_PLAN.md in that worktree and use pr stage --phase plan; manual implementation
can be checkpointed with pr stage --phase implement. Neither stage publishes.
submit validates and publishes those changes to the same PR, keeping its title,
description, and draft/ready state. GitHub applies its review rules to new commits.

The linked issue remains the approved contract. Updated policy requires issue
approval, and human plan gates still require issue review-plan --approve.
Maintainers revise PRs with upstream write access. Contributors revise their own
fork PRs with --fork: changes publish to their fork branch, and the upstream fork
coordinator verifies fork ownership, the published head, plan approval, and
validation before recording the revision. Final code review and merge remain in
GitHub.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	for _, operation := range []string{"status", "work", "rebase", "validate", "submit"} {
		cmd.AddCommand(newPRActionCmd(rootOpts, operation))
	}
	cmd.AddCommand(newPRStageCmd(rootOpts))
	return cmd
}

func newPRActionCmd(rootOpts *RootOptions, operation string) *cobra.Command {
	var opts pr.Options
	var asJSON bool
	var progress bool
	var gate string
	summaries := map[string]string{
		"status":   "Inspect the linked issue, PR checks, and local revision",
		"work":     "Develop the existing PR within its approved issue contract",
		"rebase":   "Rebase the local PR revision onto its approved target base",
		"validate": "Validate the local PR revision against approved policy",
		"submit":   "Validate and publish updates to the same PR with head fencing",
	}
	cmd := &cobra.Command{
		Use:   operation + " <pr-number>",
		Short: summaries[operation],
		Args:  cobra.ExactArgs(1),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			number, err := parseNumberArg(args[0], "PR")
			if err != nil {
				return err
			}
			opts.Number = number
			if opts.Continue && opts.Abort {
				return fmt.Errorf("--continue and --abort are mutually exclusive")
			}
			if operation == "work" {
				if err := contract.ValidatePhase(opts.Work.Phase); err != nil {
					return fmt.Errorf("invalid --phase: %w", err)
				}
				opts.Work.Gate = contract.GateMode(gate)
				if err := contract.ValidateGate(opts.Work.Gate); err != nil {
					return fmt.Errorf("invalid --gate: %w", err)
				}
				if asJSON && !opts.Work.Headless {
					return fmt.Errorf("--json requires headless execution")
				}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			tr, manager, _, err := rootOpts.resolveLeaseManager(cmd)
			if err != nil {
				return err
			}
			service := rootOpts.prService(tr, manager)
			if operation == "work" && opts.Work.Headless && progress && !opts.Work.InspectOnly {
				opts.Work.OnProgress = workProgressPrinter(cmd.ErrOrStderr())
			}
			var result *pr.Result
			switch operation {
			case "status":
				result, err = service.Status(cmd.Context(), opts.Number)
			case "work":
				result, err = service.Work(cmd.Context(), opts)
			case "rebase":
				result, err = service.Rebase(cmd.Context(), opts)
			case "validate":
				result, err = service.Validate(cmd.Context(), opts.Number)
			case "submit":
				result, err = service.Submit(cmd.Context(), opts.Number)
			}
			if asJSON || opts.Work.InspectOnly {
				status := "completed"
				if result != nil && result.Status != "" {
					status = result.Status
				}
				outcome := map[string]any{"kind": "pr-" + operation, "status": status, "result": result}
				if err != nil {
					outcome["status"], outcome["error"] = "failed", err.Error()
				}
				if outputErr := writeJSON(cmd, outcome); outputErr != nil {
					return outputErr
				}
			} else if result != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "PR #%d %s: issue=#%d head=%s base=%s\n", opts.Number, result.Status, result.IssueNumber, result.HeadSHA, result.BaseSHA)
				fmt.Fprint(cmd.OutOrStdout(), pr.RebaseSummary(result.State, opts.Number))
				if result.Worktree != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "Worktree: %s\nBranch: %s\n", result.Worktree, result.Branch)
				}
				if result.Status == pr.StatusUnverifiedHead && result.State != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "The PR head has commits pushed outside SwarmOS since verified head %s; its recorded plan, validation, and provenance describe that earlier head. Run swarm pr work %d to re-plan and re-validate.\n", result.State.HeadSHA, opts.Number)
				}
				if result.PR != nil {
					fmt.Fprintln(cmd.OutOrStdout(), result.PR.HTMLURL)
				}
				if operation == "status" {
					fmt.Fprintf(cmd.OutOrStdout(), "Local changes: %t\nRebase in progress: %t\n", result.Dirty, result.Rebasing)
					if result.PR != nil {
						fmt.Fprintf(cmd.OutOrStdout(), "Published head: %s\nTarget: %s at %s\nDraft: %t\n", result.PR.HeadSHA, result.PR.Base, result.PR.BaseSHA, result.PR.Draft)
					}
				}
				if len(result.Checks) != 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "Checks on the published head:")
				}
				for _, check := range result.Checks {
					reporter := ""
					if check.App != "" {
						reporter = " (app: " + check.App + ")"
					}
					fmt.Fprintf(cmd.OutOrStdout(), "  %s: %s %s%s\n", check.Name, check.Status, check.Conclusion, reporter)
				}
				if len(result.Reviews) != 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "Reviews:")
				}
				for _, review := range result.Reviews {
					freshness := "stale: earlier commit"
					if review.Current {
						freshness = "current head"
					}
					fmt.Fprintf(cmd.OutOrStdout(), "  %s: %s (%s %s)\n", review.Author, review.State, freshness, shortSHA(review.CommitSHA))
				}
				if result.Validation != nil {
					for _, check := range result.Validation.CheckOutcomes {
						status := "PASS"
						if !check.Passed {
							status = "FAIL"
						}
						fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", status, check.Name)
					}
				}
				if result.Work != nil && result.Work.PausedForHuman {
					if result.Work.PhaseCompleted == "awaiting-plan" {
						fmt.Fprintf(cmd.OutOrStdout(), "Interactive plan required: write .swarm/active/PLAN.md and .swarm/active/TEST_PLAN.md in %s, then run: swarm pr stage %d --phase plan\n", result.Worktree, opts.Number)
					} else {
						fmt.Fprintf(cmd.OutOrStdout(), "Plan approval required: swarm issue review-plan %d --approve\n", result.IssueNumber)
					}
				}
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit a structured PR outcome")
	if operation == "work" {
		cmd.Flags().StringVar(&opts.Work.AgentName, "agent", "", "Configured agent for all roles")
		cmd.Flags().StringVar(&opts.Work.PlannerAgent, "planner-agent", "", "Configured planner override")
		cmd.Flags().StringVar(&opts.Work.ReviewerAgent, "reviewer-agent", "", "Configured reviewer override")
		cmd.Flags().StringVar(&opts.Work.DeveloperAgent, "developer-agent", "", "Configured developer override")
		cmd.Flags().StringVar(&opts.Work.TesterAgent, "tester-agent", "", "Configured tester override")
		cmd.Flags().StringVar(&opts.Work.Phase, "phase", "all", "Phase: all, plan, review, or implement")
		cmd.Flags().StringVar(&gate, "gate", "", "Require gate none, agent, or human; cannot weaken policy")
		cmd.Flags().StringVar(&opts.Instructions, "instructions", "", "Development instructions within the approved WorkUnit scope")
		cmd.Flags().BoolVar(&opts.Work.Headless, "headless", true, "Run agents non-interactively (false requires a terminal)")
		cmd.Flags().BoolVar(&opts.Work.InspectOnly, "dry-run", false, "Print the linked task and execution policy as JSON without changing state")
		cmd.Flags().BoolVar(&progress, "progress", true, "Report headless progress on stderr")
	}
	if operation == "rebase" {
		cmd.Flags().BoolVar(&opts.Continue, "continue", false, "Continue after resolving and staging conflicts in the PR worktree")
		cmd.Flags().BoolVar(&opts.Abort, "abort", false, "Abort the managed rebase and restore its starting local commit")
	}
	return cmd
}

func requireIssueWorkspace(state *lease.IssueState) error {
	if state != nil && state.PRRevision != nil {
		return fmt.Errorf("issue has a pending PR revision; use swarm pr work %d, swarm pr stage %d, swarm pr validate %d, or swarm pr submit %d", state.PRRevision.Number, state.PRRevision.Number, state.PRRevision.Number, state.PRRevision.Number)
	}
	return nil
}

// prService builds the PR lifecycle service for this invocation's repository.
func (o *RootOptions) prService(tr tracker.Tracker, manager *lease.Manager) *pr.Service {
	return &pr.Service{Root: o.resolveRepoRoot(), Tracker: tr, Manager: manager, Fork: o.Fork, ForkRemote: o.effectiveForkRemote(), Now: o.now}
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
