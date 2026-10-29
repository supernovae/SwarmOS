package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/sandbox"
	"github.com/supernovae/swarmos/internal/workspace"
)

type workRun struct {
	primaryAgent                                 string
	e                                            *Engine
	opts                                         WorkOptions
	ctx, coordinatorCtx                          context.Context
	progress                                     *workProgress
	pre                                          *PreflightResult
	duration                                     time.Duration
	st                                           *lease.IssueState
	resumed, setupComplete                       bool
	cfg                                          *contract.SwarmConfig
	wf                                           *contract.WorkflowSpec
	deps                                         sandbox.Dependencies
	gate                                         contract.GateMode
	heartbeat                                    *lease.Heartbeat
	forkMode                                     bool
	forkRemote, wtPath, branch                   string
	res                                          *WorkResult
	activePhase                                  string
	preserveCheckpoint                           bool
	mode, implementationBase                     string
	runPlan                                      bool
	rejectedPlan, rejectedTest, rejectedFindings string
	rerun                                        string
	repairLimit                                  int
	outcomeFailed                                bool
}

func (r *workRun) releaseFailedSetup() {
	// Release only a claim this run acquired; a resumed claim may hold
	// work (such as uncommitted agent changes) its owner still needs.
	if !r.setupComplete && !r.resumed {
		_, _ = r.e.LeaseManager.Release(context.Background(), r.opts.IssueNumber, r.opts.Owner, "workflow setup failed")
	}
}

func (r *workRun) check() error {
	if heartbeatErr := r.heartbeat.Err(); heartbeatErr != nil && !budgetInterrupted(r.ctx, context.Canceled) {
		return fmt.Errorf("%w: %w", ErrLeaseLost, heartbeatErr)
	}
	if err := r.e.LeaseManager.CheckClaim(r.coordinatorCtx, r.opts.IssueNumber, r.opts.Owner); err != nil {
		r.heartbeat.Stop()
		return fmt.Errorf("%w: %w", ErrLeaseLost, err)
	}
	return nil
}

func (r *workRun) finishAdmission(result **WorkResult, workErr *error) {
	var admission *budgetAdmissionError
	if errors.As(*workErr, &admission) {
		r.res.State = admission.state
		setBudgetPause(r.res, admission.budget, admission.state.Lease.NextPhase, r.opts)
		*result, *workErr = r.res, nil
	}
}

func (r *workRun) finishInterrupted(result **WorkResult, workErr *error) {
	var rejection *importRejection
	if *workErr != nil && !errors.As(*workErr, &rejection) && budgetInterrupted(r.ctx, *workErr) {
		*workErr = r.e.pauseWork(r.coordinatorCtx, r.res, r.opts, r.pre.WorkUnit, r.activePhase, r.preserveCheckpoint, r.forkMode, r.forkRemote)
		*result = r.res
	}
}

func (r *workRun) checkImport(role string) func(context.Context, []string, []string) error {
	return func(ctx context.Context, paths, deleted []string) error {
		return r.checkRoleImport(ctx, role, paths, deleted)
	}
}
func (r *workRun) checkRoleImport(importCtx context.Context, role string, paths, deleted []string) error {
	if err := r.e.LeaseManager.CheckClaim(importCtx, r.opts.IssueNumber, r.opts.Owner); err != nil {
		return err
	}
	if r.opts.Phase == "test" {
		if err := r.checkTestRetry(); err != nil {
			return err
		}
	}
	if err := writableImportPolicy(role, paths, r.pre.Validation.ProtectedPaths, r.pre.WorkUnit.AllowHarnessChanges, r.forkMode); err != nil {
		return err
	}
	return checkDeletions(importCtx, r.wtPath, r.st, role, deleted, r.forkMode)
}

// rejectedRunSnapshot retains the clean-head evidence from before one agent run.
type rejectedRunSnapshot struct {
	run             *workRun
	role, head      string
	dirty, rebasing bool
	headErr         error
}

func (r *workRun) preserveRejected(role string) func(context.Context, error, []byte) {
	head, dirty, rebasing, headErr := workspace.StatusPRHead(r.ctx, r.wtPath)
	snapshot := rejectedRunSnapshot{r, role, head, dirty, rebasing, headErr}
	return snapshot.preserve
}
func (s rejectedRunSnapshot) preserve(_ context.Context, runErr error, patch []byte) {
	r := s.run
	var rejection *importRejection
	if !errors.As(runErr, &rejection) || !rejection.Recoverable {
		return
	}
	switch {
	case s.headErr != nil || s.dirty || s.rebasing:
		rejection.PatchNote = "worktree was not clean when the run started"
	case patch == nil:
		rejection.PatchNote = "a complete recovery patch was unavailable"
	default:
		path, err := agent.SaveRecoveryPatch(r.wtPath, agent.RecoveryHeader{Issue: r.opts.IssueNumber, PR: r.opts.PRNumber, Role: s.role, HeadSHA: s.head, Rejected: rejection.Paths}, patch)
		if err != nil {
			rejection.PatchNote = err.Error()
		} else {
			rejection.Patch = path
		}
	}
}
func (r *workRun) rejectImport(runErr error) error {
	var rejection *importRejection
	if !errors.As(runErr, &rejection) {
		return runErr
	}
	var completed *sandbox.CompletedImportError
	rejection.Completed = errors.As(runErr, &completed)
	retryEligible := rejection.Role == "tester" && !r.forkMode && r.checkTestRetry() == nil
	reason := importRejectionReason(rejection, r.opts.IssueNumber, r.opts.PRNumber, retryEligible)
	updated, err := r.e.LeaseManager.Handoff(r.coordinatorCtx, r.opts.IssueNumber, r.opts.Owner, "implement", "implement", reason)
	if err != nil {
		return fmt.Errorf("%w: record import rejection: %w", runErr, err)
	}
	r.res.State = updated
	if rejection.Patch != "" {
		// Only the local error names the private path; the projected reason does not.
		err := fmt.Errorf("%s (saved at %s): %w", reason, rejection.Patch, runErr)
		if retryEligible {
			return &TestRetryAvailableError{Err: err}
		}
		return err
	}
	err = fmt.Errorf("%s: %w", reason, runErr)
	if retryEligible {
		return &TestRetryAvailableError{Err: err}
	}
	return err
}

func (r *workRun) pauseInterrupted(runErr error, phase string, checkpoint bool) (bool, error) {
	if !budgetInterrupted(r.ctx, runErr) {
		return false, runErr
	}
	return true, r.e.pauseWork(r.coordinatorCtx, r.res, r.opts, r.pre.WorkUnit, phase, checkpoint, r.forkMode, r.forkRemote)
}

func (r *workRun) admit(phase string) error {
	if budgetInterrupted(r.ctx, context.Canceled) {
		return r.e.pauseAdmission(r.coordinatorCtx, r.opts, phase)
	}
	return r.e.admitStage(r.ctx, r.opts.IssueNumber, r.opts.Owner, phase)
}

func (r *workRun) stopForSplit() bool {
	if !splitRecommended(r.st) {
		return false
	}
	r.res.State, r.res.PhaseCompleted, r.res.SplitRecommended = r.st, "plan", true
	return true
}

func (r *workRun) stopHeartbeat() { r.heartbeat.Stop() }

// TestRetryAvailableError carries coordinator-verified recovery eligibility.
type TestRetryAvailableError struct{ Err error }

func (e *TestRetryAvailableError) Error() string { return e.Err.Error() }
func (e *TestRetryAvailableError) Unwrap() error { return e.Err }
