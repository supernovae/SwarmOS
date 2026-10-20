package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/workflow"
)

func newIssueCmd(rootOpts *RootOptions) *cobra.Command {
	issueCmd := &cobra.Command{Use: "issue", Short: "Create, approve, lint, or decompose SwarmOS contract-driven issues"}
	var title, objective, workflowName, gate string
	var constraints, acceptance, capabilities []string
	var ready bool
	var dependsOn []int
	var parentIssue int
	var asJSON bool
	issueCmd.PersistentFlags().BoolVar(&asJSON, "json", false, "Emit structured issue results")
	createCmd := &cobra.Command{Use: "create", Short: "Create a draft structured SwarmOS WorkUnit issue", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(title) == "" {
			return fmt.Errorf("--title is required")
		}
		if err := contract.ValidateGate(contract.GateMode(gate)); err != nil {
			return fmt.Errorf("invalid --gate: %w", err)
		}
		wu := &contract.WorkUnit{ID: "ISSUE-NEW", ParentIssue: parentIssue, DependsOn: dependsOn, RequiredCapabilities: capabilities, Title: title, Objective: objective, Constraints: constraints, Acceptance: acceptance, Workflow: workflowName, Gate: contract.GateMode(gate)}
		if err := contract.ValidateWorkUnit(wu); err != nil {
			return err
		}
		tr, err := rootOpts.resolveTracker(cmd)
		if err != nil {
			return err
		}
		iss, err := tr.CreateIssue(cmd.Context(), title, contract.FormatIssueMarkdown(wu), nil)
		if err != nil {
			return err
		}
		kind := "draft"
		if ready {
			_, mgr, _, err := rootOpts.resolveLeaseManager(cmd)
			if err != nil {
				return err
			}
			owner, err := tr.CurrentUser(cmd.Context())
			if err != nil || strings.TrimSpace(owner) == "" {
				return fmt.Errorf("determine approving actor: %w", err)
			}
			if _, err := mgr.ApproveTask(cmd.Context(), iss.Number, owner); err != nil {
				return fmt.Errorf("approve newly created issue: %w", err)
			}
			kind = "approved"
		}
		if asJSON {
			return writeJSON(cmd, map[string]any{"kind": "issue", "status": kind, "issue": iss})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Created %s issue #%d: %s (%s)\n", kind, iss.Number, iss.Title, iss.HTMLURL)
		return nil
	}}
	createCmd.Flags().StringVar(&title, "title", "", "Issue title")
	createCmd.Flags().StringVar(&objective, "objective", "", "WorkUnit objective")
	createCmd.Flags().StringArrayVar(&constraints, "constraint", nil, "Constraint bullet (may be specified multiple times)")
	createCmd.Flags().StringArrayVar(&acceptance, "acceptance", nil, "Acceptance criterion (may be specified multiple times)")
	createCmd.Flags().StringVar(&workflowName, "workflow", "feature", "Workflow name (feature, bugfix, refactor)")
	createCmd.Flags().StringVar(&gate, "gate", "", "Plan review gate (none, agent, human; omitted inherits policy)")
	createCmd.Flags().BoolVar(&ready, "ready", false, "Approve the immutable task revision after creation")
	createCmd.Flags().IntSliceVar(&dependsOn, "depends-on", nil, "Prerequisite issue numbers that must complete first")
	createCmd.Flags().StringSliceVar(&capabilities, "capability", nil, "Required volunteer node capabilities (e.g., coding,testing,docs)")
	createCmd.Flags().IntVar(&parentIssue, "parent", 0, "Parent epic/workunit issue number")
	approveCmd := &cobra.Command{Use: "approve <issue-number>", Short: "Approve the current immutable WorkUnit revision for execution", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		number, err := strconv.Atoi(strings.TrimPrefix(args[0], "#"))
		if err != nil || number <= 0 {
			return fmt.Errorf("invalid issue number %q", args[0])
		}
		tr, mgr, _, err := rootOpts.resolveLeaseManager(cmd)
		if err != nil {
			return err
		}
		owner, err := tr.CurrentUser(cmd.Context())
		if err != nil || strings.TrimSpace(owner) == "" {
			return fmt.Errorf("determine approving actor: %w", err)
		}
		st, err := mgr.ApproveTask(cmd.Context(), number, owner)
		if err != nil {
			return err
		}
		if asJSON {
			return writeJSON(cmd, map[string]any{"kind": "task-approval", "issue_number": number, "state": st})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Approved issue #%d (task=%s policy=%s base=%s)\n", number, st.TaskDigest, st.PolicyDigest, st.BaseSHA)
		return nil
	}}
	lintCmd := &cobra.Command{Use: "lint <issue-number>", Short: "Validate an existing issue against the SwarmOS WorkUnit JSON schema", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		num, err := strconv.Atoi(strings.TrimPrefix(args[0], "#"))
		if err != nil || num <= 0 {
			return fmt.Errorf("invalid issue number %q", args[0])
		}
		tr, err := rootOpts.resolveTracker(cmd)
		if err != nil {
			return err
		}
		iss, err := tr.GetIssue(cmd.Context(), num)
		if err != nil {
			return err
		}
		wu, err := contract.ParseWorkUnitFromIssue(iss.Number, iss.Title, iss.Body)
		if err != nil {
			return err
		}
		if asJSON {
			return writeJSON(cmd, map[string]any{"kind": "issue-lint", "status": "valid", "work_unit": wu})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Issue #%d (%s) is a valid WorkUnit [workflow=%s, gate=%s, constraints=%d, acceptance=%d, depends_on=%v, capabilities=%v]\n", wu.IssueNumber, wu.ID, wu.Workflow, wu.Gate, len(wu.Constraints), len(wu.Acceptance), wu.DependsOn, wu.RequiredCapabilities)
		return nil
	}}
	issueCmd.AddCommand(createCmd, approveCmd, lintCmd, newDecomposeCmd(rootOpts))
	return issueCmd
}

func newDecomposeCmd(rootOpts *RootOptions) *cobra.Command {
	var sequential bool
	var asJSON bool
	cmd := &cobra.Command{Use: "decompose <issue-number>", Short: "Partition a parent WorkUnit issue into linked child WorkUnit issues", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		parentNum, err := strconv.Atoi(strings.TrimPrefix(args[0], "#"))
		if err != nil || parentNum <= 0 {
			return fmt.Errorf("invalid issue number %q", args[0])
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
		children, err := eng.DecomposeIssue(cmd.Context(), repoRoot, parentNum, nil, sequential)
		if err != nil {
			return err
		}
		if asJSON {
			return writeJSON(cmd, map[string]any{"kind": "decomposition", "parent_issue": parentNum, "children": children})
		}
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Decomposed parent issue #%d into %d child WorkUnit issue(s):\n", parentNum, len(children))
		for _, ch := range children {
			fmt.Fprintf(out, "  + #%d: %s (%s)\n", ch.Number, ch.Title, ch.HTMLURL)
		}
		return nil
	}}
	cmd.Flags().BoolVar(&sequential, "sequential", true, "Chain child issues sequentially via depends_on")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit structured child issues")
	return cmd
}
