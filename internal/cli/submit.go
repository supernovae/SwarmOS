package cli

import (
	"context"
	"encoding/json"
	"errors"
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
	var showProgress bool
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
			p := operationProgress(cmd.Context(), cmd.ErrOrStderr(), showProgress)
			defer func() {
				status := "completed"
				if runErr != nil {
					status = "failed"
				}
				p.Finish(status)
			}()
			var failedReport *validation.ValidationReport
			defer func() {
				if runErr != nil && failedReport != nil {
					if asJSON {
						outcome := map[string]any{"kind": "submission", "status": "failed", "error": runErr.Error(), "report": failedReport}
						if action := nextActionOf(runErr); action != nil {
							outcome["next_action"] = action
						}
						_ = writeJSON(cmd, outcome)
					} else {
						writeValidationChecks(cmd, failedReport)
					}
				} else {
					reportJSONFailure(cmd, asJSON, "submission", runErr)
				}
			}()
			stops := stopContext{opts: rootOpts, resource: "issue"}
			defer func() { runErr = stops.stop(runErr) }()
			root := rootOpts.resolveRepoRoot()
			issue, worktree, err := resolveIssueAndWorktree(root, rootOpts.issueArgs(args))
			if err != nil {
				return err
			}
			stops.number = issue
			lock, err := lease.LockClaim(root, issue)
			if err != nil {
				return err
			}
			defer lease.UnlockClaim(lock)
			if err := refuseEmptySubmitEarly(cmd.Context(), root, worktree, issue); err != nil {
				return err
			}
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
				baseBranch, err = defaultBaseBranch(root, manager.UpstreamRemote())
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
			cfg, duration, err := approvedLeasePolicy(root, manager.UpstreamRemote(), state.BaseSHA)
			if err != nil {
				return err
			}
			heartbeat := manager.StartHeartbeat(ctx, issue, owner, duration)
			defer heartbeat.Stop()
			ctx = heartbeat.Context()
			_, branch := workspace.ClaimCheckpointFrom(manager.UpstreamRemote(), rootOpts.Fork, rootOpts.effectiveForkRemote(), issue, iss.Title, state.Lease.ClaimID, state.Branch)
			p.Next("checkpoint")
			head, err := workspace.Checkpoint(ctx, worktree, branch, workspace.CheckpointOptions{
				WorkUnit: unit, Purpose: "submit", Detail: cfg.CommitMessageDetail(),
			})
			if err != nil {
				return err
			}
			if head == state.BaseSHA {
				return emptySubmitError(issue, state.BaseSHA)
			}
			p.Next("validation")
			validationOpts := validation.ValidateOptions{Version: Version, ClaimID: state.Lease.ClaimID, OnProgress: p.Observe, BaseSHA: state.BaseSHA, ForkMode: rootOpts.Fork || manager.HasWorkerGateway(), LookupEnv: rootOpts.lookupEnv, DependencyPolicy: rootOpts.dependencyPolicy(worktree)}
			report, valErr := validation.Reuse(ctx, worktree, validationOpts, owner, state.Artifacts)
			if valErr == nil && report == nil {
				report, valErr = validation.Validate(ctx, worktree, unit, validationOpts)
			}
			writeValidationSkipNotice(cmd, report)
			if report != nil && report.Reused {
				fmt.Fprintln(cmd.ErrOrStderr(), report.Artifact.Summary)
			}
			if report != nil {
				failedReport = report
				report.Artifact.Metadata = lease.TesterMetadata(state, report.Artifact.Metadata)
				lease.PreserveTesterGuidance(state, &report.Artifact)
				if _, err := manager.RecordTrustedValidation(ctx, issue, owner, report.Artifact); err != nil {
					return errors.Join(valErr, err)
				}
			}
			if valErr != nil {
				return fmt.Errorf("trusted validation failed: %w", valErr)
			}
			if err := requireCleanHead(ctx, worktree, head); err != nil {
				return fmt.Errorf("validation changed the candidate; review and rerun submission: %w", err)
			}
			freshness := checkSubmissionFreshnessFrom(ctx, root, manager.UpstreamRemote(), baseBranch, state.BaseSHA, head)
			advisoryOut := cmd.OutOrStdout()
			if asJSON {
				advisoryOut = cmd.ErrOrStderr()
			}
			fmt.Fprint(advisoryOut, freshness.advisory())
			var policyWarning *workspace.HarnessPolicyWarning
			if unit.AllowHarnessChanges && freshness.TargetSHA != "" {
				policyWarning, _ = workspace.ObserveHarnessPolicy(ctx, root, state.BaseSHA, state.PolicyTreeDigest, freshness.TargetSHA)
				fmt.Fprint(advisoryOut, prepareHarnessPolicyWarning(policyWarning, issue, state.PRNumber, rootOpts))
			}
			p.Next("checkpoint push/record")
			// GitHub publishes this head and the fenced state in one atomic Git push.
			if rootOpts.Fork {
				if err := workspace.PushForkCheckpointFrom(ctx, root, manager.UpstreamRemote(), rootOpts.effectiveForkRemote(), branch, head); err != nil {
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
			prBody := formatDraftPRBody(unit, state, manager.LedgerLayout().StateRef(issue))
			prHead := branch
			if rootOpts.Fork {
				forkOwner := owner
				if parts := strings.Split(strings.TrimSpace(state.ForkRepo), "/"); len(parts) == 2 && parts[0] != "" {
					forkOwner = parts[0]
				}
				prHead = forkOwner + ":" + branch
			}
			p.Next("draft PR create/update")
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
				if freshness.Status != "conflicts" {
					if updatedPR, updateErr := tr.UpdateDraftPR(ctx, pr.Number, prTitle, prBody); updateErr == nil && updatedPR != nil {
						pr = updatedPR
					}
				}
			}
			if note := freshness.conflictNote(pr.Number, issue, rootOpts); note != "" {
				fmt.Fprint(advisoryOut, note)
				if updated, updateErr := tr.UpdateDraftPR(ctx, pr.Number, prTitle, prBody+note); updateErr != nil || updated == nil {
					fmt.Fprintf(advisoryOut, "PR description lacks the freshness note; run `%s`.\n", submissionRecoveryCommand(pr.Number, rootOpts))
				} else {
					pr = updated
				}
			}
			p.Next("submission record")
			submittedState, err := manager.RecordSubmission(ctx, issue, owner, pr.Number, pr.HTMLURL)
			if err != nil {
				return err
			}
			if submittedState != nil && strings.TrimSpace(submittedState.PRURL) != "" {
				pr.HTMLURL = submittedState.PRURL
			}
			if asJSON {
				outcome := map[string]any{"kind": "submission", "status": "submitted", "issue_number": issue, "pr_number": pr.Number, "pr_url": pr.HTMLURL, "head_sha": head, "base_freshness": freshness, "report": report}
				if policyWarning != nil {
					prepareHarnessPolicyWarning(policyWarning, issue, pr.Number, rootOpts)
					outcome["harness_policy_warning"] = policyWarning
				}
				return writeJSON(cmd, outcome)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Submitted draft PR #%d for issue #%d on %s (%s)\n", pr.Number, issue, branch, pr.HTMLURL)
			return nil
		},
	}
	cmd.Flags().StringVar(&baseBranch, "base", "", "Target branch name for the draft PR (defaults to the repository default branch)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit the durable submission identity")
	cmd.Flags().BoolVar(&showProgress, "progress", true, "Report progress on stderr")
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

func defaultBaseBranch(root, upstreamRemote string) (string, error) {
	if hasSubmissionRemote(context.Background(), root, upstreamRemote) {
		// Retain advertised-default discovery, but a failed refresh must not prevent
		// submission when the cached name still identifies its target branch.
		_, _ = workspace.ResolveBaseFrom(root, upstreamRemote)
		out, err := gitcredential.Output(context.Background(), root, "symbolic-ref", "--quiet", "--short", "refs/remotes/"+upstreamRemote+"/HEAD")
		if err != nil {
			return "", fmt.Errorf("cannot discover remote default branch; specify --base")
		}
		return strings.TrimPrefix(strings.TrimSpace(string(out)), upstreamRemote+"/"), nil
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

func formatDraftPRBody(unit *contract.WorkUnit, state *lease.IssueState, stateRef string) string {
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
		if runs := lease.RoleRuns(state); len(runs) > 0 {
			body.WriteString("\n## ⏱️ Role Run Telemetry\n\n")
			body.WriteString("Observed per-run timing and the usage each harness reported; `unavailable` means it was not observed. Descriptive only, never a gate input.\n\n")
			body.WriteString(lease.RoleRunTable(runs))
			if rawJSON, err := json.MarshalIndent(runs, "", "  "); err == nil {
				body.WriteString("\n<details>\n<summary>Machine-Readable Role Run Telemetry (JSON)</summary>\n\n```json\n")
				body.Write(rawJSON)
				body.WriteString("\n```\n\n</details>\n")
			}
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
				if a.Metadata["operator_steered"] == "true" {
					fmt.Fprintf(&body, "Operator-steered %s: %s messages; SHA-256 `%s`.\n\n", a.Type, a.Metadata["guidance_count"], a.Metadata["guidance_sha256"])
				}
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
				fmt.Fprintf(&body, "<details>\n<summary>%s</summary>\n\n%s\n\n</details>\n\n", title, lease.TruncateArtifactBody(a.Body, stateRef))
			}
		}
	}
	return body.String()
}

func emptySubmitError(issue int, base string) error {
	return fmt.Errorf("issue #%d has no commits beyond approved base %s; run swarm issue work %d --phase implement", issue, base, issue)
}
func refuseEmptySubmitEarly(ctx context.Context, root, worktree string, issue int) error {
	base, err := lease.LoadClaimBaseHint(root, issue)
	if err != nil || base == "" {
		return nil
	}
	count, err := workspace.CommitsBeyond(ctx, worktree, base)
	if err != nil || count != 0 {
		return nil
	}
	dirty, err := workspace.HasCheckpointableChanges(ctx, worktree)
	if err != nil || dirty {
		return nil
	}
	return emptySubmitError(issue, base)
}
