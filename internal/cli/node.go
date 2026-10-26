package cli

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/node"
	"github.com/supernovae/swarmos/internal/registry"
	"github.com/supernovae/swarmos/internal/workflow"
	"github.com/supernovae/swarmos/internal/workspace"
)

func newNodeCmd(rootOpts *RootOptions) *cobra.Command {
	nodeCmd := &cobra.Command{
		Use:   "node",
		Short: "Configure and run volunteer worker nodes and parallel agent pools",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}

	var (
		nodeID       string
		capabilities []string
		models       []string
		ramOverride  string
		profileJSON  bool
	)

	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Detect host capabilities/models and register volunteer node profile (~/.config/swarm/node.yaml)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			defer func() { reportJSONFailure(cmd, profileJSON, "node-init", runErr) }()
			prof := node.DetectHostProfile(nodeID)
			if len(capabilities) > 0 {
				prof.Capabilities = capabilities
			}
			if len(models) > 0 {
				prof.Models = models
			}
			if ramOverride != "" {
				prof.Resources.RAM = ramOverride
			}

			cfgPath := rootOpts.nodeConfigPath()
			// --upstream-remote saves the node default (origin clears it);
			// without it, re-initializing keeps the saved default.
			if remote := strings.TrimSpace(rootOpts.UpstreamRemote); remote != "" {
				if remote != workspace.DefaultUpstreamRemote {
					prof.UpstreamRemote = remote
				}
			} else if saved, err := node.LoadUpstreamRemote(cfgPath); err == nil {
				prof.UpstreamRemote = saved
			}
			if err := node.SaveProfile(cfgPath, prof); err != nil {
				return err
			}

			if profileJSON {
				return writeJSON(cmd, map[string]any{"kind": "node-init", "status": "initialized", "path": cfgPath, "profile": prof})
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Initialized SwarmOS Volunteer Node profile at %s\n", cfgPath)
			printNodeProfile(cmd, prof)
			return nil
		},
	}
	initCmd.Flags().StringVar(&nodeID, "id", "", "Custom node identifier (defaults to <hostname>-<os>-<arch>)")
	initCmd.Flags().StringSliceVar(&capabilities, "capabilities", []string{"coding", "testing", "review", "docs"}, "Node capabilities")
	initCmd.Flags().StringSliceVar(&models, "models", nil, "Override available agent models (defaults to auto-detected claude/codex/opencode/omp)")
	initCmd.Flags().StringVar(&ramOverride, "ram", "", "Override detected RAM (e.g., 64GB)")
	initCmd.Flags().BoolVar(&profileJSON, "json", false, "Emit the saved node profile as JSON")

	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Display current volunteer node capabilities, models, and hardware resources",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			defer func() { reportJSONFailure(cmd, profileJSON, "node-status", runErr) }()
			prof, err := node.LoadOrDetectProfile(rootOpts.nodeConfigPath())
			if err != nil {
				return err
			}
			if profileJSON {
				return writeJSON(cmd, map[string]any{"kind": "node-status", "profile": prof})
			}
			printNodeProfile(cmd, prof)
			return nil
		},
	}

	statusCmd.Flags().BoolVar(&profileJSON, "json", false, "Emit the node profile as JSON")

	var (
		allRepos       bool
		concurrency    int
		maxJobs        int
		watch          bool
		pollInterval   time.Duration
		agentName      string
		plannerAgent   string
		reviewerAgent  string
		developerAgent string
		testerAgent    string
		gate           string
		autoSubmit     bool
		asJSON         bool
	)

	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Run autonomous volunteer worker pool claiming and executing ready WorkUnits",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if concurrency <= 0 || maxJobs < 0 || pollInterval <= 0 {
				return fmt.Errorf("--concurrency and --poll-interval must be positive; --max-jobs must be nonnegative")
			}
			if err := contract.ValidateGate(contract.GateMode(gate)); err != nil {
				return err
			}
			profile, err := node.LoadOrDetectProfile(rootOpts.nodeConfigPath())
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			if asJSON {
				out = cmd.ErrOrStderr()
			}
			var admitted atomic.Int64
			var completed atomic.Int64
			var failures atomic.Int64
			var outputMu sync.Mutex
			reserve := func() bool {
				for {
					current := admitted.Load()
					if maxJobs > 0 && current >= int64(maxJobs) {
						return false
					}
					if admitted.CompareAndSwap(current, current+1) {
						return true
					}
				}
			}
			options := workflow.WorkOptions{Version: Version,
				OnBaseChange: baseChangePrinter(progressWriter(cmd)),
				AgentName:    agentName, PlannerAgent: plannerAgent, ReviewerAgent: reviewerAgent,
				DeveloperAgent: developerAgent, TesterAgent: testerAgent,
				Handoff: true,
				Phase:   "all", Gate: contract.GateMode(gate), Headless: true,
				ForkMode: rootOpts.Fork, ForkRemote: rootOpts.effectiveForkRemote(),
			}
			var pauses []*workflow.ClaimBudgetPause
			execute := func(pulled *pulledWorkUnit) (*workflow.WorkResult, error) {
				tr, manager, cfg, err := rootOpts.resolveTargetLeaseManager(cmd, registry.RepoEntry{
					Repo: pulled.Repo, Path: pulled.RepoRoot, Backend: pulled.Backend,
				})
				if err != nil {
					return nil, err
				}
				manager.BindClaim(pulled.Issue.Number, pulled.State.Lease.ClaimID)
				engine := workflow.NewEngineWithManagerOptions(pulled.RepoRoot, tr, manager, cfg, rootOpts.Fork, rootOpts.effectiveForkRemote(), rootOpts.engineOptions())
				lock, err := lease.LockClaim(pulled.RepoRoot, pulled.Issue.Number)
				if err != nil {
					return nil, err
				}
				selected := options
				selected.RepoRoot, selected.IssueNumber = pulled.RepoRoot, pulled.Issue.Number
				selected.Phase, selected.ClaimID = pulled.TargetPhase, pulled.State.Lease.ClaimID
				selected.OnClaim = func(state *lease.IssueState) error {
					return lease.SaveClaimToken(pulled.RepoRoot, pulled.Issue.Number, state.Lease.ClaimID, state.BaseSHA)
				}
				result, err := engine.ExecuteWork(ctx, selected)
				lease.UnlockClaim(lock)
				if err != nil {
					return result, err
				}
				if autoSubmit && result.ValidationPassed && result.ClaimBudgetPause == nil {
					submitOpts := *rootOpts
					submitOpts.Repo = pulled.Repo
					submitOpts.RepoRoot = pulled.RepoRoot
					submitOpts.Backend = pulled.Backend
					// Buffer submission output so validation does not hold the
					// shared output lock and stall other workers' reporting.
					var submitOut, submitErr bytes.Buffer
					submit := newSubmitCmd(&submitOpts)
					submit.SetOut(&submitOut)
					submit.SetErr(&submitErr)
					submit.SetArgs([]string{fmt.Sprint(pulled.Issue.Number)})
					err = submit.ExecuteContext(ctx)
					outputMu.Lock()
					_, _ = submitOut.WriteTo(out)
					_, _ = submitErr.WriteTo(cmd.ErrOrStderr())
					outputMu.Unlock()
					if err != nil {
						return result, fmt.Errorf("automatic submission: %w", err)
					}
				}
				return result, nil
			}
			runBatch := func() {
				var workers sync.WaitGroup
				for worker := 0; worker < concurrency; worker++ {
					workers.Add(1)
					go func(worker int) {
						defer workers.Done()
						for ctx.Err() == nil && reserve() {
							pulled, err := pullNextReadyIssue(ctx, cmd, rootOpts, allRepos, options, 0)
							if err != nil {
								admitted.Add(-1)
								if !errors.Is(err, errNoReadyWork) {
									failures.Add(1)
									outputMu.Lock()
									fmt.Fprintf(out, "[worker-%d] selection failed: %v\n", worker+1, err)
									outputMu.Unlock()
								}
								return
							}
							result, err := execute(pulled)
							if errors.Is(err, lease.ErrPlanReplanLimit) {
								admitted.Add(-1)
								continue
							}
							completed.Add(1)
							outputMu.Lock()
							if err != nil {
								failures.Add(1)
								fmt.Fprintf(out, "[worker-%d] %s #%d failed: %v\n", worker+1, pulled.Repo, pulled.Issue.Number, err)
							} else {
								fmt.Fprintf(out, "[worker-%d] %s #%d %s: phase=%s validation=%t\n", worker+1, pulled.Repo, pulled.Issue.Number, result.Outcome(), result.PhaseCompleted, result.ValidationPassed)
								selectedRoot := *rootOpts
								selectedRoot.Repo, selectedRoot.RepoRoot, selectedRoot.Backend = pulled.Repo, pulled.RepoRoot, pulled.Backend
								if guidance := claimBudgetGuidance(&selectedRoot, result, "issue", pulled.Issue.Number); guidance != "" {
									fmt.Fprintln(out, guidance)
									pauses = append(pauses, result.ClaimBudgetPause)
								}
							}
							outputMu.Unlock()
							if err != nil {
								return
							}
						}
					}(worker)
				}
				workers.Wait()
			}
			for {
				runBatch()
				if failures.Load() > 0 {
					if asJSON {
						_ = writeJSON(cmd, map[string]any{"kind": "node", "status": "failed", "attempts": completed.Load(), "failures": failures.Load(), "claim_budget_pauses": pauses})
					}
					return fmt.Errorf("node %q encountered %d failure(s)", profile.NodeID, failures.Load())
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if !watch || maxJobs > 0 && admitted.Load() >= int64(maxJobs) {
					break
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(pollInterval):
				}
			}
			if asJSON {
				return writeJSON(cmd, map[string]any{"kind": "node", "status": "drained", "attempts": completed.Load(), "failures": failures.Load(), "claim_budget_pauses": pauses})
			}
			fmt.Fprintf(out, "Node %q finished: %d task attempt(s)\n", profile.NodeID, completed.Load())
			return nil
		},
	}

	runCmd.Flags().BoolVar(&allRepos, "all", false, "Poll all registered repositories")
	runCmd.Flags().IntVar(&concurrency, "concurrency", 1, "Number of concurrent worker goroutines")
	runCmd.Flags().IntVar(&maxJobs, "max-jobs", 0, "Maximum claimed task/phase attempts (0 = drain without a limit)")
	runCmd.Flags().BoolVar(&watch, "watch", false, "Continuously poll after draining the ready queue")
	runCmd.Flags().DurationVar(&pollInterval, "poll-interval", 15*time.Second, "Delay between scans with --watch")
	runCmd.Flags().StringVar(&agentName, "agent", "", "Default agent harness")
	runCmd.Flags().StringVar(&plannerAgent, "planner-agent", "", "Agent harness for planner role")
	runCmd.Flags().StringVar(&reviewerAgent, "reviewer-agent", "", "Agent harness for reviewer role")
	runCmd.Flags().StringVar(&developerAgent, "developer-agent", "", "Agent harness for developer role")
	runCmd.Flags().StringVar(&testerAgent, "tester-agent", "", "Agent harness for tester role")
	runCmd.Flags().StringVar(&gate, "gate", "", "Plan review gate override (none, agent, human)")
	runCmd.Flags().BoolVar(&autoSubmit, "auto-submit", false, "Automatically submit Draft PRs when validation passes")
	runCmd.Flags().BoolVar(&asJSON, "json", false, "Emit a final structured outcome; progress goes to stderr")

	nodeCmd.AddCommand(initCmd, statusCmd, runCmd)
	return nodeCmd
}

func printNodeProfile(cmd *cobra.Command, prof *contract.NodeProfile) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "  Node ID:      %s\n", prof.NodeID)
	fmt.Fprintf(out, "  Capabilities: %s\n", strings.Join(prof.Capabilities, ", "))
	fmt.Fprintf(out, "  Models:       %s\n", strings.Join(prof.Models, ", "))
	fmt.Fprintf(out, "  Resources:    %s/%s, %d CPUs, %s RAM\n",
		prof.Resources.OS, prof.Resources.Arch, prof.Resources.CPUs, prof.Resources.RAM)
	fmt.Fprintf(out, "  Upstream:     %s\n", workspace.UpstreamRemoteOrDefault(prof.UpstreamRemote))
}
