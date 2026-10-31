package lease

import (
	"github.com/supernovae/swarmos/internal/contract"
	"strings"
)

// TaskApprovalError marks a stop that only `swarm issue approve` can clear:
// the WorkUnit or its approved baseline no longer matches the recorded
// approval. It wraps ErrApprovalRequired so existing checks keep working.
type TaskApprovalError struct {
	IssueNumber int
	Err         error
}

func (e *TaskApprovalError) Error() string { return e.Unwrap().Error() }
func (e *TaskApprovalError) Unwrap() error {
	if e.Err == nil {
		return ErrApprovalRequired
	}
	return e.Err
}

// HumanPlanApprovalError marks a human-gated task whose plan lacks a current
// authenticated maintainer decision (`swarm issue review-plan <N> --approve`).
type HumanPlanApprovalError struct {
	IssueNumber int
	Err         error
}

func (e *HumanPlanApprovalError) Error() string { return planApprovalMessage(e.Unwrap()) }
func (e *HumanPlanApprovalError) Unwrap() error {
	if e.Err == nil {
		return ErrPlanApprovalRequired
	}
	return e.Err
}

// PlanApprovalError marks a plan lacking a current trusted review.
// Contributor reviews remain advisory and cannot clear this stop.
type PlanApprovalError struct {
	IssueNumber int
	Err         error
}

func (e *PlanApprovalError) Error() string { return planApprovalMessage(e.Unwrap()) }
func (e *PlanApprovalError) Unwrap() error {
	if e.Err == nil {
		return ErrPlanApprovalRequired
	}
	return e.Err
}

// ErrPlanApprovalRequired preserves compatibility with the general approval sentinel.
var ErrPlanApprovalRequired error = planApprovalRequired{}

type planApprovalRequired struct{}

func (planApprovalRequired) Error() string        { return "plan approval is missing or stale" }
func (planApprovalRequired) Is(target error) bool { return target == ErrApprovalRequired }
func planApprovalMessage(err error) string {
	return strings.ReplaceAll(err.Error(), ErrApprovalRequired.Error(), ErrPlanApprovalRequired.Error())
}

// VerifyHumanPlanApproval checks the exact authenticated report binding without mutation.
func VerifyHumanPlanApproval(st *IssueState) error {
	if st == nil || st.EffectiveGate != contract.GateHuman {
		return nil
	}
	if err := verifyReviewBinding(st, contract.GateHuman); err != nil {
		return &HumanPlanApprovalError{IssueNumber: st.Lease.IssueNumber, Err: err}
	}
	return nil
}
