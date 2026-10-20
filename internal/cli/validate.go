package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workspace"
)

func newValidateCmd(rootOpts *RootOptions) *cobra.Command {
	var (
		ciMode              bool
		allowHarnessChanges bool
	)
	cmd := &cobra.Command{
		Use:   "validate [<issue-number>]",
		Short: "Run protected-path tamper checks, artifact verification, and required-checks.yaml",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repoRoot := rootOpts.resolveRepoRoot()
			var (
				issueNum int
				wtPath   string
				wu       *contract.WorkUnit
			)

			if ciMode && len(args) == 0 {
				wtPath = repoRoot
				wu = &contract.WorkUnit{ID: "CI", IssueNumber: 1, AllowHarnessChanges: allowHarnessChanges}
			} else {
				var err error
				issueNum, wtPath, err = resolveIssueAndWorktree(repoRoot, args)
				if err != nil {
					return err
				}
				wu, _ = workspace.LoadActiveWorkUnit(wtPath)
				if wu == nil && issueNum > 0 {
					if tr, tErr := rootOpts.resolveTracker(cmd); tErr == nil {
						if iss, iErr := tr.GetIssue(cmd.Context(), issueNum); iErr == nil {
							wu, _ = contract.ParseWorkUnitFromIssueWithRoot(iss.Number, iss.Title, iss.Body, repoRoot)
						}
					}
				}
			}
			if wu != nil && allowHarnessChanges {
				wu.AllowHarnessChanges = true
			}
			valSpec, err := harness.LoadValidationSpec(wtPath)
			if err != nil {
				valSpec, err = harness.LoadValidationSpec(repoRoot)
				if err != nil {
					return err
				}
			}

			report, valErr := validation.ValidateWithOptions(cmd.Context(), wtPath, wu, valSpec, validation.ValidateOptions{
				CIMode: ciMode,
			})

			if report != nil && issueNum > 0 && !ciMode {
				if tr, mgr, _, mErr := rootOpts.resolveLeaseManager(cmd); mErr == nil {
					owner, _ := tr.CurrentUser(cmd.Context())
					_, _ = mgr.UpdateArtifact(cmd.Context(), issueNum, owner, report.Artifact)
				}
			}

			out := cmd.OutOrStdout()
			if report != nil {
				for _, chk := range report.CheckOutcomes {
					status := "PASS"
					if !chk.Passed {
						status = "FAIL"
					}
					fmt.Fprintf(out, "[%s] %s (%s)\n", status, chk.Name, chk.Command)
				}
			}
			if valErr != nil {
				return valErr
			}
			fmt.Fprintln(out, "Validation PASSED")
			return nil
		},
	}

	cmd.Flags().BoolVar(&ciMode, "ci", false, "Run in CI mode (skip .swarm/active worktree artifact checks)")
	cmd.Flags().BoolVar(&allowHarnessChanges, "allow-harness-changes", false, "Permit modifications to protected harness/CI paths")
	return cmd
}
