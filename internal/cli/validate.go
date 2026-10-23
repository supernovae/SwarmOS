package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workspace"
)

func newValidateCmd(options *RootOptions) *cobra.Command {
	var asJSON bool
	var baseSHA string
	cmd := &cobra.Command{Use: "validate [<issue-number>]", Short: "Run issue checks and protected-path policy from the approved immutable base", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		root := options.resolveRepoRoot()
		ctx := cmd.Context()
		issue, worktree, err := resolveIssueAndWorktree(root, args)
		if err != nil {
			return err
		}
		lock, err := lease.LockClaim(root, issue)
		if err != nil {
			return err
		}
		defer lease.UnlockClaim(lock)
		tr, manager, _, err := options.resolveLeaseManager(cmd)
		if err != nil {
			return err
		}
		owner, err := tr.CurrentUser(ctx)
		if err != nil {
			return err
		}
		claim, err := lease.LoadClaimToken(root, issue)
		if err != nil {
			return err
		}
		manager.BindClaim(issue, claim)
		if err := manager.CheckClaim(ctx, issue, owner); err != nil {
			return err
		}
		state, err := manager.GetCurrentState(ctx, issue)
		if err != nil {
			return err
		}
		if err := requireIssueWorkspace(state); err != nil {
			return err
		}
		if baseSHA != "" && baseSHA != state.BaseSHA {
			return fmt.Errorf("--base does not match the task's approved immutable base")
		}
		baseSHA = state.BaseSHA
		_ = workspace.FetchCommit(root, "origin", baseSHA)
		iss, err := tr.GetIssue(ctx, issue)
		if err != nil {
			return err
		}
		unit, err := contract.ParseWorkUnitFromIssue(iss.Number, iss.Title, iss.Body)
		if err != nil {
			return err
		}
		cfg, err := harness.LoadConfigAt(root, baseSHA)
		if err != nil {
			return err
		}
		duration, err := time.ParseDuration(cfg.Lease.Duration)
		if err != nil {
			return err
		}
		renewed, stop := maintainClaim(ctx, manager, issue, owner, duration)
		defer stop()
		ctx = renewed
		report, validationErr := validation.Validate(ctx, worktree, unit, validation.ValidateOptions{CIMode: false, BaseSHA: baseSHA, ForkMode: options.Fork})
		if report != nil {
			if state != nil {
				for _, att := range state.Attestations {
					if att.Role == "tester" {
						if report.Artifact.Metadata == nil {
							report.Artifact.Metadata = make(map[string]string)
						}
						report.Artifact.Metadata["role"] = "tester"
						report.Artifact.Metadata["agent"] = att.Agent
						report.Artifact.Metadata["harness"] = att.Harness
						report.Artifact.Metadata["harness_version"] = att.HarnessVersion
						report.Artifact.Metadata["execution_mode"] = att.ExecutionMode
						report.Artifact.Metadata["authentication"] = att.Authentication
						report.Artifact.Metadata["model"] = att.Model
						report.Artifact.Metadata["sandbox"] = att.Sandbox
						break
					}
				}
			}
			_, err := manager.UpdateArtifact(ctx, issue, owner, report.Artifact)
			validationErr = errors.Join(validationErr, err)
		}
		return writeValidationOutcome(cmd, report, validationErr, asJSON)
	}}
	cmd.Flags().StringVar(&baseSHA, "base", "", "Approved immutable task base SHA")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit a structured validation outcome")
	return cmd
}

func newRepoValidateCmd(options *RootOptions) *cobra.Command {
	var baseSHA string
	var asJSON bool
	cmd := &cobra.Command{Use: "validate", Short: "Validate a repository checkout against a trusted immutable base", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		if baseSHA == "" {
			return fmt.Errorf("--base <trusted-full-commit-SHA> is required")
		}
		report, err := validation.Validate(cmd.Context(), options.resolveRepoRoot(), nil, validation.ValidateOptions{CIMode: true, BaseSHA: baseSHA, ForkMode: options.Fork})
		return writeValidationOutcome(cmd, report, err, asJSON)
	}}
	cmd.Flags().StringVar(&baseSHA, "base", "", "Trusted full immutable base commit SHA")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit a structured validation outcome")
	return cmd
}

func writeValidationOutcome(cmd *cobra.Command, report *validation.ValidationReport, validationErr error, asJSON bool) error {
	if asJSON {
		status := "passed"
		if validationErr != nil {
			status = "failed"
		}
		if err := writeJSON(cmd, map[string]any{"kind": "validation", "status": status, "report": report}); err != nil {
			return err
		}
	} else if report != nil {
		for _, check := range report.CheckOutcomes {
			status := "PASS"
			if !check.Passed {
				status = "FAIL"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", status, check.Name)
		}
	}
	return validationErr
}
