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
	"github.com/supernovae/swarmos/internal/progress"
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
			p := rootOpts.operationProgress(cmd.Context(), cmd.ErrOrStderr(), showProgress)
			cmd.SetContext(progress.WithActivity(cmd.Context(), p.Activity()))
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
			stateRef := manager.LedgerLayout().StateRef(issue)
			prSection := draftPRSection(unit, state, stateRef, manager.UpstreamRemote())
			retryCommand := strings.Split(submissionRecoveryCommand(0, rootOpts), " pr rebase ")[0] + fmt.Sprintf(" issue submit %d", issue)
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
			outside := ""
			if pr != nil {
				outside = pr.Body
			}
			number := 0
			if pr != nil {
				number = pr.Number
			}
			prBody, err := lease.ComposePRDescription(outside, "issue-description", prSection+lease.DescriptionText(freshness.conflictNote(number, issue, rootOpts)), stateRef, issue)
			if err != nil {
				return err
			}
			if pr == nil {
				pr, err = tr.CreateDraftPR(ctx, tracker.DraftPRRequest{IssueNumber: issue, Title: prTitle, Body: prBody, Head: prHead, Base: baseBranch})
			} else {
				pr, err = tr.UpdateDraftPR(ctx, pr.Number, prTitle, prBody)
			}
			if err != nil || pr == nil {
				if err == nil {
					err = fmt.Errorf("tracker returned no PR")
				}
				return fmt.Errorf("PR publication/description update failed; a PR may already exist; retry the same command: %s: %w", retryCommand, err)
			}
			if note := freshness.conflictNote(pr.Number, issue, rootOpts); note != "" {
				fmt.Fprint(advisoryOut, note)
				prBody, err = lease.ComposePRDescription(prBody, "issue-description", prSection+lease.DescriptionText(note), stateRef, issue)
				if err != nil {
					return err
				}
				updated, updateErr := tr.UpdateDraftPR(ctx, pr.Number, prTitle, prBody)
				if updateErr != nil || updated == nil {
					if updateErr == nil {
						updateErr = fmt.Errorf("tracker returned no PR")
					}
					return fmt.Errorf("PR #%d was published but description update failed; retry the same command: %s: %w", pr.Number, retryCommand, updateErr)
				}
				pr = updated
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

func draftPRSection(unit *contract.WorkUnit, state *lease.IssueState, stateRef string, remote ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Closes #%d\n\n## Objective\n\n", unit.IssueNumber)
	b.WriteString(lease.DescriptionSlot(unit.Objective))
	b.WriteString("\n## Constraints\n\n")
	for _, c := range unit.Constraints {
		b.WriteString(lease.DescriptionSlot("- " + c + "\n"))
	}
	b.WriteString("\n## Acceptance criteria\n\nThese checkboxes track final acceptance review by a maintainer. SwarmOS leaves them unchecked at submission.\n")
	for _, c := range unit.Acceptance {
		b.WriteString("- [ ] " + lease.DescriptionSlot(c+"\n"))
	}
	b.WriteString("\n## Review handoff\n\nUse the test plan and validation results to verify each criterion, then mark this PR ready for review in GitHub.\n")
	if state != nil {
		b.WriteString(lease.DescriptionProductionSummary(state))
		raw, _ := json.MarshalIndent(lease.BuildProvenanceAttestation(unit, state), "", "  ")
		b.WriteString("\n## 🔒 Harness & Model Provenance Attestation\n\nMachine-Readable Provenance Attestation (JSON)\n```json\n" + lease.DescriptionText(string(raw)) + "\n```\n")
		if runs := lease.RoleRuns(state); len(runs) > 0 {
			raw, _ := json.MarshalIndent(runs, "", "  ")
			b.WriteString(lease.DescriptionAtomicSlot("\n## ⏱️ Role Run Telemetry\n\n" + lease.RoleRunTable(runs) + "\n```json\n" + string(raw) + "\n```\n"))
		}
	}
	b.WriteString("\n## 📦 Attached SwarmOS Artifacts\n\n")
	b.WriteString(lease.DescriptionArtifacts(state))
	b.WriteString(lease.DescriptionRetrieval(unit.IssueNumber, stateRef, remote...))
	return b.String()
}
func formatDraftPRBody(unit *contract.WorkUnit, state *lease.IssueState, stateRef string) string {
	body, _ := lease.ComposePRDescription("", "issue-description", draftPRSection(unit, state, stateRef), stateRef, unit.IssueNumber)
	return body
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
