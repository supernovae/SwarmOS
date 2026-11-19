package workflow

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/sandbox"
	"github.com/supernovae/swarmos/internal/storageuse"
)

func (e *Engine) issueReadiness(opts WorkOptions, st *lease.IssueState, execution contract.ExecutionConfig) bool {
	return runtime.GOOS == "linux" && execution.Mode == contract.ExecutionIsolated && opts.PRNumber == 0 && !opts.ManualClaim &&
		st.PRNumber == 0 && st.PRRevision == nil && st.ForkRepo == "" && !opts.ForkMode && !e.ForkMode && !e.LeaseManager.HasWorkerGateway()
}

func (e *Engine) checkIssueReadiness(ctx context.Context, opts WorkOptions, cfg *contract.SwarmConfig, base string, deps sandbox.Dependencies, agents map[string]string, preview []RolePreview) (*sandbox.ReadinessReport, error) {
	report := &sandbox.ReadinessReport{Input: "approved base " + base, Rows: []sandbox.ReadinessRow{}}
	if !opts.InspectOnly && agents["developer"] == "" && agents["tester"] == "" {
		return report, nil
	}
	projects, err := sandbox.DiscoverReadiness(ctx, opts.RepoRoot, base)
	if err != nil {
		return report, err
	}
	for _, role := range []string{"developer", "tester"} {
		name := agents[role]
		reason := ""
		if opts.InspectOnly {
			reason = "dry-run skips readiness execution"
			for _, row := range preview {
				if row.Role == role && row.Disposition != "selected" {
					reason = row.Reason
					break
				}
			}
			if name == "" {
				name = resolveRoleAgent(cfg, role, opts)
			}
		} else if name == "" {
			continue
		}
		rows, err := agent.CheckConfiguredReadiness(ctx, projects, name, role, cfg, e.Tracker.BackendName() == "local", e.runnerOptions(ctx), deps, reason)
		report.Rows = append(report.Rows, rows...)
		if err != nil {
			return report, fmt.Errorf("%s readiness at %s: %w", role, report.Input, err)
		}
	}
	return report, nil
}

// AcquireAfterPreflight binds scheduled and explicit issue work to the same
// final approved candidate before the first coordination write.
func (e *Engine) AcquireAfterPreflight(ctx context.Context, opts WorkOptions, pre *PreflightResult, owner, primaryAgent, phase string, duration time.Duration) (claimed *lease.IssueState, resultErr error) {
	if pre.Readiness == nil {
		return e.LeaseManager.Acquire(ctx, opts.IssueNumber, owner, primaryAgent, phase, duration, pre.Config.Lease.MaxRenewals)
	}
	ctx, scope, err := storageuse.Ensure(ctx, opts.RepoRoot)
	if err != nil {
		return nil, err
	}
	if scope != nil {
		defer scope.Complete(&resultErr)
	}
	r := workRun{e: e, ctx: ctx, opts: opts, pre: pre}
	return e.LeaseManager.AcquireDirectChecked(ctx, opts.IssueNumber, owner, primaryAgent, phase, duration, pre.Config.Lease.MaxRenewals, r.checkClaimReadiness)
}

func (r *workRun) checkClaimReadiness(st *lease.IssueState) error {
	if st.TaskDigest != r.pre.TaskDigest {
		return fmt.Errorf("approved WorkUnit changed during readiness preflight")
	}
	cfg, err := harness.LoadConfigAtContext(r.ctx, r.opts.RepoRoot, st.BaseSHA)
	if err != nil {
		return err
	}
	wf, err := harness.LoadWorkflowAtContext(r.ctx, r.opts.RepoRoot, r.pre.WorkUnit.Workflow, st.BaseSHA)
	if err != nil {
		return err
	}
	gate, err := contract.ResolveGate(r.pre.WorkUnit.Gate, wf.DefaultGate, cfg.DefaultGate)
	if err != nil {
		return err
	}
	gate, err = contract.ResolveGate(r.opts.Gate, gate, st.EffectiveGate)
	if err != nil {
		return err
	}
	approvalState := *st
	approvalState.EffectiveGate = gate
	hasPlan := hasDesignAndTestPlanArtifacts(st) && !hasRejectedReview(st)
	roles := continuationRoles(rolesForPhase(r.opts.Phase, planningGate(st, gate), hasPlan, lease.VerifyApproval(&approvalState, r.pre.WorkUnit) == nil), st, r.opts)
	agents := make(map[string]string, 2)
	if !strings.EqualFold(cfg.PlanMode, "interactive") || (r.opts.Phase != "plan" && (r.opts.Phase != "all" || hasPlan)) {
		for _, role := range []string{"developer", "tester"} {
			if slices.Contains(roles, role) {
				agents[role] = resolveRoleAgent(cfg, role, r.opts)
			}
		}
	}
	if agents["developer"] != r.pre.Agents["developer"] || agents["tester"] != r.pre.Agents["tester"] {
		return fmt.Errorf("approved developer or tester selection changed during readiness preflight; rerun swarm issue work")
	}
	if st.BaseSHA == r.pre.BaseSHA && st.PolicyDigest == r.pre.PolicyDigest {
		return nil
	}
	deps, err := r.e.AgentDependencies(r.ctx, cfg, r.opts.RepoRoot, st.BaseSHA)
	if err != nil {
		return err
	}
	report, err := r.e.checkIssueReadiness(r.ctx, r.opts, cfg, st.BaseSHA, deps, agents, nil)
	if err != nil {
		return err
	}
	r.pre.Readiness = report
	r.pre.Dependencies = deps
	r.pre.BaseSHA = st.BaseSHA
	r.pre.PolicyDigest = st.PolicyDigest
	return nil
}

func (r *workRun) checkLaunchReadiness(role, name string) error {
	if r.pre.Readiness == nil {
		return nil
	}
	_, err := r.e.checkIssueReadiness(r.ctx, r.opts, r.cfg, r.st.BaseSHA, r.deps, map[string]string{role: name}, nil)
	return err
}
