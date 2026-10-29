package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/registry"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workflow"
)

func newWorkCmd(rootOpts *RootOptions) *cobra.Command {
	var agentName, plannerAgent, reviewerAgent, developerAgent, testerAgent, phase, gate, instructions string
	var roleInstructions workflow.WorkOptions
	var headless, handoff, nextReady, allRepos, jsonOutput, dryRun bool
	var progressOutput bool
	cmd := &cobra.Command{
		Use:   "work [<issue-number>]",
		Short: "Run an approved task through planning, review, and implementation",
		Args:  cobra.MaximumNArgs(1),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && nextReady {
				return fmt.Errorf("<issue-number> and --next are mutually exclusive")
			}
			if len(args) == 0 && !nextReady {
				return fmt.Errorf("issue number is required (or pass --next)")
			}
			if allRepos && !nextReady {
				return fmt.Errorf("--all requires --next")
			}
			if err := contract.ValidatePhase(phase); err != nil {
				return fmt.Errorf("invalid --phase: %w", err)
			}
			if err := contract.ValidateGate(contract.GateMode(gate)); err != nil {
				return fmt.Errorf("invalid --gate: %w", err)
			}
			if dryRun && nextReady {
				return fmt.Errorf("--dry-run requires an explicit issue number")
			}
			if handoff && phase != "plan" && phase != "review" {
				return fmt.Errorf("--handoff requires --phase plan or --phase review")
			}
			if jsonOutput && !headless {
				return fmt.Errorf("--json requires headless execution")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			var failedReport *validation.ValidationReport
			defer func() { reportJSONFailureWithReport(cmd, jsonOutput, "work", runErr, failedReport) }()
			guidanceOpts := *rootOpts
			stops := stopContext{opts: &guidanceOpts, resource: "issue"}
			defer func() {
				if !dryRun {
					runErr = stops.stop(runErr)
				}
			}()
			opts := workflow.WorkOptions{Version: Version,
				OnBaseChange: baseChangePrinter(progressWriter(cmd)),
				AgentName:    agentName, PlannerAgent: plannerAgent, ReviewerAgent: reviewerAgent,
				DeveloperAgent: developerAgent, TesterAgent: testerAgent, Phase: phase,
				DeveloperInstructions: roleInstructions.DeveloperInstructions, TesterInstructions: roleInstructions.TesterInstructions, PlannerInstructions: roleInstructions.PlannerInstructions, ReviewerInstructions: roleInstructions.ReviewerInstructions,
				Instructions: instructions, Gate: contract.GateMode(gate), Headless: headless, Handoff: handoff,
				ForkMode: rootOpts.Fork, ForkRemote: rootOpts.effectiveForkRemote(),
			}
			if progressOutput && !dryRun {
				printer := workProgressPrinter(progressWriter(cmd))
				opts.SetupProgress = rootOpts.newProgressReporter(cmd.Context(), printer)
				defer opts.SetupProgress.Close()
				if headless {
					opts.OnProgress = printer
				}
			}
			var tr tracker.Tracker
			var mgr *lease.Manager
			var cfg *contract.SwarmConfig
			var err error
			var selection *overlapSelection
			if nextReady {
				pulled, err := pullNextReadyIssue(cmd.Context(), cmd, rootOpts, allRepos, opts, 0)
				if err != nil {
					return err
				}
				selection = pulled.Overlap
				opts.IssueNumber, opts.RepoRoot, opts.Phase, opts.ClaimID = pulled.Issue.Number, pulled.RepoRoot, pulled.TargetPhase, pulled.State.Lease.ClaimID
				guidanceOpts.Repo, guidanceOpts.RepoRoot, guidanceOpts.Backend = pulled.Repo, pulled.RepoRoot, pulled.Backend
				tr, mgr, cfg, err = rootOpts.resolveTargetLeaseManager(cmd, registry.RepoEntry{Repo: pulled.Repo, Path: pulled.RepoRoot, Backend: pulled.Backend})
				if err != nil {
					return err
				}
			} else {
				opts.IssueNumber, err = parseNumberArg(args[0], "issue")
				if err != nil {
					return err
				}
				opts.RepoRoot = rootOpts.resolveRepoRoot()
				tr, mgr, cfg, err = rootOpts.resolveLeaseManager(cmd)
				if err != nil {
					return err
				}
			}
			stops.number = opts.IssueNumber
			engine := workflow.NewEngineWithManagerOptions(opts.RepoRoot, tr, mgr, cfg, rootOpts.Fork, rootOpts.effectiveForkRemote(), rootOpts.engineOptions())
			if !dryRun {
				lock, err := lease.LockClaim(opts.RepoRoot, opts.IssueNumber)
				if err != nil {
					return err
				}
				defer lease.UnlockClaim(lock)
				if opts.ClaimID == "" {
					claimID, tokenErr := lease.LoadClaimToken(opts.RepoRoot, opts.IssueNumber)
					if tokenErr == nil {
						state, err := engine.LeaseManager.GetCurrentState(cmd.Context(), opts.IssueNumber)
						if err != nil {
							return err
						}
						if state.Lease.Status == "active" && state.Lease.ExpiresAt.After(rootOpts.now()) && state.Lease.ClaimID == claimID {
							opts.ClaimID = claimID
						}
					} else if !os.IsNotExist(tokenErr) {
						return fmt.Errorf("load persisted claim: %w", tokenErr)
					}
				}
				opts.OnClaim = func(state *lease.IssueState) error {
					return lease.SaveClaimToken(opts.RepoRoot, opts.IssueNumber, state.Lease.ClaimID, state.BaseSHA)
				}
			}
			if dryRun {
				opts.InspectOnly = true
				preflight, err := engine.Preflight(cmd.Context(), opts)
				if err != nil {
					return err
				}
				return writeJSON(cmd, map[string]any{"kind": "work-dry-run", "issue_number": opts.IssueNumber, "effective_plan": preflight})
			}
			result, err := engine.ExecuteWork(cmd.Context(), opts)
			if result != nil {
				writeValidationSkipNotice(cmd, result.Validation)
			}
			opts.SetupProgress.Close()
			if errors.Is(err, workflow.ErrPlanReviewRejected) && result != nil {
				recovery := planReviewRecovery(&guidanceOpts, result.State, "issue", opts.IssueNumber)
				if jsonOutput {
					return &planRecoveryError{err: err, recovery: recovery}
				}
				fmt.Fprintln(cmd.OutOrStdout(), recovery)
			}
			if err != nil {
				if failedReport = failedWorkValidation(result); !jsonOutput {
					writeWorkValidationFailure(cmd, result)
				}
				return err
			}
			guidance := claimBudgetGuidance(&guidanceOpts, result, "issue", result.IssueNumber)
			split := splitGuidance(&guidanceOpts, result, result.IssueNumber)
			status := result.Outcome()
			if jsonOutput {
				outcome := map[string]any{"kind": "work", "status": status, "result": result}
				if selection != nil {
					outcome["selection"] = selection
				}
				return writeJSON(cmd, outcome)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Issue #%d %s: phase=%s branch=%s validation=%t\n", result.IssueNumber, status, result.PhaseCompleted, result.BranchName, result.ValidationPassed)
			if guidance != "" {
				fmt.Fprintln(cmd.OutOrStdout(), guidance)
			}
			if split != "" {
				fmt.Fprintln(cmd.OutOrStdout(), split)
			}
			if result.PausedForHuman {
				if result.PhaseCompleted == "awaiting-plan" {
					fmt.Fprintf(cmd.OutOrStdout(), "Interactive plan required: write .swarm/active/PLAN.md and .swarm/active/TEST_PLAN.md in %s, then run: swarm issue stage %d --phase plan\n", result.WorktreePath, result.IssueNumber)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "Plan approval required: swarm issue review-plan %d --approve\n", result.IssueNumber)
					if len(result.PlanReviewers) > 0 {
						fmt.Fprintf(cmd.OutOrStdout(), "Reviewers requested: %s\n", strings.Join(result.PlanReviewers, ", "))
					}
					if result.ReviewerNoticeError != "" {
						fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not notify plan reviewers: %s\n", result.ReviewerNoticeError)
					}
				}
			}
			if result.HandedOffToPhase != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Next phase: %s\n", result.HandedOffToPhase)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&agentName, "agent", "", "Configured agent for all roles")
	cmd.Flags().StringVar(&plannerAgent, "planner-agent", "", "Configured planner override")
	cmd.Flags().StringVar(&reviewerAgent, "reviewer-agent", "", "Configured reviewer override")
	cmd.Flags().StringVar(&developerAgent, "developer-agent", "", "Configured developer override")
	cmd.Flags().StringVar(&testerAgent, "tester-agent", "", "Configured tester override")
	cmd.Flags().StringVar(&roleInstructions.DeveloperInstructions, "developer-instructions", "", "Additional operator guidance for the developer role")
	cmd.Flags().StringVar(&roleInstructions.TesterInstructions, "tester-instructions", "", "Additional operator guidance for the tester role")
	cmd.Flags().StringVar(&roleInstructions.PlannerInstructions, "planner-instructions", "", "Additional operator guidance for the planner role")
	cmd.Flags().StringVar(&roleInstructions.ReviewerInstructions, "reviewer-instructions", "", "Additional operator guidance for the reviewer role")
	cmd.Flags().StringVar(&instructions, "instructions", "", "Additional operator instructions for the run")
	cmd.Flags().StringVar(&phase, "phase", "all", "Phase: all, plan, review, or implement")
	cmd.Flags().StringVar(&gate, "gate", "", "Require gate none, agent, or human; cannot weaken policy")
	cmd.Flags().BoolVar(&headless, "headless", true, "Run agents non-interactively (false requires a terminal)")
	cmd.Flags().BoolVar(&handoff, "handoff", false, "Checkpoint and release to the next phase")
	cmd.Flags().BoolVar(&nextReady, "next", false, "Claim the next approved ready task")
	cmd.Flags().BoolVar(&allRepos, "all", false, "With --next, scan registered repositories")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit a structured outcome")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the effective policy as JSON without claiming or running agents")
	cmd.Flags().BoolVar(&progressOutput, "progress", true, "Report setup, headless stages, and a 15-second heartbeat to stderr (false disables)")
	return cmd
}
