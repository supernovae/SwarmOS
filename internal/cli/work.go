package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/registry"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workflow"
)

func newWorkCmd(rootOpts *RootOptions) *cobra.Command {
	var (
		agentName      string
		plannerAgent   string
		reviewerAgent  string
		developerAgent string
		testerAgent    string
		phase          string
		gate           string
		headless       bool
		handoff        bool
		nextReady      bool
		allRepos       bool
	)

	cmd := &cobra.Command{
		Use:   "work [<issue-number>]",
		Short: "Execute SwarmOS Plan-Review-Implement-Validate workflow for an issue (or --next available)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var (
				issueNum int
				repoRoot string
				tr       tracker.Tracker
				err      error
			)

			targetPhase := phase
			if nextReady {
				pulled, pErr := PullNextReadyIssue(cmd.Context(), cmd, rootOpts, allRepos, agentName, 0)
				if pErr != nil {
					return pErr
				}
				issueNum = pulled.Issue.Number
				repoRoot = pulled.RepoRoot
				if targetPhase == "all" && pulled.TargetPhase != "" && pulled.TargetPhase != "all" {
					targetPhase = pulled.TargetPhase
				}
				tr, _, _, err = rootOpts.resolveTargetLeaseManager(cmd, registry.RepoEntry{
					Repo:    pulled.Repo,
					Path:    pulled.RepoRoot,
					Backend: pulled.Backend,
				})
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Auto-selected [%s] issue #%d: %s (phase=%s)\n",
					pulled.Repo, issueNum, pulled.Issue.Title, targetPhase)
			} else {
				if len(args) == 0 {
					return fmt.Errorf("issue number is required (or pass --next to pull the next ready issue)")
				}
				issueNum, err = strconv.Atoi(strings.TrimPrefix(args[0], "#"))
				if err != nil || issueNum <= 0 {
					return fmt.Errorf("invalid issue number %q", args[0])
				}
				repoRoot = rootOpts.resolveRepoRoot()
				tr, err = rootOpts.resolveTracker(cmd)
				if err != nil {
					return err
				}
			}

			eng, err := workflow.NewEngine(repoRoot, tr)
			if err != nil {
				return err
			}

			res, err := eng.ExecuteWork(cmd.Context(), workflow.WorkOptions{
				RepoRoot:       repoRoot,
				IssueNumber:    issueNum,
				AgentName:      agentName,
				PlannerAgent:   plannerAgent,
				ReviewerAgent:  reviewerAgent,
				DeveloperAgent: developerAgent,
				TesterAgent:    testerAgent,
				Phase:          targetPhase,
				Gate:           contract.GateMode(gate),
				Headless:       headless,
				Handoff:        handoff,
			})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Completed workflow phase %q for issue #%d in %s (branch %s)\n",
				res.PhaseCompleted, res.IssueNumber, res.WorktreePath, res.BranchName)
			if res.ReviewVerdict != "" {
				fmt.Fprintf(out, "  Plan Review Verdict: %s\n", res.ReviewVerdict)
			}
			if res.HandedOffToPhase != "" {
				fmt.Fprintf(out, "  Handoff: Ready for next contributor/node on phase %q\n", res.HandedOffToPhase)
			}
			if res.PausedForHuman {
				fmt.Fprintf(out, "  Gate: Paused for human review (label swarm:plan-review applied). Run `swarm review %d --approve` to continue.\n", res.IssueNumber)
			}
			if res.ValidationPassed {
				fmt.Fprintln(out, "  Validation: PASS")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&agentName, "agent", "", "Default agent harness for all roles (claude, codex, opencode, omp, mock)")
	cmd.Flags().StringVar(&plannerAgent, "planner-agent", "", "Agent harness for the planner role")
	cmd.Flags().StringVar(&reviewerAgent, "reviewer-agent", "", "Agent harness for the reviewer role")
	cmd.Flags().StringVar(&developerAgent, "developer-agent", "", "Agent harness for the developer role")
	cmd.Flags().StringVar(&testerAgent, "tester-agent", "", "Agent harness for the tester role")
	cmd.Flags().StringVar(&phase, "phase", "all", "Workflow phase to run: all, plan, or implement")
	cmd.Flags().StringVar(&gate, "gate", "", "Plan review gate override: none, agent, or human")
	cmd.Flags().BoolVar(&headless, "headless", false, "Run agent non-interactively")
	cmd.Flags().BoolVar(&handoff, "handoff", false, "Transition lease to collaborative handoff state after completing phase")
	cmd.Flags().BoolVar(&nextReady, "next", false, "Automatically pull and work on the next unclaimed/handed-off swarm:ready issue")
	cmd.Flags().BoolVar(&allRepos, "all", false, "With --next, search across all registered repositories")
	return cmd
}
