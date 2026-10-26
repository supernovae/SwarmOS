package cli

import (
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/lease"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/workflow"
)

func newIssueCmd(rootOpts *RootOptions) *cobra.Command {
	issueCmd := &cobra.Command{
		Use:   "issue",
		Short: "Manage issue-backed WorkUnits and their execution lifecycle",
		Long:  "An issue is the approved WorkUnit contract. approve authorizes execution; review-plan approves the implementation plan; submit opens the linked draft PR for final GitHub code and acceptance review.",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	var title, objective, workflowName, gate string
	var constraints, acceptance, capabilities []string
	var ready bool
	var dependsOn []int
	var parentIssue int
	var asJSON bool
	createCmd := &cobra.Command{Use: "create", Short: "Create a draft structured SwarmOS WorkUnit issue", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) (runErr error) {
		defer func() { reportJSONFailure(cmd, asJSON, "issue", runErr) }()
		if strings.TrimSpace(title) == "" {
			return fmt.Errorf("--title is required")
		}
		if err := contract.ValidateGate(contract.GateMode(gate)); err != nil {
			return fmt.Errorf("invalid --gate: %w", err)
		}
		if parentIssue < 0 {
			return fmt.Errorf("invalid --parent %d", parentIssue)
		}
		wu := &contract.WorkUnit{ID: "ISSUE-NEW", ParentIssue: parentIssue, DependsOn: dependsOn, RequiredCapabilities: capabilities, Title: title, Objective: objective, Constraints: constraints, Acceptance: acceptance, Workflow: workflowName, Gate: contract.GateMode(gate)}
		if err := contract.ValidateWorkUnit(wu); err != nil {
			return err
		}
		tr, err := rootOpts.resolveTracker(cmd)
		if err != nil {
			return err
		}
		if parentIssue > 0 {
			if _, err := tr.GetIssue(cmd.Context(), parentIssue); err != nil {
				return fmt.Errorf("verify parent issue #%d: %w", parentIssue, err)
			}
		}
		initialLabels := []string{"swarm:draft"}
		if ready {
			initialLabels = nil
		}
		iss, err := tr.CreateIssue(cmd.Context(), title, contract.FormatIssueMarkdown(wu), initialLabels)
		if err != nil {
			return err
		}
		if parentIssue > 0 {
			if err := tr.LinkSubIssue(cmd.Context(), parentIssue, iss.Number); err != nil {
				return fmt.Errorf("created child issue #%d (%s), but failed to link as sub-issue of parent #%d (maintainer can retry linking #%d -> #%d): %w", iss.Number, iss.HTMLURL, parentIssue, parentIssue, iss.Number, err)
			}
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
			if refreshed, refreshErr := tr.GetIssue(cmd.Context(), iss.Number); refreshErr == nil && refreshed != nil {
				iss = refreshed
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
	createCmd.Flags().BoolVar(&asJSON, "json", false, "Emit structured issue results")
	approveCmd := &cobra.Command{Use: "approve <issue-number>", Short: "Approve the current immutable WorkUnit revision for execution", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) (runErr error) {
		defer func() { reportJSONFailure(cmd, asJSON, "task-approval", runErr) }()
		number, err := parseNumberArg(args[0], "issue")
		if err != nil {
			return err
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
			var claimed *lease.AlreadyClaimedError
			if errors.As(err, &claimed) && claimed.Owner == owner {
				return fmt.Errorf("%w; release your claim with swarm issue release %d", err, number)
			}
			return err
		}
		if asJSON {
			return writeJSON(cmd, map[string]any{"kind": "task-approval", "issue_number": number, "state": st})
		}
		if st.CheckpointDropped {
			fmt.Fprintln(cmd.OutOrStdout(), "Dropped empty checkpoint already contained in the approved base.")
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Approved issue #%d (task=%s policy=%s base=%s)\n", number, st.TaskDigest, st.PolicyDigest, st.BaseSHA)
		return nil
	}}
	approveCmd.Flags().BoolVar(&asJSON, "json", false, "Emit structured approval results")
	lintCmd := &cobra.Command{Use: "lint <issue-number>", Short: "Validate an existing issue against the SwarmOS WorkUnit JSON schema", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) (runErr error) {
		defer func() { reportJSONFailure(cmd, asJSON, "issue-lint", runErr) }()
		num, err := parseNumberArg(args[0], "issue")
		if err != nil {
			return err
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
		// Shape and hints are advisory and never change the lint result, so the
		// working tree's protected paths are good enough here.
		repoRoot := rootOpts.resolveRepoRoot()
		spec, _ := harness.LoadValidationSpec(repoRoot)
		shape := workflow.AssessWorkUnitShape(repoRoot, wu, spec)
		if asJSON {
			return writeJSON(cmd, map[string]any{"kind": "issue-lint", "status": "valid", "work_unit": wu, "shape": shape})
		}
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Issue #%d (%s) is a valid WorkUnit [workflow=%s, gate=%s, constraints=%d, acceptance=%d, depends_on=%v, capabilities=%v]\n", wu.IssueNumber, wu.ID, wu.Workflow, wu.Gate, len(wu.Constraints), len(wu.Acceptance), wu.DependsOn, wu.RequiredCapabilities)
		fmt.Fprintf(out, "Suggested shape: %s", shape.Shape.Title())
		if len(shape.Signals) > 0 {
			fmt.Fprintf(out, " (%s)", strings.Join(shape.Signals, "; "))
		}
		fmt.Fprintln(out)
		for _, hint := range shape.Hints {
			fmt.Fprintf(out, "  hint: %s\n", hint)
		}
		return nil
	}}
	lintCmd.Flags().BoolVar(&asJSON, "json", false, "Emit structured lint results")
	issueCmd.AddCommand(newIssueEditCmd(rootOpts), newReconcileCmd(rootOpts), newCleanupCmd(rootOpts), createCmd, approveCmd, lintCmd, newDecomposeCmd(rootOpts), newIssueEpicCmd(rootOpts), newClaimCmd(rootOpts), newWorkCmd(rootOpts), newStageCmd(rootOpts), newReviewCmd(rootOpts), newValidateCmd(rootOpts), newSubmitCmd(rootOpts), newStatusCmd(rootOpts), newRenewCmd(rootOpts), newReleaseCmd(rootOpts), newRevokeCmd(rootOpts))
	return issueCmd
}

func newIssueEpicCmd(rootOpts *RootOptions) *cobra.Command {
	epicCmd := &cobra.Command{
		Use:   "epic",
		Short: "Create or manage non-executable Epic planning issues",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	var title string
	var bodyFile string
	var asJSON bool
	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Create a standalone typed Epic issue from a body file (maintainer only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			defer func() { reportJSONFailure(cmd, asJSON, "epic", runErr) }()
			if strings.TrimSpace(title) == "" {
				return fmt.Errorf("--title is required")
			}
			if strings.TrimSpace(bodyFile) == "" {
				return fmt.Errorf("--body-file is required")
			}
			rawBody, err := os.ReadFile(bodyFile)
			if err != nil {
				return fmt.Errorf("read epic body file %q: %w", bodyFile, err)
			}
			if strings.TrimSpace(string(rawBody)) == "" {
				return fmt.Errorf("epic body file %q is empty", bodyFile)
			}
			tr, err := rootOpts.resolveTracker(cmd)
			if err != nil {
				return err
			}
			iss, err := tr.CreateTypedIssue(cmd.Context(), strings.TrimSpace(title), string(rawBody), nil, "Epic")
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd, map[string]any{"kind": "epic", "status": "created", "issue": iss})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created Epic issue #%d: %s (%s)\n", iss.Number, iss.Title, iss.HTMLURL)
			return nil
		},
	}
	createCmd.Flags().StringVar(&title, "title", "", "Epic issue title")
	createCmd.Flags().StringVar(&bodyFile, "body-file", "", "Path to Markdown file containing the Epic body")
	createCmd.Flags().BoolVar(&asJSON, "json", false, "Emit structured Epic creation output")
	epicCmd.AddCommand(createCmd)
	return epicCmd
}

func newDecomposeCmd(rootOpts *RootOptions) *cobra.Command {
	var sequential bool
	var asJSON bool
	cmd := &cobra.Command{Use: "decompose <issue-number>", Short: "Partition a parent WorkUnit issue into linked child WorkUnit issues", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) (runErr error) {
		defer func() { reportJSONFailure(cmd, asJSON, "decomposition", runErr) }()
		parentNum, err := parseNumberArg(args[0], "issue")
		if err != nil {
			return err
		}
		repoRoot := rootOpts.resolveRepoRoot()
		tr, err := rootOpts.resolveTracker(cmd)
		if err != nil {
			return err
		}
		eng, err := workflow.NewEngineWithOptions(repoRoot, tr, rootOpts.engineOptions())
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
