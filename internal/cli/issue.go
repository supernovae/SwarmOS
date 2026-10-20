package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
)

func newIssueCmd(rootOpts *RootOptions) *cobra.Command {
	issueCmd := &cobra.Command{
		Use:   "issue",
		Short: "Create or lint SwarmOS contract-driven issues",
	}

	var (
		title       string
		objective   string
		constraints []string
		acceptance  []string
		workflow    string
		gate        string
		ready       bool
	)

	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Create a structured SwarmOS WorkUnit issue",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(title) == "" {
				return fmt.Errorf("--title is required")
			}
			wu := &contract.WorkUnit{
				ID:          "ISSUE-NEW",
				Title:       title,
				Objective:   objective,
				Constraints: constraints,
				Acceptance:  acceptance,
				Workflow:    workflow,
				Gate:        contract.GateMode(gate),
			}
			if err := contract.ValidateWorkUnit(wu); err != nil {
				return err
			}

			tr, err := rootOpts.resolveTracker(cmd)
			if err != nil {
				return err
			}

			var labels []string
			if ready {
				labels = append(labels, "swarm:ready")
			}

			body := contract.FormatIssueMarkdown(wu)
			iss, err := tr.CreateIssue(cmd.Context(), title, body, labels)
			if err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Created issue #%d: %s (%s)\n", iss.Number, iss.Title, iss.HTMLURL)
			return nil
		},
	}

	createCmd.Flags().StringVar(&title, "title", "", "Issue title")
	createCmd.Flags().StringVar(&objective, "objective", "", "WorkUnit objective")
	createCmd.Flags().StringArrayVar(&constraints, "constraint", nil, "Constraint bullet (may be specified multiple times)")
	createCmd.Flags().StringArrayVar(&acceptance, "acceptance", nil, "Acceptance criterion (may be specified multiple times)")
	createCmd.Flags().StringVar(&workflow, "workflow", "feature", "Workflow name (feature, bugfix, refactor)")
	createCmd.Flags().StringVar(&gate, "gate", "none", "Plan review gate (none, agent, human)")
	createCmd.Flags().BoolVar(&ready, "ready", true, "Apply swarm:ready label on creation")

	lintCmd := &cobra.Command{
		Use:   "lint <issue-number>",
		Short: "Validate an existing issue against the SwarmOS WorkUnit JSON schema",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			wu, err := contract.ParseWorkUnitFromIssueWithRoot(iss.Number, iss.Title, iss.Body, rootOpts.resolveRepoRoot())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Issue #%d (%s) is a valid WorkUnit [workflow=%s, gate=%s, constraints=%d, acceptance=%d]\n",
				wu.IssueNumber, wu.ID, wu.Workflow, wu.Gate, len(wu.Constraints), len(wu.Acceptance))
			return nil
		},
	}

	issueCmd.AddCommand(createCmd, lintCmd)
	return issueCmd
}
