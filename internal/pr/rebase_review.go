package pr

import (
	"context"
	"fmt"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/workflow"
	"github.com/supernovae/swarmos/internal/workspace"
)

// policyReviewNeeded requires a previously authenticated verdict on exactly
// these plans and this task. Only the old policy/base bindings may differ.
func policyReviewNeeded(st *lease.IssueState, wu *contract.WorkUnit) bool {
	if st.CarryReason != "policy files changed" || st.EffectiveGate != contract.GateAgent {
		return false
	}
	old := *st
	old.ApprovalTransition = nil
	for _, a := range st.Artifacts {
		if a.Type == contract.ArtifactReview {
			old.BaseSHA = a.Metadata["base_sha"]
			old.PolicyDigest = a.Metadata["policy_digest"]
			old.PolicyTreeDigest = a.Metadata["policy_tree_digest"]
			if old.BaseSHA == "" || old.PolicyDigest == "" || old.PolicyTreeDigest == "" || old.PolicyTreeDigest == st.PolicyTreeDigest {
				return false
			}
			return lease.VerifyApproval(&old, wu) == nil
		}
	}
	return false
}

func (s *Service) preflightRebaseReview(ctx context.Context, r *Result, opts Options, rv *resolver) error {
	if lease.VerifyApproval(r.State, rv.unit) == nil {
		return nil
	}
	if r.State.EffectiveGate == contract.GateHuman {
		return &lease.HumanPlanApprovalError{IssueNumber: r.IssueNumber, Err: fmt.Errorf("human plan approval required: swarm issue review-plan %d --approve", r.IssueNumber)}
	}
	if s.Fork || !policyReviewNeeded(r.State, rv.unit) {
		return fmt.Errorf("plan review required: swarm issue review-plan %d", r.IssueNumber)
	}
	cfg, err := harness.LoadConfigAt(s.Root, r.BaseSHA)
	if err != nil {
		return err
	}
	name := workflow.ResolveRoleAgent(cfg, "reviewer", opts.Work)
	_, err = agent.Preflight(name, cfg, s.Tracker.BackendName() == "local", "reviewer")
	return err
}

func (s *Service) reviewRebasePlan(ctx context.Context, r *Result, owner string, st *lease.IssueState, opts Options, rv *resolver) (*lease.IssueState, error) {
	if lease.VerifyApproval(st, rv.unit) == nil {
		return st, nil
	}
	if s.Fork || !policyReviewNeeded(st, rv.unit) {
		return st, fmt.Errorf("plan approval required: swarm issue review-plan %d", r.IssueNumber)
	}
	if err := restoreApprovedPlan(r.Worktree, st); err != nil {
		return st, err
	}
	cfg, err := harness.LoadConfigAt(s.Root, st.BaseSHA)
	if err != nil {
		return st, err
	}
	wf, err := harness.LoadWorkflowAt(s.Root, rv.unit.Workflow, st.BaseSHA)
	if err != nil {
		return st, err
	}
	name := workflow.ResolveRoleAgent(cfg, "reviewer", opts.Work)
	engine := workflow.NewEngineWithManager(s.Root, s.Tracker, s.Manager, cfg, false, "")
	_, updated, err := engine.RunAgentReview(ctx, r.Worktree, rv.unit, wf, st, owner, name, rv.mode, "Review the unchanged approved plan under the new policy; do not re-plan.", r.PR.Number)
	if cleanErr := workspace.CleanEphemeralHarnessMarkers(r.Worktree); cleanErr != nil {
		return updated, cleanErr
	}
	if err != nil {
		return updated, err
	}
	if err = lease.VerifyApproval(updated, rv.unit); err != nil {
		return updated, err
	}
	updated.CarryReason = "approved plan re-reviewed"
	return updated, nil
}
