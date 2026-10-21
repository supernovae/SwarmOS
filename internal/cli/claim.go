package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/workflow"
	"github.com/supernovae/swarmos/internal/workspace"
)

func newClaimCmd(rootOpts *RootOptions) *cobra.Command {
	var agentName, durationStr string
	var asJSON bool
	cmd := &cobra.Command{Use: "claim <issue-number>", Short: "Reserve an approved task and prepare its pinned worktree", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		issue, err := strconv.Atoi(strings.TrimPrefix(args[0], "#"))
		if err != nil || issue <= 0 {
			return fmt.Errorf("invalid issue number %q", args[0])
		}
		var duration time.Duration
		if durationStr != "" {
			duration, err = time.ParseDuration(durationStr)
			if err != nil || duration <= 0 {
				return fmt.Errorf("--duration must be positive")
			}
		}
		root := rootOpts.resolveRepoRoot()
		tr, manager, cfg, err := rootOpts.resolveLeaseManager(cmd)
		if err != nil {
			return err
		}
		engine := workflow.NewEngineWithManager(root, tr, manager, cfg, rootOpts.Fork, rootOpts.effectiveForkRemote())
		preflight, err := engine.Preflight(cmd.Context(), workflow.WorkOptions{RepoRoot: root, IssueNumber: issue, AgentName: agentName, Phase: "plan"})
		if err != nil {
			return err
		}
		if duration == 0 {
			duration, err = time.ParseDuration(preflight.Config.Lease.Duration)
			if err != nil {
				return err
			}
		}
		owner, err := tr.CurrentUser(cmd.Context())
		if err != nil {
			return err
		}
		lock, err := lockClaim(root, issue)
		if err != nil {
			return err
		}
		defer unlockClaim(lock)
		state, err := manager.Acquire(cmd.Context(), issue, owner, preflight.Agents["planner"], "plan", duration, preflight.Config.Lease.MaxRenewals)
		if err != nil {
			return err
		}
		fail := func(cause error) error {
			_, releaseErr := manager.Release(cmd.Context(), issue, owner, "claim setup failed")
			return errors.Join(cause, releaseErr)
		}
		if err := saveClaimToken(root, issue, state.Lease.ClaimID); err != nil {
			return fail(err)
		}
		checkpointRemote := "origin"
		targetBranch := state.Branch
		if rootOpts.Fork {
			checkpointRemote = rootOpts.effectiveForkRemote()
			if targetBranch == "" {
				targetBranch = workspace.ForkBranchName(issue, state.Lease.ClaimID)
			}
		}
		path, branch, err := workspace.ProvisionFromRemoteAt(root, checkpointRemote, issue, preflight.Issue.Title, state.BaseSHA, targetBranch, state.HeadSHA)
		if err != nil {
			return fail(err)
		}
		if err := workspace.CompileHarnessBundleAt(path, preflight.WorkUnit, preflight.Workflow, state.Artifacts, "planner", state.BaseSHA); err != nil {
			return fail(err)
		}
		if asJSON {
			return writeJSON(cmd, map[string]any{"kind": "claim", "issue_number": issue, "state": state, "branch": branch, "worktree": path})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Claimed issue #%d as %s [agent=%s expires=%s]\nBranch: %s\nWorktree: %s\n", issue, owner, state.Lease.Agent, state.Lease.ExpiresAt.Format(time.RFC3339), branch, path)
		return nil
	}}
	cmd.Flags().StringVar(&agentName, "agent", "", "Configured planner override")
	cmd.Flags().StringVar(&durationStr, "duration", "", "Lease duration (approved policy default)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit a structured claim outcome")
	return cmd
}
