package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/workflow"
)

func newPlanCmd(rootOpts *RootOptions) *cobra.Command {
	var fromFile string
	var scanOpen bool
	var normalize bool
	var agentName string
	var apply bool
	var ready bool
	var publishIssue int
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Decompose a project roadmap into WorkUnit issues (--from) or scan/import open issues against project goals (--scan)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if fromFile != "" && scanOpen {
				return fmt.Errorf("--from and --scan are mutually exclusive")
			}
			if fromFile == "" && !scanOpen {
				return fmt.Errorf("specify --from <file> or --scan")
			}
			if ready && !apply {
				return fmt.Errorf("--ready requires --apply")
			}
			repoRoot := rootOpts.resolveRepoRoot()
			tr, err := rootOpts.resolveTracker(cmd)
			if err != nil {
				return err
			}
			eng, err := workflow.NewEngine(repoRoot, tr)
			if err != nil {
				return err
			}
			report, err := eng.RunProjectPlan(cmd.Context(), workflow.ProjectPlanOptions{
				RepoRoot:     repoRoot,
				FromFile:     fromFile,
				ScanOpen:     scanOpen,
				Normalize:    normalize,
				AgentName:    agentName,
				Apply:        apply,
				Ready:        ready,
				PublishIssue: publishIssue,
			})
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd, map[string]any{
					"kind":   "project-plan",
					"report": report,
				})
			}
			fmt.Fprint(cmd.OutOrStdout(), report.SummaryMarkdown)
			return nil
		},
	}

	cmd.Flags().StringVar(&fromFile, "from", "", "Path to Markdown or YAML project roadmap/plan file")
	cmd.Flags().BoolVar(&scanOpen, "scan", false, "Scan, sense-make, and audit open tracker issues against project goals and rules")
	cmd.Flags().BoolVar(&normalize, "normalize", false, "When used with --scan --apply, convert unstructured raw issues into structured WorkUnit markdown")
	cmd.Flags().StringVar(&agentName, "agent", "", "Optional configured planner agent to draft/enrich the project plan")
	cmd.Flags().BoolVar(&apply, "apply", false, "Create/update issues, apply triage labels, or publish report on the tracker")
	cmd.Flags().BoolVar(&ready, "ready", false, "When used with --apply, also approve created/normalized WorkUnit issues as swarm:ready")
	cmd.Flags().IntVar(&publishIssue, "publish-issue", 0, "Optional tracking issue number to post the sense-making report to when --apply is set")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit structured project plan report as JSON")
	return cmd
}
