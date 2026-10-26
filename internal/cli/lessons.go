package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/workflow"
)

func newLessonsCmd(rootOpts *RootOptions) *cobra.Command {
	var limit int
	var apply bool
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "lessons",
		Short: "Propose additions to .swarm/rules/lessons.md from what agents learned in merged PRs",
		Long: "Collects the \"What we learned\" notes agents recorded in recently merged SwarmOS PRs, skips lessons " +
			".swarm/rules/lessons.md already has, and lists the rest. With --apply, appends them to the local file " +
			"so you can review and commit them like any other change.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			defer func() { reportJSONFailure(cmd, asJSON, "lessons", runErr) }()
			if limit <= 0 {
				return fmt.Errorf("--limit must be a positive number of pull requests")
			}
			repoRoot := rootOpts.resolveRepoRoot()
			tr, mgr, cfg, err := rootOpts.resolveLeaseManager(cmd)
			if err != nil {
				return err
			}
			eng := workflow.NewEngineWithManagerOptions(repoRoot, tr, mgr, cfg, rootOpts.Fork, rootOpts.effectiveForkRemote(), rootOpts.engineOptions())
			report, err := eng.HarvestLessons(cmd.Context(), repoRoot, limit, apply)
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd, map[string]any{"kind": "lessons", "report": report})
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Checked %d recently closed PRs (%d merged).\n", report.ScannedPRs, report.MergedPRs)
			if len(report.Proposed) == 0 {
				fmt.Fprintf(out, "No new lessons to add to %s.\n", report.Path)
				return nil
			}
			verb := "New lessons for"
			if report.Applied {
				verb = "Added to"
			}
			fmt.Fprintf(out, "%s %s:\n", verb, report.Path)
			for _, l := range report.Proposed {
				fmt.Fprintf(out, "  - %s (learned in #%d, PR #%d)\n", l.Text, l.IssueNumber, l.PRNumber)
			}
			if report.Applied {
				fmt.Fprintln(out, "Review the change with `git diff`, edit anything that reads wrong, and commit it. New tasks pick it up once it is merged.")
			} else {
				fmt.Fprintln(out, "Run again with --apply to append these to the file.")
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "How many recently closed pull requests to check")
	cmd.Flags().BoolVar(&apply, "apply", false, "Append new lessons to .swarm/rules/lessons.md in the local checkout")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit the lessons report as JSON")
	return cmd
}
