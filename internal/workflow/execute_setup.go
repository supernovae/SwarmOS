package workflow

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/workspace"
)

func (r *workRun) preflightRun() error {
	var err error
	r.opts.InspectOnly = false
	if r.opts.RepoRoot == "" {
		r.opts.RepoRoot = r.e.repoRoot
	}
	if r.opts.Phase == "" {
		r.opts.Phase = "all"
	}
	if r.opts.Owner == "" {
		var err error
		if r.opts.Owner, err = r.e.Tracker.CurrentUser(r.ctx); err != nil {
			return err
		}
	}
	r.pre, err = r.e.PreflightForClaim(r.ctx, r.opts)
	if err != nil {
		return err
	}
	if err := r.e.LeaseManager.VerifyDependencies(r.ctx, r.pre.WorkUnit); err != nil {
		return err
	}
	r.duration = r.opts.LeaseDuration
	if r.duration <= 0 {
		if r.duration, err = time.ParseDuration(r.pre.Config.Lease.Duration); err != nil {
			return fmt.Errorf("parse configured lease duration: %w", err)
		}
	}
	r.primaryAgent = r.pre.Agents["planner"]
	if r.primaryAgent == "" {
		r.primaryAgent = r.pre.Agents["reviewer"]
	}
	if r.primaryAgent == "" {
		r.primaryAgent = r.pre.Agents["developer"]
	}
	if r.primaryAgent == "" {
		r.primaryAgent = r.pre.Agents["tester"]
	}
	return nil
}

func (r *workRun) claimRun() error {
	var err error
	if r.opts.SetupProgress == nil {
		r.progress.next("claim")
	}
	r.resumed = r.opts.ClaimID != ""
	err = r.opts.SetupProgress.ReportSetupPhase(r.ctx, r.opts.IssueNumber, "claim", func() error {
		if r.resumed {
			r.e.LeaseManager.BindClaim(r.opts.IssueNumber, r.opts.ClaimID)
			r.st, err = r.e.LeaseManager.Resume(r.ctx, r.opts.IssueNumber, r.opts.Owner, r.opts.ClaimID)
		} else {
			r.st, err = r.e.LeaseManager.Acquire(r.ctx, r.opts.IssueNumber, r.opts.Owner, r.primaryAgent, r.opts.Phase, r.duration, r.pre.Config.Lease.MaxRenewals)
		}
		return err
	})
	if err != nil {
		return err
	}
	return nil
}

func (r *workRun) loadClaimPolicy() error {
	var err error
	if r.st.BaseSHA != r.pre.BaseSHA && (r.resumed || r.st.ClaimPreviousBase != r.pre.BaseSHA) {
		return errors.New("approved task base changed during preflight; rerun swarm issue work (or swarm pr work)")
	}
	if r.st.ClaimPreviousBase != "" && r.opts.OnBaseChange != nil {
		r.opts.OnBaseChange(r.opts.IssueNumber, r.st.ClaimPreviousBase, r.st.BaseSHA)
	}
	if r.opts.OnClaim != nil {
		if err := r.opts.OnClaim(r.st); err != nil {
			return fmt.Errorf("persist claim token: %w", err)
		}
	}
	r.cfg, err = harness.LoadConfigAt(r.opts.RepoRoot, r.st.BaseSHA)
	if err != nil {
		return fmt.Errorf("load approved base config: %w", err)
	}
	r.wf, err = harness.LoadWorkflowAt(r.opts.RepoRoot, r.pre.WorkUnit.Workflow, r.st.BaseSHA)
	if err != nil {
		return fmt.Errorf("load approved base workflow: %w", err)
	}
	// A fresh claim may move the base; prefetch for the claimed one. A
	// failure releases the claim through the setup defer.
	r.deps = r.pre.Dependencies
	if r.st.BaseSHA != r.pre.BaseSHA {
		if r.deps, err = r.e.AgentDependencies(r.ctx, r.cfg, r.opts.RepoRoot, r.st.BaseSHA); err != nil {
			return err
		}
	}
	r.gate, err = contract.ResolveGate(r.pre.WorkUnit.Gate, r.wf.DefaultGate, r.cfg.DefaultGate)
	if err != nil {
		return err
	}
	if r.opts.Gate != "" {
		if r.gate, err = contract.ResolveGate(r.opts.Gate, r.gate, r.gate); err != nil {
			return err
		}
	}
	r.st, err = r.e.LeaseManager.RequireGate(r.ctx, r.opts.IssueNumber, r.opts.Owner, r.gate)
	if err != nil {
		return err
	}
	r.gate = r.st.EffectiveGate
	if !(strings.EqualFold(r.cfg.PlanMode, "interactive") && (r.opts.Phase == "plan" || (r.opts.Phase == "all" && !(hasDesignAndTestPlanArtifacts(r.st) && !hasRejectedReview(r.st))))) {
		for _, role := range rolesForPhase(r.opts.Phase, r.gate, (hasDesignAndTestPlanArtifacts(r.st) && !hasRejectedReview(r.st)), lease.VerifyApproval(r.st, r.pre.WorkUnit) == nil) {
			if r.pre.Agents[role] == "" {
				return fmt.Errorf("task phase changed during preflight; rerun swarm issue work (or swarm pr work) to validate the %s agent", role)
			}
		}
	}
	return nil
}

func (r *workRun) startHeartbeat() {
	r.e.Config = r.cfg
	r.coordinatorCtx = r.ctx
	r.heartbeat = r.e.LeaseManager.StartHeartbeat(r.ctx, r.opts.IssueNumber, r.opts.Owner, r.duration)

}

func (r *workRun) resumeForkCheckpoint() error {
	var err error
	if r.forkMode && r.opts.PRNumber == 0 && r.st.ResumeCheckpointBranch != "" {
		// Verify the recorded old claim branch before creating the new claim's
		// namespaced checkpoint. The upstream state supplies both identities.
		observed := workspace.InspectCheckpoint(r.coordinatorCtx, r.opts.RepoRoot, r.forkRemote, r.opts.IssueNumber, r.st.ResumeCheckpointBranch, r.st.HeadSHA)
		if observed.RemoteBranch.SHA != r.st.HeadSHA {
			return fmt.Errorf("budget resume fork checkpoint differs from recorded head")
		}
		if err := workspace.FetchCommit(r.opts.RepoRoot, r.forkRemote, r.st.HeadSHA); err != nil {
			return err
		}
		if err := r.e.LeaseManager.CheckClaim(r.coordinatorCtx, r.opts.IssueNumber, r.opts.Owner); err != nil {
			return err
		}
		if err := workspace.PushForkCheckpointFrom(r.coordinatorCtx, r.opts.RepoRoot, r.e.upstreamRemote(), r.forkRemote, r.st.Branch, r.st.HeadSHA); err != nil {
			return err
		}
		if err := workspace.RebindBudgetCheckpoint(r.coordinatorCtx, r.opts.RepoRoot, r.opts.IssueNumber, r.st.ResumeCheckpointBranch, r.st.Branch, r.st.HeadSHA); err != nil {
			return err
		}
		r.st, err = r.e.LeaseManager.RecordCheckpoint(r.coordinatorCtx, r.opts.IssueNumber, r.opts.Owner, r.st.Branch, r.st.HeadSHA)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *workRun) provisionWorkspace() error {
	var err error
	checkpointRemote, targetBranch := workspace.ClaimCheckpointFrom(r.e.upstreamRemote(), r.forkMode, r.forkRemote, r.opts.IssueNumber, r.pre.Issue.Title, r.st.Lease.ClaimID, r.st.Branch)
	if r.opts.SetupProgress == nil {
		r.progress.next("workspace")
	} else if r.opts.PRNumber != 0 {
		r.progress.next("workspace-check")
	}
	if r.opts.PRNumber != 0 {
		if r.opts.PRWorktree == "" || r.st.PRRevision == nil || r.st.PRRevision.Number != r.opts.PRNumber {
			return fmt.Errorf("PR worktree and fenced revision are required")
		}
		r.wtPath, r.branch = r.opts.PRWorktree, r.st.PRRevision.Branch
		head, dirty, rebasing, headErr := workspace.PRHead(r.ctx, r.wtPath)
		if headErr != nil {
			return headErr
		}
		if rebasing || dirty || head != r.st.PRRevision.LocalHeadSHA {
			return fmt.Errorf("PR worktree is dirty, rebasing, or differs from its recorded checkpoint")
		}
	} else {
		err = r.opts.SetupProgress.ReportSetupPhase(r.ctx, r.opts.IssueNumber, "workspace", func() error {
			pc := r.st.ProvisionContext(r.forkMode)
			pc.UpstreamRemote = r.e.upstreamRemote()
			r.wtPath, r.branch, err = workspace.ProvisionWithContext(r.opts.RepoRoot, checkpointRemote, r.opts.IssueNumber, r.pre.Issue.Title, r.st.BaseSHA, targetBranch, r.st.HeadSHA, pc)
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *workRun) initializeResult() {
	r.res = &WorkResult{IssueNumber: r.opts.IssueNumber, WorktreePath: r.wtPath, BranchName: r.branch, BaseSHA: r.st.BaseSHA, HeadSHA: r.st.HeadSHA, RoleAgentsUsed: map[string]string{}, State: r.st}
	if r.opts.PRNumber != 0 {
		r.res.HeadSHA = r.st.PRRevision.LocalHeadSHA
	}
	r.ctx = r.heartbeat.Context()
}

func (r *workRun) prepareRun() (bool, error) {
	r.mode = "interactive"
	if r.opts.Headless {
		r.mode = "headless"
	}
	r.implementationBase = r.st.BaseSHA
	if r.st.NextPhaseReason == "claim budget" && r.st.HeadSHA != "" {
		r.implementationBase = r.st.HeadSHA
	}
	if r.opts.PRNumber != 0 {
		r.implementationBase = r.st.PRRevision.LocalHeadSHA
	}
	if r.opts.PRNumber != 0 {
		if err := workspace.WriteReviewEvidence(r.wtPath, r.opts.ReviewEvidence); err != nil {
			return true, err
		}
		if _, err := workspace.WriteCIEvidence(r.wtPath, r.opts.CIEvidence); err != nil {
			return true, err
		}
	}
	return false, nil
}

func (r *workRun) setupFork() {
	r.forkMode = r.e.ForkMode || r.opts.ForkMode
	r.forkRemote = strings.TrimSpace(r.opts.ForkRemote)
	if r.forkRemote == "" {
		r.forkRemote = strings.TrimSpace(r.e.ForkRemote)
	}
	if r.forkRemote == "" {
		r.forkRemote = workspace.DefaultForkRemote
	}
}
