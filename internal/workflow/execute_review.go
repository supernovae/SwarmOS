package workflow

import (
	"fmt"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
)

func (r *workRun) reviewGate() (bool, error) {
	if r.stopForSplit() {
		return true, nil
	}
	if r.gate == contract.GateHuman && (r.opts.Phase == "review" || r.opts.Phase == "all" && (r.runPlan || lease.VerifyApproval(r.st, r.pre.WorkUnit) != nil)) {
		return r.awaitHumanReview()
	}
	if (r.gate == contract.GateAgent && (r.runPlan || (r.opts.Phase == "all" && lease.VerifyApproval(r.st, r.pre.WorkUnit) != nil))) || (r.opts.Phase == "review" && r.gate != contract.GateHuman) {
		return r.runReview()
	}
	return false, nil
}

func (r *workRun) finishReviewHandoff() (bool, error) {
	var err error
	if r.opts.Phase == "review" {
		if r.opts.Handoff {
			r.progress.next("handoff")
			if err := r.check(); err != nil {
				return true, err
			}
			if r.st, err = r.e.LeaseManager.Handoff(r.ctx, r.opts.IssueNumber, r.opts.Owner, "review", "implement"); err != nil {
				return true, err
			}
			r.res.State, r.res.HandedOffToPhase = r.st, "implement"
		}
		return true, nil
	}
	return false, nil
}

func (r *workRun) awaitHumanReview() (bool, error) {
	var err error
	r.progress.next("awaiting-approval")
	if err := r.check(); err != nil {
		return true, err
	}
	if r.opts.Handoff {
		if r.st, err = r.e.LeaseManager.Handoff(r.ctx, r.opts.IssueNumber, r.opts.Owner, "plan", "review"); err != nil {
			return true, err
		}
		r.res.State, r.res.HandedOffToPhase = r.st, "review"
	} else {
		if r.st, err = r.e.LeaseManager.UpdatePhase(r.ctx, r.opts.IssueNumber, r.opts.Owner, "review", "active"); err != nil {
			return true, err
		}
		r.res.State = r.st
	}
	reviewers, noticeErr := r.e.LeaseManager.NotifyPlanReviewers(r.ctx, r.opts.IssueNumber)
	r.res.PlanReviewers = reviewers
	if noticeErr != nil {
		r.res.ReviewerNoticeError = noticeErr.Error()
	}
	r.res.PausedForHuman, r.res.PhaseCompleted = true, "plan"
	return true, nil
}

func (r *workRun) runReview() (bool, error) {
	var err error
	r.progress.next("review")
	if r.gate != contract.GateAgent && r.gate != contract.GateNone {
		return true, fmt.Errorf("%w: gate %q requires an authenticated human decision", ErrPlanApprovalNeeded, r.gate)
	}
	if r.st, err = r.e.LeaseManager.GetCurrentState(r.ctx, r.opts.IssueNumber); err != nil {
		return true, err
	}
	r.res.State = r.st
	reviewer := r.pre.Agents["reviewer"]
	r.res.RoleAgentsUsed["reviewer"] = reviewer
	r.activePhase = "review"
	verdict, reviewed, err := r.e.runAgentReview(r.ctx, r.wtPath, r.pre.WorkUnit, r.wf, r.st, r.opts.Owner, reviewer, r.mode, r.opts.instructionsFor("reviewer"), r.opts.PRNumber, r.opts, r.opts.SetupProgress)
	if paused, pauseErr := r.pauseInterrupted(err, "review", false); paused {
		return true, pauseErr
	}
	r.res.ReviewVerdict = verdict
	if reviewed != nil {
		r.st, r.res.State = reviewed, reviewed
	}
	if err != nil {
		return true, err
	}
	r.res.PhaseCompleted = "review"
	if done, err := r.finishReviewHandoff(); done {
		return true, err
	}
	return false, nil
}
