package cli

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workspace"
)

func newSubmitCmd(rootOpts *RootOptions) *cobra.Command {
	var baseBranch string

	cmd := &cobra.Command{
		Use:   "submit [<issue-number>]",
		Short: "Validate, strip ephemeral markers, commit worktree changes, and open an artifact-backed Draft PR",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repoRoot := rootOpts.resolveRepoRoot()
			issueNum, wtPath, err := resolveIssueAndWorktree(repoRoot, args)
			if err != nil {
				return err
			}

			tr, mgr, _, err := rootOpts.resolveLeaseManager(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			owner, _ := tr.CurrentUser(ctx)
			if owner == "" {
				owner = "local-dev"
			}

			iss, err := tr.GetIssue(ctx, issueNum)
			if err != nil {
				return err
			}
			wu, err := contract.ParseWorkUnitFromIssueWithRoot(iss.Number, iss.Title, iss.Body, repoRoot)
			if err != nil {
				return err
			}

			valSpec, err := harness.LoadValidationSpec(wtPath)
			if err != nil {
				valSpec, err = harness.LoadValidationSpec(repoRoot)
				if err != nil {
					return err
				}
			}

			// 1. Run validation before submitting
			valReport, err := validation.Validate(ctx, wtPath, wu, valSpec)
			if err != nil {
				return fmt.Errorf("cannot submit before validation passes: %w", err)
			}
			st, _ := mgr.UpdateArtifact(ctx, issueNum, owner, valReport.Artifact)

			// 2. Strip ephemeral harness markers from AGENTS.md / CLAUDE.md and synced untracked files
			if err := workspace.CleanEphemeralHarnessMarkers(wtPath); err != nil {
				return err
			}

			// 3. Commit worktree changes on swarm/issue-<N>-<slug>
			branchName := workspace.BranchName(issueNum, iss.Title)
			if out, err := exec.Command("git", "-C", wtPath, "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
				if b := strings.TrimSpace(string(out)); b != "" && b != "HEAD" {
					branchName = b
				}
			}

			_ = exec.Command("git", "-C", wtPath, "config", "user.name", owner).Run()
			_ = exec.Command("git", "-C", wtPath, "config", "user.email", owner+"@users.noreply.github.com").Run()

			if out, err := exec.Command("git", "-C", wtPath, "add", "-A").CombinedOutput(); err != nil {
				return fmt.Errorf("git add failed: %w (%s)", err, string(out))
			}

			statusOut, _ := exec.Command("git", "-C", wtPath, "status", "--porcelain").Output()
			commitMsg := fmt.Sprintf("feat(swarm): resolve #%d — %s", issueNum, iss.Title)
			if strings.TrimSpace(string(statusOut)) != "" {
				if out, err := exec.Command("git", "-C", wtPath, "commit", "-m", commitMsg).CombinedOutput(); err != nil {
					return fmt.Errorf("git commit failed: %w (%s)", err, string(out))
				}
			}

			// Push branch if using GitHub backend and origin remote exists
			if strings.HasPrefix(tr.BackendName(), "github") && hasGitRemote(wtPath) {
				if out, err := exec.Command("git", "-C", wtPath, "push", "-u", "origin", branchName).CombinedOutput(); err != nil {
					return fmt.Errorf("git push failed: %w (%s)", err, string(out))
				}
			}

			// 4. Create Draft PR with Objective, Acceptance Checklist, and Artifact Summary Table
			if st == nil {
				st, _ = mgr.GetCurrentState(ctx, issueNum)
			}
			prBody := formatDraftPRBody(wu, st)
			pr, err := tr.CreateDraftPR(ctx, tracker.DraftPRRequest{
				IssueNumber: issueNum,
				Title:       fmt.Sprintf("%s (#%d)", iss.Title, issueNum),
				Body:        prBody,
				Head:        branchName,
				Base:        baseBranch,
			})
			if err != nil {
				return err
			}

			_, _ = mgr.UpdatePhase(ctx, issueNum, owner, "submitted", "completed")
			_ = tr.RemoveLabel(ctx, issueNum, "swarm:in-progress")
			_ = tr.AddLabels(ctx, issueNum, []string{"swarm:pr-open"})

			fmt.Fprintf(cmd.OutOrStdout(), "Submitted Draft PR #%d for issue #%d on branch %s (%s)\n",
				pr.Number, issueNum, branchName, pr.HTMLURL)
			return nil
		},
	}

	cmd.Flags().StringVar(&baseBranch, "base", "main", "Base branch for Draft PR")
	return cmd
}

func formatDraftPRBody(wu *contract.WorkUnit, st *lease.IssueState) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Closes #%d\n\n", wu.IssueNumber))
	sb.WriteString("## Objective\n\n")
	sb.WriteString(strings.TrimSpace(wu.Objective) + "\n\n")

	sb.WriteString("## Acceptance Criteria Checklist\n\n")
	for _, a := range wu.Acceptance {
		sb.WriteString("- [x] " + a + "\n")
	}
	sb.WriteString("\n")

	if st != nil && len(st.Artifacts) > 0 {
		sb.WriteString("## SwarmOS Artifact Summary\n\n")
		sb.WriteString("| Artifact Type | Producer | Verdict | Summary |\n| :--- | :--- | :--- | :--- |\n")
		for _, art := range st.Artifacts {
			v := art.Verdict
			if v == "" {
				v = "-"
			}
			sb.WriteString(fmt.Sprintf("| `%s` (v%d) | `%s` | `%s` | %s |\n", art.Type, art.Version, art.Producer, v, art.Summary))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
