package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/pr"
	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workspace"
)

func newStageCmd(rootOpts *RootOptions) *cobra.Command { return newStageCommand(rootOpts, false) }

func newPRStageCmd(rootOpts *RootOptions) *cobra.Command { return newStageCommand(rootOpts, true) }

func newStageCommand(rootOpts *RootOptions, prStage bool) *cobra.Command {
	var phase string
	var harnessFlag string
	var modelFlag string
	var handoff bool
	var asJSON bool

	use, short := "stage [<issue-number>]", "Stage interactive or harness-produced plan/implementation artifacts onto the active lease"
	argsValidator := cobra.MaximumNArgs(1)
	if prStage {
		use, short = "stage <pr-number>", "Stage an interactive plan or unpublished implementation in the managed PR worktree"
		argsValidator = cobra.ExactArgs(1)
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  argsValidator,
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			defer func() { reportJSONFailure(cmd, asJSON, "stage", runErr) }()
			if phase == "" {
				phase = "plan"
			}
			if phase != "plan" && phase != "implement" {
				return fmt.Errorf("invalid --phase %q (must be plan or implement)", phase)
			}
			if handoff && phase != "plan" {
				return fmt.Errorf("--handoff applies only to --phase plan")
			}
			root := rootOpts.resolveRepoRoot()
			issue, worktree, prNumber := 0, "", 0
			var prStatus *pr.Result
			var err error
			if prStage {
				prNumber, err = strconv.Atoi(strings.TrimPrefix(args[0], "#"))
				if err != nil || prNumber <= 0 {
					return fmt.Errorf("invalid PR number %q", args[0])
				}
				if rootOpts.Fork {
					return fmt.Errorf("%s requires upstream write access; contributor-only --fork coordination does not support PR revisions", cmd.CommandPath())
				}
			} else {
				issue, worktree, err = resolveIssueAndWorktree(root, args)
				if err != nil {
					return err
				}
			}
			tr, manager, _, err := rootOpts.resolveLeaseManager(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if prStage {
				if tr.BackendName() != "local" {
					if err := tr.Authorize(ctx, "approve-task"); err != nil {
						return fmt.Errorf("upstream write authorization required: %w", err)
					}
				}
				prStatus, err = (&pr.Service{Root: root, Tracker: tr, Manager: manager}).Status(ctx, prNumber)
				if err != nil {
					return err
				}
				issue, worktree = prStatus.IssueNumber, prStatus.Worktree
				if prStatus.State.PRRevision == nil || prStatus.Status == "diverged" || prStatus.Status == "published-pending-reconciliation" {
					return fmt.Errorf("PR #%d requires an existing pending unpublished revision; run swarm pr work %d or reconcile with swarm pr submit %d", prNumber, prNumber, prNumber)
				}
				if worktree == "" || prStatus.Rebasing {
					return fmt.Errorf("PR #%d requires an existing managed worktree that is not rebasing", prNumber)
				}
			}
			lock, err := lease.LockClaim(root, issue)
			if err != nil {
				return err
			}
			defer lease.UnlockClaim(lock)

			if prStage {
				fresh, e := (&pr.Service{Root: root, Tracker: tr, Manager: manager}).Status(ctx, prNumber)
				if e != nil {
					return e
				}
				if fresh.Status == "diverged" || fresh.Status == "published-pending-reconciliation" || fresh.Rebasing || fresh.Worktree != worktree || fresh.State.PRRevision == nil || fresh.State.PRRevision.LocalHeadSHA != prStatus.State.PRRevision.LocalHeadSHA {
					return fmt.Errorf("PR #%d revision changed or is rebasing; refusing to stage", prNumber)
				}
				prStatus = fresh
			}
			owner, err := tr.CurrentUser(ctx)
			if err != nil || strings.TrimSpace(owner) == "" {
				return fmt.Errorf("staging requires an authenticated owner")
			}
			claim, err := lease.LoadClaimToken(root, issue)
			if err != nil {
				return fmt.Errorf("load active claim token: %w", err)
			}
			manager.BindClaim(issue, claim)
			if err := manager.CheckClaim(ctx, issue, owner); err != nil {
				return err
			}
			state, err := manager.GetCurrentState(ctx, issue)
			if err != nil {
				return err
			}
			if prStage {
				rev := state.PRRevision
				if state.PRNumber != prNumber || rev == nil || rev.Number != prNumber || rev.RemoteHeadSHA != prStatus.PR.HeadSHA || rev.Branch != workspace.PRBranchName(prNumber) || rev.LocalHeadSHA != prStatus.State.PRRevision.LocalHeadSHA {
					return fmt.Errorf("PR #%d pending revision changed; refusing to stage", prNumber)
				}
				verified, err := workspace.VerifyPRWorktree(ctx, root, prNumber, rev.LocalHeadSHA)
				if err != nil {
					return err
				}
				resolved, err := filepath.Abs(worktree)
				if err != nil || filepath.Clean(resolved) != verified {
					return fmt.Errorf("PR #%d worktree differs from its managed checkout", prNumber)
				}
				worktree = verified
			} else if err := requireIssueWorkspace(state); err != nil {
				return err
			}
			iss, err := tr.GetIssue(ctx, issue)
			if err != nil {
				return err
			}
			unit, err := contract.ParseWorkUnitFromIssue(iss.Number, iss.Title, iss.Body)
			if err != nil {
				return err
			}
			_ = workspace.FetchCommit(root, "origin", state.BaseSHA)
			cfg, err := harness.LoadConfigAt(root, state.BaseSHA)
			if err != nil {
				return err
			}
			duration, err := time.ParseDuration(cfg.Lease.Duration)
			if err != nil {
				return err
			}
			heartbeat := manager.StartHeartbeat(ctx, issue, owner, duration)
			defer heartbeat.Stop()
			ctx = heartbeat.Context()

			harnessName := strings.TrimSpace(firstNonEmptyStr(harnessFlag, "interactive"))
			modelName := strings.TrimSpace(firstNonEmptyStr(modelFlag, "interactive"))
			metaForRole := func(role string) map[string]string {
				return map[string]string{
					"role":            role,
					"agent":           harnessName,
					"harness":         harnessName,
					"harness_version": "interactive",
					"model":           modelName,
					"sandbox":         "host",
				}
			}

			if phase == "plan" {
				if strings.EqualFold(cfg.PlanMode, "agent") {
					return fmt.Errorf("repository policy sets plan_mode: agent; interactive/manual plan staging is disabled")
				}
				planBytes, err := os.ReadFile(filepath.Join(worktree, ".swarm", "active", "PLAN.md"))
				if err != nil || strings.TrimSpace(string(planBytes)) == "" {
					return fmt.Errorf("missing or empty .swarm/active/PLAN.md in %s", worktree)
				}
				testBytes, err := os.ReadFile(filepath.Join(worktree, ".swarm", "active", "TEST_PLAN.md"))
				if err != nil || strings.TrimSpace(string(testBytes)) == "" {
					return fmt.Errorf("missing or empty .swarm/active/TEST_PLAN.md in %s", worktree)
				}
				if _, err := manager.UpdatePhase(ctx, issue, owner, "plan", "active"); err != nil {
					return err
				}
				if _, err := manager.UpdateArtifact(ctx, issue, owner, contract.Artifact{
					Type:     contract.ArtifactDesign,
					Producer: harnessName + ":planner",
					Summary:  fmt.Sprintf("Implementation plan for #%d", issue),
					Body:     string(planBytes),
					Metadata: metaForRole("planner"),
				}); err != nil {
					return err
				}
				state, err = manager.UpdateArtifact(ctx, issue, owner, contract.Artifact{
					Type:     contract.ArtifactTestPlan,
					Producer: harnessName + ":planner",
					Summary:  fmt.Sprintf("Test plan covering %d acceptance criteria", len(unit.Acceptance)),
					Body:     string(testBytes),
					Metadata: metaForRole("planner"),
				})
				if err != nil {
					return err
				}
				// Plan-review label projection is handled by LeaseManager.UpdateArtifact.
				nextPhase := ""
				if handoff {
					nextPhase = "implement"
					if state.EffectiveGate != contract.GateNone {
						nextPhase = "review"
					}
					state, err = manager.Handoff(ctx, issue, owner, "plan", nextPhase)
					if err != nil {
						return err
					}
				}
				if asJSON {
					outcome := map[string]any{
						"kind":         "stage",
						"phase":        "plan",
						"issue_number": issue,
						"harness":      harnessName,
						"model":        modelName,
						"handed_off":   nextPhase,
						"state":        state,
					}
					if prStage {
						outcome["pr_number"] = prNumber
					}
					return writeJSON(cmd, outcome)
				}
				if prStage {
					fmt.Fprintf(cmd.OutOrStdout(), "Staged plan and test-plan artifacts for PR #%d (issue #%d) [harness=%s model=%s gate=%s]\n", prNumber, issue, harnessName, modelName, state.EffectiveGate)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "Staged plan and test-plan artifacts for issue #%d [harness=%s model=%s gate=%s]\n", issue, harnessName, modelName, state.EffectiveGate)
				}
				return nil
			}

			// phase == "implement"
			if err := lease.VerifyApproval(state, unit); err != nil {
				return fmt.Errorf("cannot stage implementation before plan approval: %w", err)
			}
			if _, err := manager.UpdatePhase(ctx, issue, owner, "implement", "active"); err != nil {
				return err
			}
			branch := state.Branch
			if prStage {
				branch = state.PRRevision.Branch
			}
			if branch == "" {
				if rootOpts.Fork {
					branch = workspace.ForkBranchName(issue, state.Lease.ClaimID)
				} else {
					branch = workspace.BranchName(issue, iss.Title)
				}
			}
			summary, implBody := stageImplementationSummary(worktree, issue)
			head, err := workspace.Checkpoint(ctx, worktree, branch, workspace.CheckpointOptions{
				WorkUnit: unit, Purpose: "implement", Detail: cfg.CommitMessageDetail(),
			})
			if err != nil {
				return err
			}
			if prStage {
				state, err = manager.RecordPRHead(ctx, issue, owner, prNumber, branch, head)
				if err != nil {
					return err
				}
			}
			if _, err := manager.UpdateArtifact(ctx, issue, owner, contract.Artifact{
				Type:     contract.ArtifactImplementation,
				Producer: harnessName + ":developer",
				Verdict:  "PASS",
				Summary:  summary,
				Body:     implBody,
				Metadata: metaForRole("developer"),
			}); err != nil {
				return err
			}
			report, valErr := validation.Validate(ctx, worktree, unit, validation.ValidateOptions{BaseSHA: state.BaseSHA, ForkMode: rootOpts.Fork})
			if report != nil {
				if report.Artifact.Metadata == nil {
					report.Artifact.Metadata = make(map[string]string)
				}
				for k, v := range metaForRole("tester") {
					report.Artifact.Metadata[k] = v
				}
				if _, err := manager.UpdateArtifact(ctx, issue, owner, report.Artifact); err != nil {
					return err
				}
			}
			if !prStage {
				if rootOpts.Fork {
					if err := workspace.PushForkCheckpoint(ctx, root, rootOpts.effectiveForkRemote(), branch, head); err != nil {
						return err
					}
				}
				state, err = manager.RecordCheckpoint(ctx, issue, owner, branch, head)
				if err != nil {
					return err
				}
			} else {
				state, err = manager.GetCurrentState(ctx, issue)
				if err != nil {
					return err
				}
			}
			if valErr != nil {
				return valErr
			}
			validationLabel := "NONE"
			if report != nil {
				validationLabel = "FAIL"
				if report.Passed {
					validationLabel = "PASS"
				}
			}
			if asJSON {
				outcome := map[string]any{
					"kind":              "stage",
					"phase":             "implement",
					"issue_number":      issue,
					"harness":           harnessName,
					"model":             modelName,
					"branch":            branch,
					"head_sha":          head,
					"validation_passed": report != nil && report.Passed,
					"state":             state,
				}
				if prStage {
					outcome["pr_number"] = prNumber
				}
				return writeJSON(cmd, outcome)
			}
			if prStage {
				fmt.Fprintf(cmd.OutOrStdout(), "Staged unpublished implementation checkpoint %s for PR #%d (issue #%d) on %s [validation=%s]\n", head, prNumber, issue, branch, validationLabel)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "Staged implementation checkpoint %s for issue #%d on %s [validation=%s]\n", head, issue, branch, validationLabel)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&phase, "phase", "plan", "Workflow phase to stage: plan or implement")
	cmd.Flags().StringVar(&harnessFlag, "harness", "", "Harness identifier for provenance attestation (e.g., claude, codex, opencode, omp)")
	cmd.Flags().StringVar(&modelFlag, "model", "", "Model identifier for provenance attestation")
	cmd.Flags().BoolVar(&handoff, "handoff", false, "With --phase plan, hand off the lease to the next workflow phase after staging")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit structured staging output as JSON")
	return cmd
}

func stageImplementationSummary(wtPath string, issueNum int) (string, string) {
	statusOut, _ := gitcredential.Output(context.Background(), wtPath, "status", "--short")
	diffStat, _ := gitcredential.Output(context.Background(), wtPath, "diff", "--stat", "HEAD")
	lines := strings.Split(strings.TrimSpace(string(statusOut)), "\n")
	count := 0
	var body strings.Builder
	fmt.Fprintf(&body, "# Implementation Summary (Issue #%d)\n\n## Working Tree Changes\n```text\n", issueNum)
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if strings.Contains(l, ".swarm/active") || strings.Contains(l, "AGENTS.md") || strings.Contains(l, "CLAUDE.md") {
			continue
		}
		count++
		body.WriteString(l + "\n")
	}
	if count == 0 {
		body.WriteString("(clean working tree)\n")
	}
	body.WriteString("```\n")
	if len(strings.TrimSpace(string(diffStat))) > 0 {
		body.WriteString("\n## Diff Stat\n```text\n" + strings.TrimSpace(string(diffStat)) + "\n```\n")
	}
	return fmt.Sprintf("Implemented changes across %d file(s)", count), body.String()
}
