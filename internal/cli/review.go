package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/workflow"
	"github.com/supernovae/swarmos/internal/workspace"
)

func newReviewCmd(rootOpts *RootOptions) *cobra.Command {
	var (
		agentName string
		approve   bool
		reject    bool
		notes     string
		headless  bool
	)

	cmd := &cobra.Command{
		Use:   "review <issue-number>",
		Short: "Run an automated reviewer agent or record human plan approval/rejection",
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

			ctx := cmd.Context()
			owner, _ := tr.CurrentUser(ctx)
			if owner == "" {
				owner = "local-dev"
			}

			if approve || reject {
				verdict := "APPROVED"
				summary := "Human approved plan"
				if reject {
					verdict = "CHANGES_REQUESTED"
					summary = "Human requested changes on plan"
				}
				body := fmt.Sprintf("VERDICT: %s\n\n%s\n", verdict, strings.TrimSpace(notes))
				if strings.TrimSpace(notes) == "" {
					body = fmt.Sprintf("VERDICT: %s\n\nReviewed by %s via `swarm review`.\n", verdict, owner)
				}

				wtPath := workspace.WorktreePath(repoRoot, issueNum)
				if info, err := os.Stat(wtPath); err == nil && info.IsDir() {
					_ = os.MkdirAll(filepath.Join(wtPath, ".swarm", "active"), 0o755)
					_ = os.WriteFile(filepath.Join(wtPath, ".swarm", "active", "REVIEW.md"), []byte(body), 0o644)
				}

				if _, err := mgr.UpdateArtifact(ctx, issueNum, owner, contract.Artifact{
					Type:     contract.ArtifactReview,
					Producer: "human:" + owner,
					Verdict:  verdict,
					Summary:  summary,
					Body:     body,
				}); err != nil {
					return err
				}
				if approve {
					_ = tr.RemoveLabel(ctx, issueNum, "swarm:plan-review")
					_ = tr.AddLabels(ctx, issueNum, []string{"swarm:plan-approved"})
				} else {
					_ = tr.AddLabels(ctx, issueNum, []string{"swarm:plan-review"})
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Recorded review verdict %s for issue #%d\n", verdict, issueNum)
				return nil
			}

			if agentName == "" {
				agentName = cfg.DefaultAgent
				if agentName == "" {
					agentName = "claude"
				}
			}

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
			st, err := mgr.GetCurrentState(ctx, issueNum)
			if err != nil {
				return err
			}

			wtPath, _, err := workspace.Provision(repoRoot, issueNum, iss.Title)
			if err != nil {
				return err
			}

			eng, err := workflow.NewEngine(repoRoot, tr)
			if err != nil {
				return err
			}
			mode := "interactive"
			if headless {
				mode = "headless"
			}

			verdict, _, err := eng.RunAgentReview(ctx, wtPath, wu, wf, st, owner, agentName, mode)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Reviewer agent %q completed review for issue #%d: %s\n", agentName, issueNum, verdict)
			return nil
		},
	}

	cmd.Flags().StringVar(&agentName, "agent", "", "Reviewer agent to run (claude, codex, opencode, omp, mock)")
	cmd.Flags().BoolVar(&approve, "approve", false, "Record human approval of the plan")
	cmd.Flags().BoolVar(&reject, "reject", false, "Record human rejection / changes requested")
	cmd.Flags().StringVar(&notes, "notes", "", "Review notes when approving or rejecting")
	cmd.Flags().BoolVar(&headless, "headless", true, "Run reviewer agent in headless mode")
	return cmd
}
