package cli

import (
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/workflow"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

func newReviewCmd(rootOpts *RootOptions) *cobra.Command {
	var agentName, notes string
	var approve, reject, headless, progress, asJSON bool
	cmd := &cobra.Command{
		Use:   "review-plan <issue-number>",
		Short: "Review the implementation plan with an isolated agent or authorized human",
		Long: `Review PLAN.md and TEST_PLAN.md before implementation, using an isolated
agent or an authorized human decision. Approval is bound to the current task
and plan digests. This command does not review a finished PR or check off its
acceptance criteria; final code and acceptance review happens in GitHub after
issue submit opens the linked draft PR.

From an upstream GitHub checkout, maintainers can review an active public
personal-fork plan while its contributor keeps the lease. Initial plans use
the approved base; checkpoint and PR revision plans require their exact published
candidate. Agent output remains advisory at a human gate; use --approve for the
independent human decision.`,
		Args: cobra.ExactArgs(1),
		PreRunE: func(cmd *cobra.Command, args []string) (runErr error) {
			defer func() { reportJSONFailure(cmd, asJSON && (approve || reject), "review", runErr) }()
			if approve && reject {
				return fmt.Errorf("--approve and --reject are mutually exclusive")
			}
			if (approve || reject) && agentName != "" {
				return fmt.Errorf("--agent cannot be combined with a human decision")
			}
			if notes != "" && !approve && !reject {
				return fmt.Errorf("--notes requires --approve or --reject")
			}
			if !approve && !reject && asJSON && !headless {
				return fmt.Errorf("--json requires headless agent review")
			}
			if rootOpts.Fork && (approve || reject) {
				return fmt.Errorf("human review (--approve and --reject) is maintainer-only and cannot be used with --fork")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			issue, err := parseNumberArg(args[0], "issue")
			if err != nil {
				return err
			}
			if !approve && !reject {
				if !rootOpts.Fork && rootOpts.Backend != "local" {
					tr, manager, cfg, err := rootOpts.resolveLeaseManager(cmd)
					if err != nil {
						return err
					}
					if strings.HasPrefix(tr.BackendName(), "github (") {
						st, err := manager.GetCurrentState(cmd.Context(), issue)
						if err != nil && !errors.Is(err, coordination.ErrNotFound) {
							return err
						}
						if st != nil && st.ForkRepo != "" {
							return runUpstreamReview(cmd, rootOpts, issue, agentName, headless, progress, asJSON, workflow.NewEngineWithManagerOptions(rootOpts.resolveRepoRoot(), tr, manager, cfg, false, "", rootOpts.engineOptions()))
						}
					}
				}
				work := newWorkCmd(rootOpts)
				work.SetOut(cmd.OutOrStdout())
				work.SetErr(cmd.ErrOrStderr())
				// Delegate to issue work so agent review gets identical validation and output.
				workArgs := []string{strconv.Itoa(issue), "--phase", "review", "--headless=" + strconv.FormatBool(headless), "--progress=" + strconv.FormatBool(progress)}
				if agentName != "" {
					workArgs = append(workArgs, "--reviewer-agent", agentName)
				}
				if asJSON {
					workArgs = append(workArgs, "--json")
				}
				work.SetArgs(workArgs)
				return work.ExecuteContext(cmd.Context())
			}
			tr, manager, _, err := rootOpts.resolveLeaseManager(cmd)
			if err != nil {
				return err
			}
			verdict := "APPROVED"
			if reject {
				verdict = "CHANGES_REQUESTED"
			}
			reviewed, err := manager.RecordHumanReview(cmd.Context(), issue, verdict, notes)
			if err != nil {
				return err
			}
			if approve {
				if err := tr.RemoveLabel(cmd.Context(), issue, "swarm:plan-review"); err != nil {
					return err
				}
				if err := tr.AddLabels(cmd.Context(), issue, []string{"swarm:plan-approved"}); err != nil {
					return err
				}
			} else {
				if err := tr.RemoveLabel(cmd.Context(), issue, "swarm:plan-approved"); err != nil {
					return err
				}
				if err := tr.AddLabels(cmd.Context(), issue, []string{"swarm:plan-review"}); err != nil {
					return err
				}
			}
			if asJSON {
				outcome := map[string]any{"kind": "review", "issue_number": issue, "reviewer_kind": "human", "verdict": verdict}
				if d := directPlanReviewDisplay(rootOpts, reviewed); d != nil {
					outcome["plan_review"] = d
				}
				return writeJSON(cmd, outcome)
			}
			if d := directPlanReviewDisplay(rootOpts, reviewed); d != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "Plan review round: %d; automatic plan retries: %d/%d; operator decision needed: %t\n", d.Round, d.RetriesUsed, d.RetriesAllowed, d.OperatorDecision)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Human review recorded for issue #%d: %s\n", issue, verdict)
			return nil
		},
	}
	cmd.Flags().StringVar(&agentName, "agent", "", "Configured agent for the reviewer role (overrides roles.reviewer)")
	cmd.Flags().BoolVar(&approve, "approve", false, "Record an authenticated human approval")
	cmd.Flags().BoolVar(&reject, "reject", false, "Record an authenticated human rejection")
	cmd.Flags().StringVar(&notes, "notes", "", "Human decision notes")
	cmd.Flags().BoolVar(&headless, "headless", true, "Run reviewer non-interactively")
	cmd.Flags().BoolVar(&progress, "progress", true, "Report headless agent review progress on stderr (false disables)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit a structured review outcome")
	return cmd
}

func runUpstreamReview(cmd *cobra.Command, opts *RootOptions, issue int, reviewer string, headless, progress, asJSON bool, engine *workflow.Engine) (runErr error) {
	jsonWritten := false
	defer func() { reportJSONFailure(cmd, asJSON && !jsonWritten, "review", runErr) }()
	observation, err := engine.LeaseManager.PrepareUpstreamPlanReview(cmd.Context(), issue)
	if err != nil {
		return err
	}
	var reporter *workflow.ProgressReporter
	if progress {
		reporter = opts.newProgressReporter(cmd.Context(), workProgressPrinter(progressWriter(cmd)))
		defer reporter.Close()
		fmt.Fprintf(cmd.ErrOrStderr(), "Issue #%d review started\n", issue)
	}
	verdict, _, err := engine.RunUpstreamPlanReview(cmd.Context(), observation, reviewer, headless, reporter)
	if errors.Is(err, workflow.ErrPlanReviewRejected) && asJSON {
		jsonWritten = true
		if outputErr := writeJSON(cmd, map[string]any{"kind": "review", "issue_number": issue, "reviewer_kind": "agent", "verdict": verdict, "advisory": observation.HumanGate(), "status": "changes-requested", "source": observation.Source()}); outputErr != nil {
			return outputErr
		}
		return err
	}
	if err != nil {
		return err
	}
	if progress {
		fmt.Fprintf(cmd.ErrOrStderr(), "Issue #%d review completed\n", issue)
	}
	advisory := observation.HumanGate()
	if asJSON {
		return writeJSON(cmd, map[string]any{"kind": "review", "issue_number": issue, "reviewer_kind": "agent", "verdict": verdict, "advisory": advisory, "status": "completed", "source": observation.Source()})
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Agent review recorded for issue #%d: %s (advisory: %t)\n", issue, verdict, advisory)
	return nil
}
