package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/validation"
)

func newValidateCmd(options *RootOptions) *cobra.Command {
	var asJSON bool
	var showProgress bool
	var baseSHA string
	cmd := &cobra.Command{Use: "validate [<issue-number>]", Short: "Run issue checks and protected-path policy from the approved immutable base", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) (runErr error) {
		p := operationProgress(cmd.Context(), cmd.ErrOrStderr(), showProgress)
		defer func() {
			status := "completed"
			if runErr != nil {
				status = "failed"
			}
			p.Finish(status)
		}()
		reported := false
		defer func() {
			if !reported {
				reportJSONFailure(cmd, asJSON, "validation", runErr)
			}
		}()
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
		owner, _, err := bindPersistedClaim(ctx, root, tr, manager, issue)
		if err != nil {
			return err
		}
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
		_, unit, err := loadWorkUnit(ctx, tr, issue)
		if err != nil {
			return err
		}
		_, duration, err := approvedLeasePolicy(root, manager.UpstreamRemote(), baseSHA)
		if err != nil {
			return err
		}
		heartbeat := manager.StartHeartbeat(ctx, issue, owner, duration)
		defer heartbeat.Stop()
		ctx = heartbeat.Context()
		p.Next("validation")
		report, validationErr := validation.Validate(ctx, worktree, unit, validation.ValidateOptions{Version: Version, ClaimID: state.Lease.ClaimID, OnProgress: p.Observe, CIMode: false, BaseSHA: baseSHA, ForkMode: options.Fork, GoFastPass: true, LookupEnv: options.lookupEnv})
		if report != nil {
			report.Artifact.Metadata = lease.TesterMetadata(state, report.Artifact.Metadata)
			lease.PreserveTesterGuidance(state, &report.Artifact)
			_, err := manager.RecordTrustedValidation(ctx, issue, owner, report.Artifact)
			validationErr = errors.Join(validationErr, err)
		}
		reported = true
		return writeValidationOutcome(cmd, report, validationErr, asJSON)
	}}
	cmd.Flags().StringVar(&baseSHA, "base", "", "Trusted base commit SHA; must equal the issue's approved base (defaults to it)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit a structured validation outcome")
	cmd.Flags().BoolVar(&showProgress, "progress", true, "Report progress on stderr")
	return cmd
}

func newRepoValidateCmd(options *RootOptions) *cobra.Command {
	var baseSHA, protectedMode string
	var asJSON bool
	cmd := &cobra.Command{Use: "validate", Short: "Validate a repository checkout against a trusted immutable base", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		if baseSHA == "" {
			err := fmt.Errorf("--base <trusted-full-commit-SHA> is required")
			reportJSONFailure(cmd, asJSON, "validation", err)
			return err
		}
		if protectedMode != "enforce" && protectedMode != "report" {
			err := fmt.Errorf("--protected-paths must be enforce or report, not %q", protectedMode)
			reportJSONFailure(cmd, asJSON, "validation", err)
			return err
		}
		report, err := validation.Validate(cmd.Context(), options.resolveRepoRoot(), nil, validation.ValidateOptions{CIMode: true, RunnerIsolated: onCIRunner(), BaseSHA: baseSHA, ForkMode: options.Fork, ReportProtectedChanges: protectedMode == "report", LookupEnv: options.lookupEnv})
		if !asJSON && report != nil {
			writeProtectedChanges(cmd, report.ProtectedChanges)
		}
		return writeValidationOutcome(cmd, report, err, asJSON)
	}}
	cmd.Flags().StringVar(&baseSHA, "base", "", "Trusted full base commit SHA to validate against (required)")
	cmd.Flags().StringVar(&protectedMode, "protected-paths", "enforce", "Protected-path changes: enforce (fail) or report (list them and run checks, for maintainer pull requests)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit a structured validation outcome")
	return cmd
}

// writeProtectedChanges lists reported protected-path changes so reviewers see
// policy edits at a glance, and says when merging will require re-approval.
func writeProtectedChanges(cmd *cobra.Command, files []string) {
	policy := false
	for _, file := range files {
		fmt.Fprintf(cmd.OutOrStdout(), "PROTECTED %s\n", file)
		policy = policy || strings.HasPrefix(file, ".swarm/")
	}
	if policy {
		fmt.Fprintln(cmd.OutOrStdout(), "NOTE .swarm/ policy changed; approved WorkUnits need swarm issue approve again after this merges")
	}
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
		writeValidationChecks(cmd, report)
	}
	return validationErr
}

// onCIRunner reports whether repo validate runs on a CI runner (GitHub Actions
// and most CI systems set CI=true). There the runner is the isolation boundary;
// on a workstation, checks keep the repository's configured sandbox.
func onCIRunner() bool {
	return os.Getenv("CI") == "true" || os.Getenv("GITHUB_ACTIONS") == "true"
}

func writeValidationChecks(cmd *cobra.Command, report *validation.ValidationReport) {
	for _, check := range report.CheckOutcomes {
		name := validation.EscapeDiagnostic(check.Name)
		if check.Passed {
			fmt.Fprintf(cmd.OutOrStdout(), "PASS %s\n", name)
			continue
		}
		if check.FallbackReason != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "SKIP %s: %s\n", name, validation.EscapeDiagnostic(check.FallbackReason))
			continue
		}
		fmt.Fprintf(cmd.OutOrStdout(), "FAIL %s (exit %d)\n", name, check.ExitCode)
		if len(check.FailingTests) > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "failing tests: %s\n", strings.Join(check.FailingTests, ", "))
		}
		if check.OutputTail != "" {
			fmt.Fprintln(cmd.OutOrStdout(), check.OutputTail)
		}
	}
}
