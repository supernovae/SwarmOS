package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/workspace"
)

func newClaimCmd(rootOpts *RootOptions) *cobra.Command {
	var agentName string
	var durationStr string

	cmd := &cobra.Command{
		Use:   "claim <issue-number>",
		Short: "Verify trust gate, acquire lease on tracker, provision worktree, and compile .swarm/active/WORKUNIT.md",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			issueNum, err := strconv.Atoi(strings.TrimPrefix(args[0], "#"))
			if err != nil || issueNum <= 0 {
				return fmt.Errorf("invalid issue number %q", args[0])
			}
			repoRoot := rootOpts.resolveRepoRoot()
			tr, mgr, cfg, err := rootOpts.resolveLeaseManager(cmd)
			if err != nil {
				return err
			}

			if agentName == "" {
				agentName = cfg.DefaultAgent
				if agentName == "" {
					agentName = "claude"
				}
			}

			dur := 4 * time.Hour
			if durationStr != "" {
				parsed, err := time.ParseDuration(durationStr)
				if err != nil {
					return fmt.Errorf("invalid --duration %q: %w", durationStr, err)
				}
				dur = parsed
			} else if cfg.Lease.Duration != "" {
				if parsed, err := time.ParseDuration(cfg.Lease.Duration); err == nil {
					dur = parsed
				}
			}

			ctx := cmd.Context()
			iss, err := tr.GetIssue(ctx, issueNum)
			if err != nil {
				return err
			}

			wu, err := contract.ParseWorkUnitFromIssueWithRoot(iss.Number, iss.Title, iss.Body, repoRoot)
			if err != nil {
				return err
			}

			wf, err := harness.LoadWorkflow(repoRoot, wu.Workflow)
			if err != nil {
				return err
			}

			owner, err := tr.CurrentUser(ctx)
			if err != nil || owner == "" {
				owner = "local-dev"
			}

			st, err := mgr.Acquire(ctx, issueNum, owner, agentName, "plan", dur, cfg.Lease.MaxRenewals)
			if err != nil {
				return err
			}

			wtPath, branchName, err := workspace.Provision(repoRoot, issueNum, iss.Title)
			if err != nil {
				return err
			}

			if err := workspace.CompileHarnessBundle(wtPath, wu, wf, st.Artifacts, "planner"); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Claimed issue #%d (%s) as %s [agent=%s, expires=%s]\n",
				issueNum, iss.Title, owner, agentName, st.Lease.ExpiresAt.Format(time.RFC3339))
			fmt.Fprintf(out, "  Branch:   %s\n", branchName)
			fmt.Fprintf(out, "  Worktree: %s\n", wtPath)
			fmt.Fprintf(out, "  Contract: %s/.swarm/active/WORKUNIT.md\n", wtPath)
			return nil
		},
	}

	cmd.Flags().StringVar(&agentName, "agent", "", "Agent harness (claude, codex, opencode, omp, mock)")
	cmd.Flags().StringVar(&durationStr, "duration", "", "Lease TTL duration (default 4h)")
	return cmd
}
