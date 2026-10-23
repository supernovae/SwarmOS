package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/registry"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workflow"
)

func newWorkCmd(rootOpts *RootOptions) *cobra.Command {
	var agentName, plannerAgent, reviewerAgent, developerAgent, testerAgent, phase, gate string
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
			if jsonOutput {
				defer func() {
					if runErr != nil {
						_ = writeJSON(cmd, map[string]any{"kind": "work", "status": "failed", "error": runErr.Error()})
					}
				}()
			}
			opts := workflow.WorkOptions{
				AgentName: agentName, PlannerAgent: plannerAgent, ReviewerAgent: reviewerAgent,
				DeveloperAgent: developerAgent, TesterAgent: testerAgent, Phase: phase,
				Gate: contract.GateMode(gate), Headless: headless, Handoff: handoff,
				ForkMode: rootOpts.Fork, ForkRemote: rootOpts.effectiveForkRemote(),
			}
			var tr tracker.Tracker
			var mgr *lease.Manager
			var cfg *contract.SwarmConfig
			var err error
			if nextReady {
				pulled, err := PullNextReadyIssue(cmd.Context(), cmd, rootOpts, allRepos, opts, 0)
				if err != nil {
					return err
				}
				opts.IssueNumber, opts.RepoRoot, opts.Phase, opts.ClaimID = pulled.Issue.Number, pulled.RepoRoot, pulled.TargetPhase, pulled.State.Lease.ClaimID
				tr, mgr, cfg, err = rootOpts.resolveTargetLeaseManager(cmd, registry.RepoEntry{Repo: pulled.Repo, Path: pulled.RepoRoot, Backend: pulled.Backend})
				if err != nil {
					return err
				}
			} else {
				opts.IssueNumber, err = strconv.Atoi(strings.TrimPrefix(args[0], "#"))
				if err != nil || opts.IssueNumber <= 0 {
					return fmt.Errorf("invalid issue number %q", args[0])
				}
				opts.RepoRoot = rootOpts.resolveRepoRoot()
				tr, mgr, cfg, err = rootOpts.resolveLeaseManager(cmd)
				if err != nil {
					return err
				}
			}
			engine := workflow.NewEngineWithManager(opts.RepoRoot, tr, mgr, cfg, rootOpts.Fork, rootOpts.effectiveForkRemote())
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
					return lease.SaveClaimToken(opts.RepoRoot, opts.IssueNumber, state.Lease.ClaimID)
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
			if headless && progressOutput {
				opts.OnProgress = workProgressPrinter(cmd.ErrOrStderr())
			}
			result, err := engine.ExecuteWork(cmd.Context(), opts)
			if err != nil {
				return err
			}
			status := "completed"
			if result.PausedForHuman {
				status = "paused"
			} else if result.HandedOffToPhase != "" {
				status = "handed-off"
			}
			if jsonOutput {
				return writeJSON(cmd, map[string]any{"kind": "work", "status": status, "result": result})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Issue #%d %s: phase=%s branch=%s validation=%t\n", result.IssueNumber, status, result.PhaseCompleted, result.BranchName, result.ValidationPassed)
			if result.PausedForHuman {
				if result.PhaseCompleted == "awaiting-plan" {
					fmt.Fprintf(cmd.OutOrStdout(), "Interactive plan required: write .swarm/active/PLAN.md and .swarm/active/TEST_PLAN.md in %s, then run: swarm issue stage %d --phase plan\n", result.WorktreePath, result.IssueNumber)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "Human approval required: swarm issue review-plan %d --approve\n", result.IssueNumber)
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
	cmd.Flags().StringVar(&phase, "phase", "all", "Phase: all, plan, review, or implement")
	cmd.Flags().StringVar(&gate, "gate", "", "Require gate none, agent, or human; cannot weaken policy")
	cmd.Flags().BoolVar(&headless, "headless", true, "Run agents non-interactively (false requires a terminal)")
	cmd.Flags().BoolVar(&handoff, "handoff", false, "Checkpoint and release to the next phase")
	cmd.Flags().BoolVar(&nextReady, "next", false, "Claim the next approved ready task")
	cmd.Flags().BoolVar(&allRepos, "all", false, "With --next, scan registered repositories")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit a structured outcome")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Inspect effective policy without claiming or running agents")
	cmd.Flags().BoolVar(&progressOutput, "progress", true, "Report headless stages and a 15-second heartbeat to stderr (false disables)")
	return cmd
}

func writeJSON(cmd *cobra.Command, value any) error {
	return json.NewEncoder(cmd.OutOrStdout()).Encode(value)
}
