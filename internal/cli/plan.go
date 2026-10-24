package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/workflow"
)

func newPlanCmd(rootOpts *RootOptions) *cobra.Command {
	var fromFile string
	var exportPath string
	var scanOpen bool
	var normalize bool
	var agentName string
	var apply bool
	var ready bool
	var publishIssue int
	var staleDays int
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
			if publishIssue != 0 && !apply {
				return fmt.Errorf("--publish-issue requires --apply")
			}
			if publishIssue < 0 {
				return fmt.Errorf("--publish-issue must be a positive issue number")
			}
			if normalize && !scanOpen {
				return fmt.Errorf("--normalize requires --scan")
			}
			if cmd.Flags().Changed("stale-days") && !scanOpen {
				return fmt.Errorf("--stale-days requires --scan")
			}
			if staleDays < 0 {
				return fmt.Errorf("--stale-days must be zero or a positive number of days")
			}
			if rootOpts.Fork && (apply || ready) {
				return fmt.Errorf("swarm repo plan --apply and --ready are maintainer-only and cannot be used with --fork")
			}
			var exportFormat string
			if cmd.Flags().Changed("export") {
				if exportPath == "" {
					return fmt.Errorf("--export requires a nonempty path")
				}
				var err error
				exportFormat, err = workflow.ProjectPlanExportFormat(exportPath)
				if err != nil {
					return err
				}
			}
			repoRoot := rootOpts.resolveRepoRoot()
			tr, mgr, cfg, err := rootOpts.resolveLeaseManager(cmd)
			if err != nil {
				return err
			}
			eng := workflow.NewEngineWithManager(repoRoot, tr, mgr, cfg, rootOpts.Fork, rootOpts.effectiveForkRemote())
			report, err := eng.RunProjectPlan(cmd.Context(), workflow.ProjectPlanOptions{
				RepoRoot:     repoRoot,
				FromFile:     fromFile,
				ScanOpen:     scanOpen,
				Normalize:    normalize,
				AgentName:    agentName,
				Apply:        apply,
				Ready:        ready,
				PublishIssue: publishIssue,
				StaleDays:    staleDays,
			})
			if report == nil {
				return err
			}
			// A report with an error means the plan was applied but its summary
			// could not be published; still show and export what was applied.
			publishErr := err
			if exportFormat != "" {
				data, err := workflow.MarshalProjectPlanReport(report, exportFormat)
				if err != nil {
					return fmt.Errorf("export project plan: %w", err)
				}
				if err := os.WriteFile(exportPath, data, 0o644); err != nil {
					return fmt.Errorf("export project plan to %q: %w", exportPath, err)
				}
			}
			if asJSON {
				if err := writeJSON(cmd, map[string]any{
					"kind":   "project-plan",
					"report": report,
				}); err != nil {
					return err
				}
				return publishErr
			}
			fmt.Fprint(cmd.OutOrStdout(), report.SummaryMarkdown)
			return publishErr
		},
	}

	cmd.Flags().StringVar(&exportPath, "export", "", "Export project plan snapshot to a .yaml, .yml, or .json file")
	cmd.Flags().StringVar(&fromFile, "from", "", "Path to Markdown or YAML project roadmap/plan file")
	cmd.Flags().BoolVar(&scanOpen, "scan", false, "Scan, sense-make, and audit open tracker issues against project goals and rules")
	cmd.Flags().BoolVar(&normalize, "normalize", false, "When used with --scan --apply, convert unstructured raw issues into structured WorkUnit markdown")
	cmd.Flags().StringVar(&agentName, "agent", "", "Optional configured planner agent to draft/enrich the project plan")
	cmd.Flags().BoolVar(&apply, "apply", false, "Create/update issues, apply triage labels, or publish report on the tracker")
	cmd.Flags().BoolVar(&ready, "ready", false, "When used with --apply, also approve created/normalized WorkUnit issues as swarm:ready")
	cmd.Flags().IntVar(&publishIssue, "publish-issue", 0, "Optional tracking issue number to post the sense-making report to when --apply is set")
	cmd.Flags().IntVar(&staleDays, "stale-days", workflow.DefaultStaleDays, "With --scan, days without activity before an issue is listed as quiet (0 turns this off)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit structured project plan report as JSON")
	return cmd
}
