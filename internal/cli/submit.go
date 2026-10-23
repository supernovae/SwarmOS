package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workspace"
	"os/exec"
	"strings"
)

func newSubmitCmd(rootOpts *RootOptions) *cobra.Command {
	var baseBranch string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "submit [<issue-number>]",
		Short: "Validate the exact checkpoint and idempotently open its draft PR",
		Long: `Validate the exact checkpoint and idempotently open its linked draft PR.

The issue remains the approved work contract; the PR is its proposed implementation.
Submission completes the SwarmOS execution attempt, not final acceptance review.
The PR includes the plan, test plan, and validation evidence. A maintainer checks
each acceptance criterion against the implementation and observed results,
records the decision in the PR, and uses GitHub to mark it ready for review.
swarm issue review-plan approves the implementation plan, not the finished PR.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			defer func() { reportJSONFailure(cmd, asJSON, "submission", runErr) }()
			root := rootOpts.resolveRepoRoot()
			issue, worktree, err := resolveIssueAndWorktree(root, args)
			if err != nil {
				return err
			}
			lock, err := lease.LockClaim(root, issue)
			if err != nil {
				return err
			}
			defer lease.UnlockClaim(lock)
			tr, manager, _, err := rootOpts.resolveLeaseManager(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			owner, claim, err := bindPersistedClaim(ctx, root, tr, manager, issue)
			if err != nil {
				return err
			}
			state, err := manager.GetCurrentState(ctx, issue)
			if err != nil {
				return err
			}
			if err := requireIssueWorkspace(state); err != nil {
				return err
			}
			iss, unit, err := loadWorkUnit(ctx, tr, issue)
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
			cfg, duration, err := approvedLeasePolicy(root, state.BaseSHA)
			if err != nil {
				return err
			}
			heartbeat := manager.StartHeartbeat(ctx, issue, owner, duration)
			defer heartbeat.Stop()
			ctx = heartbeat.Context()
			branch := state.Branch
			if branch == "" {
				if rootOpts.Fork {
					branch = workspace.ForkBranchName(issue, state.Lease.ClaimID)
				} else {
					branch = workspace.BranchName(issue, iss.Title)
				}
			}
			head, err := workspace.Checkpoint(ctx, worktree, branch, workspace.CheckpointOptions{
				WorkUnit: unit, Purpose: "submit", Detail: cfg.CommitMessageDetail(),
			})
			if err != nil {
				return err
			}
			report, valErr := validation.Validate(ctx, worktree, unit, validation.ValidateOptions{BaseSHA: state.BaseSHA, ForkMode: rootOpts.Fork})
			if report != nil {
				report.Artifact.Metadata = lease.TesterMetadata(state, report.Artifact.Metadata)
				if _, err := manager.UpdateArtifact(ctx, issue, owner, report.Artifact); err != nil {
					return err
				}
			}
			if valErr != nil {
				return fmt.Errorf("trusted validation failed: %w", valErr)
			}
			if err := requireCleanHead(ctx, worktree, head); err != nil {
				return fmt.Errorf("validation changed the candidate; review and rerun submission: %w", err)
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
	cmd.Flags().StringVar(&baseBranch, "base", "", "Target branch name for the draft PR (defaults to the repository default branch)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit the durable submission identity")
	return cmd
}

func requireCleanHead(ctx context.Context, root, expected string) error {
	head, err := gitcredential.Output(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(head)) != expected {
		return fmt.Errorf("candidate HEAD changed from checkpoint %s", expected)
	}
	status, err := gitcredential.Output(ctx, root, "status", "--porcelain")
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
		out, err := gitcredential.Output(context.Background(), root, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
		if err != nil {
			return "", fmt.Errorf("discover remote default branch: %w", err)
		}
		return strings.TrimPrefix(strings.TrimSpace(string(out)), "origin/"), nil
	}
	out, err := gitcredential.Output(context.Background(), root, "branch", "--show-current")
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
	body.WriteString("These checkboxes track final acceptance review by a maintainer, not automated check results. SwarmOS leaves them unchecked at submission; this does not mean the checks failed.\n\n")
	for _, criterion := range unit.Acceptance {
		fmt.Fprintf(&body, "- [ ] %s\n", criterion)
	}
	body.WriteString("\n## Review handoff\n\n")
	body.WriteString("1. Use the attached test plan to map each criterion to its verification, then inspect the implementation and validation results. Record supporting test results or manual observations in the PR and check off each verified criterion above; leave unverified criteria open.\n")
	body.WriteString("2. Mark this PR ready for review in GitHub when the evidence is ready, and request a code review. `swarm issue review-plan` reviews the plan before implementation; it does not approve this PR.\n")
	body.WriteString("3. Merge through the repository's normal review and CI requirements. The closing reference above closes the linked issue when this PR merges into the default branch; issue and PR checkboxes are not synchronized.\n")
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
