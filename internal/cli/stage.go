package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workspace"
)

func newStageCmd(rootOpts *RootOptions) *cobra.Command {
	var phase string
	var harnessFlag string
	var modelFlag string
	var handoff bool
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "stage [<issue-number>]",
		Short: "Stage interactive or harness-produced plan/implementation artifacts onto the active lease",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if phase == "" {
				phase = "plan"
			}
			if phase != "plan" && phase != "implement" {
				return fmt.Errorf("invalid --phase %q (must be plan or implement)", phase)
			}
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
			if err != nil || strings.TrimSpace(owner) == "" {
				return fmt.Errorf("staging requires an authenticated owner")
			}
			claim, err := loadClaimToken(root, issue)
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
			iss, err := tr.GetIssue(ctx, issue)
			if err != nil {
				return err
			}
			unit, err := contract.ParseWorkUnitFromIssue(iss.Number, iss.Title, iss.Body)
			if err != nil {
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
				if state.EffectiveGate != contract.GateNone {
					_ = tr.AddLabels(ctx, issue, []string{"swarm:plan-review"})
				}
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
					return writeJSON(cmd, map[string]any{
						"kind":         "stage",
						"phase":        "plan",
						"issue_number": issue,
						"harness":      harnessName,
						"model":        modelName,
						"handed_off":   nextPhase,
						"state":        state,
					})
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Staged plan and test-plan artifacts for issue #%d [harness=%s model=%s gate=%s]\n", issue, harnessName, modelName, state.EffectiveGate)
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
			if branch == "" {
				branch = workspace.BranchName(issue, iss.Title)
			}
			summary, implBody := stageImplementationSummary(worktree, issue)
			head, err := workspace.Checkpoint(ctx, worktree, branch)
			if err != nil {
				return err
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
			report, valErr := validation.Validate(ctx, worktree, unit, validation.ValidateOptions{BaseSHA: state.BaseSHA})
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
			state, err = manager.RecordCheckpoint(ctx, issue, owner, branch, head)
			if err != nil {
				return err
			}
			if valErr != nil {
				return valErr
			}
			if asJSON {
				return writeJSON(cmd, map[string]any{
					"kind":              "stage",
					"phase":             "implement",
					"issue_number":      issue,
					"harness":           harnessName,
					"model":             modelName,
					"branch":            branch,
					"head_sha":          head,
					"validation_passed": report != nil && report.Passed,
					"state":             state,
				})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Staged implementation checkpoint %s for issue #%d on %s [validation=PASS]\n", head, issue, branch)
			return nil
		},
	}

	cmd.Flags().StringVar(&phase, "phase", "plan", "Workflow phase to stage: plan or implement")
	cmd.Flags().StringVar(&harnessFlag, "harness", "", "Harness identifier for provenance attestation (e.g., claude, codex, opencode, omp)")
	cmd.Flags().StringVar(&modelFlag, "model", "", "Model identifier for provenance attestation")
	cmd.Flags().BoolVar(&handoff, "handoff", false, "Hand off the lease to the next workflow phase after staging plan artifacts")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit structured staging output as JSON")
	return cmd
}

func stageImplementationSummary(wtPath string, issueNum int) (string, string) {
	statusOut, _ := exec.Command("git", "-C", wtPath, "status", "--short").Output()
	diffStat, _ := exec.Command("git", "-C", wtPath, "diff", "--stat", "HEAD").Output()
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
