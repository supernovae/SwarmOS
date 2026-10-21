package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

func newReviewCmd(rootOpts *RootOptions) *cobra.Command {
	var agentName, notes string
	var approve, reject, headless, asJSON bool
	cmd := &cobra.Command{
		Use:   "review <issue-number>",
		Short: "Run an isolated agent review or record an authorized human decision",
		Args:  cobra.ExactArgs(1),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if approve && reject {
				return fmt.Errorf("--approve and --reject are mutually exclusive")
			}
			if (approve || reject) && agentName != "" {
				return fmt.Errorf("--agent cannot be combined with a human decision")
			}
			if notes != "" && !approve && !reject {
				return fmt.Errorf("--notes requires --approve or --reject")
			}
			if rootOpts.Fork && (approve || reject) {
				return fmt.Errorf("human review (--approve and --reject) is maintainer-only and cannot be used with --fork")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			issue, err := strconv.Atoi(strings.TrimPrefix(args[0], "#"))
			if err != nil || issue <= 0 {
				return fmt.Errorf("invalid issue number %q", args[0])
			}
			if !approve && !reject {
				work := newWorkCmd(rootOpts)
				work.SetOut(cmd.OutOrStdout())
				work.SetErr(cmd.ErrOrStderr())
				workArgs := []string{strconv.Itoa(issue), "--phase", "review", "--headless=" + strconv.FormatBool(headless)}
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
			if _, err := manager.RecordHumanReview(cmd.Context(), issue, verdict, notes); err != nil {
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
				return writeJSON(cmd, map[string]any{"kind": "review", "issue_number": issue, "reviewer_kind": "human", "verdict": verdict})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Human review recorded for issue #%d: %s\n", issue, verdict)
			return nil
		},
	}
	cmd.Flags().StringVar(&agentName, "agent", "", "Configured reviewer override")
	cmd.Flags().BoolVar(&approve, "approve", false, "Record an authenticated human approval")
	cmd.Flags().BoolVar(&reject, "reject", false, "Record an authenticated human rejection")
	cmd.Flags().StringVar(&notes, "notes", "", "Human decision notes")
	cmd.Flags().BoolVar(&headless, "headless", true, "Run reviewer non-interactively")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit a structured review outcome")
	return cmd
}
