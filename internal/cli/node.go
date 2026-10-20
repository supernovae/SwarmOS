package cli

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/node"
	"github.com/supernovae/swarmos/internal/registry"
	"github.com/supernovae/swarmos/internal/workflow"
)

func newNodeCmd(rootOpts *RootOptions) *cobra.Command {
	nodeCmd := &cobra.Command{
		Use:   "node",
		Short: "Configure and run volunteer worker nodes and parallel agent pools",
	}

	var (
		nodeID       string
		capabilities []string
		models       []string
		ramOverride  string
	)

	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Detect host capabilities/models and register volunteer node profile (~/.config/swarm/node.yaml)",
		RunE: func(cmd *cobra.Command, args []string) error {
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

			cfgPath := node.DefaultNodeConfigPath()
			if err := node.SaveProfile(cfgPath, prof); err != nil {
				return err
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

	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Display current volunteer node capabilities, models, and hardware resources",
		RunE: func(cmd *cobra.Command, args []string) error {
			prof, err := node.LoadOrDetectProfile("")
			if err != nil {
				return err
			}
			printNodeProfile(cmd, prof)
			return nil
		},
	}

	var (
		allRepos       bool
		concurrency    int
		maxJobs        int
		once           bool
		pollInterval   time.Duration
		agentName      string
		plannerAgent   string
		reviewerAgent  string
		developerAgent string
		testerAgent    string
		gate           string
		autoSubmit     bool
	)

	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Run autonomous volunteer worker pool claiming and executing ready WorkUnits",
		RunE: func(cmd *cobra.Command, args []string) error {
			if concurrency <= 0 {
				concurrency = 1
			}
			prof, err := node.LoadOrDetectProfile("")
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Starting SwarmOS Node %q [capabilities=%v, models=%v, concurrency=%d]\n",
				prof.NodeID, prof.Capabilities, prof.Models, concurrency)

			ctx := cmd.Context()
			var completedJobs atomic.Int32
			var outMu sync.Mutex

			runBatch := func() int {
				var wg sync.WaitGroup
				var batchClaimed atomic.Int32

				for w := range concurrency {
					if maxJobs > 0 && int(completedJobs.Load()) >= maxJobs {
						break
					}
					wg.Add(1)
					go func(workerIdx int) {
						defer wg.Done()
						for {
							if maxJobs > 0 && int(completedJobs.Load()) >= maxJobs {
								return
							}

							pulled, pErr := PullNextReadyIssue(ctx, cmd, rootOpts, allRepos, agentName, 0)
							if pErr != nil {
								return
							}
							batchClaimed.Add(1)

							tr, _, _, tErr := rootOpts.resolveTargetLeaseManager(cmd, registry.RepoEntry{
								Repo:    pulled.Repo,
								Path:    pulled.RepoRoot,
								Backend: pulled.Backend,
							})
							if tErr != nil {
								return
							}

							eng, eErr := workflow.NewEngine(pulled.RepoRoot, tr)
							if eErr != nil {
								return
							}

							targetPhase := pulled.TargetPhase
							if targetPhase == "" {
								targetPhase = "all"
							}

							res, wErr := eng.ExecuteWork(ctx, workflow.WorkOptions{
								RepoRoot:       pulled.RepoRoot,
								IssueNumber:    pulled.Issue.Number,
								AgentName:      agentName,
								PlannerAgent:   plannerAgent,
								ReviewerAgent:  reviewerAgent,
								DeveloperAgent: developerAgent,
								TesterAgent:    testerAgent,
								Phase:          targetPhase,
								Gate:           contract.GateMode(gate),
								Headless:       true,
							})

							outMu.Lock()
							if wErr != nil {
								fmt.Fprintf(out, "[worker-%d] [%s #%d] FAILED: %v\n", workerIdx+1, pulled.Repo, pulled.Issue.Number, wErr)
							} else {
								fmt.Fprintf(out, "[worker-%d] [%s #%d] COMPLETED phase=%s validation=%v\n",
									workerIdx+1, pulled.Repo, pulled.Issue.Number, res.PhaseCompleted, res.ValidationPassed)
							}
							outMu.Unlock()

							if wErr == nil && autoSubmit && res.ValidationPassed {
								subCmd := newSubmitCmd(&RootOptions{
									Backend:  pulled.Backend,
									Repo:     pulled.Repo,
									RepoRoot: pulled.RepoRoot,
								})
								outMu.Lock()
								subCmd.SetOut(out)
								subCmd.SetErr(cmd.ErrOrStderr())
								subCmd.SetArgs([]string{fmt.Sprintf("%d", pulled.Issue.Number)})
								_ = subCmd.ExecuteContext(ctx)
								outMu.Unlock()
							}

							completedJobs.Add(1)
							if once {
								continue
							}
						}
					}(w)
				}
				wg.Wait()
				return int(batchClaimed.Load())
			}

			for {
				claimedCount := runBatch()
				if once || (maxJobs > 0 && int(completedJobs.Load()) >= maxJobs) {
					break
				}
				if claimedCount == 0 {
					select {
					case <-ctx.Done():
						return nil
					case <-time.After(pollInterval):
					}
				}
			}

			fmt.Fprintf(out, "Node %q finished (%d work unit(s) processed)\n", prof.NodeID, completedJobs.Load())
			return nil
		},
	}

	runCmd.Flags().BoolVar(&allRepos, "all", false, "Poll all registered repositories")
	runCmd.Flags().IntVar(&concurrency, "concurrency", 1, "Number of concurrent worker goroutines")
	runCmd.Flags().IntVar(&maxJobs, "max-jobs", 0, "Maximum number of work units to process before exiting (0 = unlimited)")
	runCmd.Flags().BoolVar(&once, "once", true, "Drain currently available ready queue once and exit")
	runCmd.Flags().DurationVar(&pollInterval, "poll-interval", 15*time.Second, "Polling interval when --once=false")
	runCmd.Flags().StringVar(&agentName, "agent", "", "Default agent harness")
	runCmd.Flags().StringVar(&plannerAgent, "planner-agent", "", "Agent harness for planner role")
	runCmd.Flags().StringVar(&reviewerAgent, "reviewer-agent", "", "Agent harness for reviewer role")
	runCmd.Flags().StringVar(&developerAgent, "developer-agent", "", "Agent harness for developer role")
	runCmd.Flags().StringVar(&testerAgent, "tester-agent", "", "Agent harness for tester role")
	runCmd.Flags().StringVar(&gate, "gate", "", "Plan review gate override (none, agent, human)")
	runCmd.Flags().BoolVar(&autoSubmit, "auto-submit", false, "Automatically submit Draft PRs when validation passes")

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
}
