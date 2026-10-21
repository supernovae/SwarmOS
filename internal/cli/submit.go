package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workspace"
	"os/exec"
	"strings"
	"time"
)

func newSubmitCmd(rootOpts *RootOptions) *cobra.Command {
	var baseBranch string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "submit [<issue-number>]",
		Short: "Validate the exact checkpoint and idempotently open its draft PR",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := rootOpts.resolveRepoRoot()
			issue, worktree, err := resolveIssueAndWorktree(root, args)
			if err != nil {
				return err
			}
			lock, err := lockClaim(root, issue)
			if err != nil {
				return err
			}
			defer unlockClaim(lock)
			tr, manager, _, err := rootOpts.resolveLeaseManager(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			owner, err := tr.CurrentUser(ctx)
			if err != nil {
				return err
			}
			if owner == "" {
				return fmt.Errorf("submission requires an authenticated owner")
			}
			claim, err := loadClaimToken(root, issue)
			if err != nil {
				return fmt.Errorf("load submission claim: %w", err)
			}
			manager.BindClaim(issue, claim)
			state, err := manager.GetCurrentState(ctx, issue)
			if err != nil {
				return err
			}
			_ = workspace.FetchCommit(root, "origin", state.BaseSHA)
			iss, err := tr.GetIssue(ctx, issue)
			if err != nil {
				return err
			}
			unit, err := contract.ParseWorkUnitFromIssue(iss.Number, iss.Title, iss.Body)
			if err != nil {
				return err
			}
			if err := lease.VerifyApproval(state, unit); err != nil {
				return fmt.Errorf("submission gate: %w", err)
			}
			if baseBranch == "" {
				baseBranch, err = defaultBaseBranch(root)
				if err != nil {
					return err
				}
			}
			if err := exec.CommandContext(ctx, "git", "check-ref-format", "--branch", baseBranch).Run(); err != nil {
				return fmt.Errorf("invalid PR base branch %q", baseBranch)
			}
			if state.Lease.Status == "completed" {
				if state.Lease.Owner != owner || state.Lease.ClaimID != claim || state.PRNumber == 0 {
					return fmt.Errorf("completed task has no submission for this claim")
				}
				if err := requireCleanHead(ctx, worktree, state.HeadSHA); err != nil {
					return err
				}
				if !rootOpts.Fork {
					if err := projectSubmitted(ctx, tr, issue); err != nil {
						return err
					}
				}
				if asJSON {
					return writeJSON(cmd, map[string]any{"kind": "submission", "status": "submitted", "issue_number": issue, "pr_number": state.PRNumber, "pr_url": state.PRURL, "head_sha": state.HeadSHA})
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Draft PR #%d already submitted for issue #%d (%s)\n", state.PRNumber, issue, state.PRURL)
				return nil
			}
			if err := manager.CheckClaim(ctx, issue, owner); err != nil {
				return err
			}
			cfg, err := harness.LoadConfigAt(root, state.BaseSHA)
			if err != nil {
				return err
			}
			duration, err := time.ParseDuration(cfg.Lease.Duration)
			if err != nil {
				return err
			}
			ctx, stop := maintainClaim(ctx, manager, issue, owner, duration)
			defer stop()
			branch := state.Branch
			if branch == "" {
				if rootOpts.Fork {
					branch = workspace.ForkBranchName(issue, state.Lease.ClaimID)
				} else {
					branch = workspace.BranchName(issue, iss.Title)
				}
			}
			head, err := workspace.Checkpoint(ctx, worktree, branch)
			if err != nil {
				return err
			}
			report, err := validation.Validate(ctx, worktree, unit, validation.ValidateOptions{BaseSHA: state.BaseSHA, ForkMode: rootOpts.Fork})
			if err != nil {
				return fmt.Errorf("trusted validation failed: %w", err)
			}
			if err := requireCleanHead(ctx, worktree, head); err != nil {
				return fmt.Errorf("validation changed the candidate; review and rerun submission: %w", err)
			}
			if len(state.Attestations) > 0 {
				for _, att := range state.Attestations {
					if att.Role == "tester" {
						if report.Artifact.Metadata == nil {
							report.Artifact.Metadata = make(map[string]string)
						}
						report.Artifact.Metadata["role"] = "tester"
						report.Artifact.Metadata["agent"] = att.Agent
						report.Artifact.Metadata["harness"] = att.Harness
						report.Artifact.Metadata["harness_version"] = att.HarnessVersion
						report.Artifact.Metadata["model"] = att.Model
						report.Artifact.Metadata["sandbox"] = att.Sandbox
						break
					}
				}
			}
			if _, err := manager.UpdateArtifact(ctx, issue, owner, report.Artifact); err != nil {
				return err
			}
			// GitHub publishes this head and the fenced state in one atomic Git push.
			if rootOpts.Fork {
				if err := workspace.PushForkCheckpoint(ctx, root, rootOpts.effectiveForkRemote(), branch, head); err != nil {
					return err
				}
			}
			state, err = manager.RecordCheckpoint(ctx, issue, owner, branch, head)
			if err != nil {
				return err
			}
			if err := manager.CheckClaim(ctx, issue, owner); err != nil {
				return err
			}
			prTitle := fmt.Sprintf("%s (#%d)", iss.Title, issue)
			prBody := formatDraftPRBody(unit, state)
			prHead := branch
			if rootOpts.Fork {
				forkOwner := owner
				if parts := strings.Split(strings.TrimSpace(state.ForkRepo), "/"); len(parts) == 2 && parts[0] != "" {
					forkOwner = parts[0]
				}
				prHead = forkOwner + ":" + branch
			}
			pr, err := tr.FindDraftPR(ctx, prHead, baseBranch)
			if err != nil {
				return err
			}
			if pr == nil {
				pr, err = tr.CreateDraftPR(ctx, tracker.DraftPRRequest{IssueNumber: issue, Title: prTitle, Body: prBody, Head: prHead, Base: baseBranch})
				if err != nil {
					return err
				}
			} else {
				if updatedPR, updateErr := tr.UpdateDraftPR(ctx, pr.Number, prTitle, prBody); updateErr == nil && updatedPR != nil {
					pr = updatedPR
				}
			}
			submittedState, err := manager.RecordSubmission(ctx, issue, owner, pr.Number, pr.HTMLURL)
			if err != nil {
				return err
			}
			if submittedState != nil && strings.TrimSpace(submittedState.PRURL) != "" {
				pr.HTMLURL = submittedState.PRURL
			}
			if asJSON {
				return writeJSON(cmd, map[string]any{"kind": "submission", "status": "submitted", "issue_number": issue, "pr_number": pr.Number, "pr_url": pr.HTMLURL, "head_sha": head})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Submitted draft PR #%d for issue #%d on %s (%s)\n", pr.Number, issue, branch, pr.HTMLURL)
			return nil
		},
	}
	cmd.Flags().StringVar(&baseBranch, "base", "", "PR base branch (repository default unless explicitly selected)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit the durable submission identity")
	return cmd
}

func maintainClaim(parent context.Context, manager *lease.Manager, issue int, owner string, duration time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		ticker := time.NewTicker(max(duration/3, time.Millisecond))
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := manager.Renew(ctx, issue, owner, duration); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, cancel
}

func requireCleanHead(ctx context.Context, root, expected string) error {
	head, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(head)) != expected {
		return fmt.Errorf("candidate HEAD changed from checkpoint %s", expected)
	}
	status, err := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain").Output()
	if err != nil {
		return err
	}
	if len(status) != 0 {
		return fmt.Errorf("candidate has uncheckpointed changes")
	}
	return nil
}

func projectSubmitted(ctx context.Context, tr tracker.Tracker, issue int) error {
	for _, label := range []string{"swarm:in-progress", "swarm:claimed"} {
		if err := tr.RemoveLabel(ctx, issue, label); err != nil {
			return err
		}
	}
	return tr.AddLabels(ctx, issue, []string{"swarm:pr-open"})
}

func defaultBaseBranch(root string) (string, error) {
	if hasGitRemote(root) {
		if _, err := workspace.ResolveBase(root); err != nil {
			return "", err
		}
		out, err := exec.Command("git", "-C", root, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD").Output()
		if err != nil {
			return "", fmt.Errorf("discover remote default branch: %w", err)
		}
		return strings.TrimPrefix(strings.TrimSpace(string(out)), "origin/"), nil
	}
	out, err := exec.Command("git", "-C", root, "branch", "--show-current").Output()
	if err != nil {
		return "", err
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" {
		return "", fmt.Errorf("local repository needs an explicit --base on detached HEAD")
	}
	return branch, nil
}

func formatDraftPRBody(unit *contract.WorkUnit, state *lease.IssueState) string {
	var body strings.Builder
	fmt.Fprintf(&body, "Closes #%d\n\n## Objective\n\n%s\n", unit.IssueNumber, strings.TrimSpace(unit.Objective))
	if len(unit.Constraints) > 0 {
		body.WriteString("\n## Constraints\n\n")
		for _, c := range unit.Constraints {
			fmt.Fprintf(&body, "- %s\n", c)
		}
	}
	body.WriteString("\n## Acceptance criteria\n\n")
	for _, criterion := range unit.Acceptance {
		fmt.Fprintf(&body, "- [ ] %s\n", criterion)
	}
	if state != nil {
		prov := lease.BuildProvenanceAttestation(unit, state)
		body.WriteString("\n## 🔒 Harness & Model Provenance Attestation\n\n")
		if len(prov.Roles) > 0 {
			body.WriteString("| Role | Configured Agent | Harness | Version | Model | Boundary | Artifact SHA-256 |\n")
			body.WriteString("| :--- | :--- | :--- | :--- | :--- | :--- | :--- |\n")
			for _, r := range prov.Roles {
				digest := "-"
				if len(r.ArtifactDigest) > 12 {
					digest = "`" + r.ArtifactDigest[:12] + "`"
				} else if r.ArtifactDigest != "" {
					digest = "`" + r.ArtifactDigest + "`"
				}
				fmt.Fprintf(&body, "| `%s` | `%s` | `%s` | `%s` | `%s` | `%s` | %s |\n",
					r.Role, r.Agent, r.Harness, r.HarnessVersion, r.Model, r.Sandbox, digest)
			}
			body.WriteString("\n")
		}
		if rawJSON, err := json.MarshalIndent(prov, "", "  "); err == nil {
			body.WriteString("<details>\n<summary>Machine-Readable Provenance Attestation (JSON)</summary>\n\n```json\n")
			body.Write(rawJSON)
			body.WriteString("\n```\n\n</details>\n")
		}
		if len(state.Artifacts) > 0 {
			body.WriteString("\n## 📦 Attached SwarmOS Artifacts\n\n")
			body.WriteString("| Type | Producer | Verdict | Summary |\n| --- | --- | --- | --- |\n")
			escape := strings.NewReplacer("|", "\\|", "\n", " ", "\r", " ", "`", "'")
			for _, artifact := range state.Artifacts {
				fmt.Fprintf(&body, "| %s | %s | %s | %s |\n", escape.Replace(string(artifact.Type)), escape.Replace(artifact.Producer), escape.Replace(artifact.Verdict), escape.Replace(artifact.Summary))
			}
			body.WriteString("\n")
			for _, a := range state.Artifacts {
				digest := a.Metadata["content_sha256"]
				if len(digest) > 12 {
					digest = digest[:12]
				}
				var title string
				switch a.Type {
				case contract.ArtifactDesign:
					title = fmt.Sprintf("📋 Implementation Plan (PLAN.md — sha256:%s)", digest)
				case contract.ArtifactTestPlan:
					title = fmt.Sprintf("🧪 Test Plan (TEST_PLAN.md — sha256:%s)", digest)
				case contract.ArtifactReview:
					title = fmt.Sprintf("🔍 Plan Review (REVIEW.md — Verdict: %s — sha256:%s)", a.Verdict, digest)
				case contract.ArtifactImplementation:
					title = fmt.Sprintf("🛠️ Implementation Summary (sha256:%s)", digest)
				case contract.ArtifactValidation:
					title = fmt.Sprintf("✅ Validation Checks (%s — sha256:%s)", a.Verdict, digest)
				default:
					title = fmt.Sprintf("📄 Artifact (%s — sha256:%s)", a.Type, digest)
				}
				fmt.Fprintf(&body, "<details>\n<summary>%s</summary>\n\n%s\n\n</details>\n\n", title, lease.TruncateArtifactBody(a.Body, unit.IssueNumber))
			}
		}
	}
	return body.String()
}
