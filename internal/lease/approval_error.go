package lease

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

func (e *HumanPlanApprovalError) Error() string { return e.Unwrap().Error() }
func (e *HumanPlanApprovalError) Unwrap() error {
	if e.Err == nil {
		return ErrApprovalRequired
	}
	return e.Err
}
