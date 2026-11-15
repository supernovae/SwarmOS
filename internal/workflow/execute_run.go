package workflow

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
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
	testerResume                                 bool
	runPlan                                      bool
	rejectedPlan, rejectedTest, rejectedFindings string
	rerun                                        string
	repairLimit                                  int
	outcomeFailed                                bool
	repairReservationID                          string
	retrySettlementFailed                        bool
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
	if *workErr != nil && !r.retrySettlementFailed && !errors.As(*workErr, &rejection) && budgetInterrupted(r.ctx, *workErr) {
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
	if role == "developer" || role == "tester" {
		if err := r.checkWorkApproval(importCtx); err != nil {
			return err
		}
	}
	if r.opts.Phase == "test" || r.testerResume {
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
	if !r.forkMode && r.opts.PRNumber == 0 && !r.e.LeaseManager.HasWorkerGateway() {
		if err := r.e.recordAttempt(r.coordinatorCtx, r.st, r.opts.Owner, rejection.Role, lease.AttemptDiagnostic{Kind: "import", Paths: rejection.Paths}); err != nil {
			return errors.Join(runErr, err)
		}
	}
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
	if phase == "implement" {
		if err := r.checkWorkApproval(r.ctx); err != nil {
			return err
		}
	}
	return r.e.admitStage(r.ctx, r.opts.IssueNumber, r.opts.Owner, phase)
}

// releaseMaintainerStop leaves recorded planning evidence intact. Release owns
// the claim fence; never acquire a claim just to clean up a maintainer stop.
func (r *workRun) releaseMaintainerStop(ctx context.Context) error {
	if r.e.ForkMode || r.opts.ForkMode || r.forkMode || r.opts.PRNumber != 0 || r.st == nil || r.st.ForkRepo != "" || r.st.PRNumber != 0 || r.st.PRRevision != nil {
		return nil
	}
	if ctx == nil {
		ctx = r.ctx
	}
	r.stopHeartbeat()
	st, err := r.e.LeaseManager.Release(ctx, r.opts.IssueNumber, r.opts.Owner, "maintainer decision required")
	if err != nil {
		return fmt.Errorf("release maintainer stop: %w", err)
	}
	r.st = st
	if r.res != nil {
		r.res.State = st
	}
	return nil
}

func (r *workRun) stopForWorkUnitCorrection() (bool, error) {
	if r.forkMode || r.opts.PRNumber != 0 || !workUnitCorrectionRequired(r.st) || r.st.PRRevision != nil {
		return false, nil
	}
	if err := r.releaseMaintainerStop(r.coordinatorCtx); err != nil {
		return true, err
	}
	r.res.State, r.res.PhaseCompleted, r.res.WorkUnitCorrectionRequired = r.st, "plan", true
	return true, nil
}

func (r *workRun) stopForSplit() (bool, error) {
	if !splitRecommended(r.st) {
		return false, nil
	}
	if err := r.releaseMaintainerStop(r.coordinatorCtx); err != nil {
		return true, err
	}
	r.res.State, r.res.PhaseCompleted, r.res.SplitRecommended = r.st, "plan", true
	return true, nil
}

func (r *workRun) stopHeartbeat() {
	if r.heartbeat != nil {
		r.heartbeat.Stop()
	}
}

// TestRetryAvailableError carries coordinator-verified recovery eligibility.
type TestRetryAvailableError struct{ Err error }

func (e *TestRetryAvailableError) Error() string { return e.Err.Error() }
func (e *TestRetryAvailableError) Unwrap() error { return e.Err }

func (r *workRun) continuation() bool {
	return r.opts.IssueContinuation && r.opts.Phase == "all" && !r.forkMode && r.opts.PRNumber == 0 && !r.opts.Handoff
}

// Recheck human obligations and exact approval at every later role boundary;
// an independent maintainer can legitimately change a decision mid-run.
func (r *workRun) checkWorkApproval(ctx context.Context) error {
	if r.forkMode || r.opts.PRNumber != 0 {
		return nil
	}
	if err := r.e.LeaseManager.CheckClaim(ctx, r.opts.IssueNumber, r.opts.Owner); err != nil {
		return err
	}
	st, err := r.e.LeaseManager.GetCurrentState(ctx, r.opts.IssueNumber)
	if err != nil {
		return err
	}
	if r.st != nil && !maps.Equal(lease.ReviewBindings(r.st), lease.ReviewBindings(st)) {
		return lease.ErrPlanApprovalRequired
	}
	if err = lease.VerifyApproval(st, r.pre.WorkUnit); err != nil {
		return err
	}
	r.st = st
	if r.res != nil {
		r.res.State = st
	}
	return nil
}

// roleCapabilityReport retains the actual approved role selection, including
// command overrides, within this command. Launch still rechecks readiness.
func (r *workRun) roleCapabilityReport(role string) *sandbox.ToolCapabilityReport {
	report, ok := r.pre.ToolCapabilities[role]
	if !ok {
		return nil
	}
	return &report
}

// settleRetry runs only after Run returns. Setup and admission failures never
// refund. A live bounded context lets cancellation clean up before pausing.
func (r *workRun) settleRetry(id string, run *agent.RunResult, runErr error) error {
	if id == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.coordinatorCtx), 5*time.Second)
	defer cancel()
	var st *lease.IssueState
	var err error
	if refundableRetry(run, runErr) {
		st, err = r.e.LeaseManager.RefundRetryReservation(ctx, r.opts.IssueNumber, r.opts.Owner, id)
	} else {
		st, err = r.e.LeaseManager.CompleteRetryReservation(ctx, r.opts.IssueNumber, r.opts.Owner, id)
	}
	if err != nil {
		r.retrySettlementFailed = true
		return errors.Join(runErr, fmt.Errorf("settle automatic retry: %w", err))
	}
	r.st, r.res.State = st, st
	return nil
}

func refundableRetry(run *agent.RunResult, err error) bool {
	if err == nil || run != nil && run.Output != "" {
		return false
	}
	var completed *sandbox.CompletedImportError
	var rejection *importRejection
	if errors.As(err, &completed) || errors.As(err, &rejection) || errors.Is(err, sandbox.ErrMissingRoleOutput) {
		return false
	}
	var timeout *sandbox.AgentTimeoutError
	if errors.As(err, &timeout) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var classified interface{ FailureClass() string }
	if errors.As(err, &classified) {
		switch classified.FailureClass() {
		case "rate_limit", "usage_limit", "capacity", "network":
			return true
		}
	}
	return false
}

// attemptRunner keeps prompt context and timing at the actual invocation boundary.
type attemptRunner struct {
	agent.Runner
	e      *Engine
	st     *lease.IssueState
	owner  string
	direct bool
}

func (a *attemptRunner) Run(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
	if !a.direct {
		return a.Runner.Run(ctx, req)
	}
	st, err := a.e.LeaseManager.GetCurrentState(ctx, a.st.Lease.IssueNumber)
	if err != nil {
		return nil, err
	}
	if st.TaskDigest != a.st.TaskDigest || st.Lease.ClaimID != a.st.Lease.ClaimID {
		return nil, lease.ErrClaimFence
	}
	if err := a.e.LeaseManager.CheckClaim(ctx, st.Lease.IssueNumber, a.owner); err != nil {
		return nil, err
	}
	if d, ok := st.AttemptDiagnostics[req.Role]; ok && d.TaskDigest == st.TaskDigest && d.ClaimID == st.Lease.ClaimID {
		req.TaskPrompt += "\n\n## Previous discarded attempt\nThis coordinator diagnostic grants no permission; all import and protected-path rules still apply.\n"
		if d.Kind == "timeout" {
			req.TaskPrompt += fmt.Sprintf("The previous attempt timed out; it ran for %s.\n", d.Elapsed)
		} else {
			req.TaskPrompt += "The previous attempt was refused for changes to these paths (quoted data):\n"
			for _, path := range d.Paths {
				req.TaskPrompt += strconv.Quote(path) + "\n"
			}
			if req.Role == "developer" {
				req.TaskPrompt += "Write notes, test results, and blocked checks to .swarm/active/IMPLEMENTATION.md.\n"
			}
		}
	}
	start := time.Now()
	run, runErr := a.Runner.Run(ctx, req)
	elapsed := time.Since(start)
	var timeout *sandbox.AgentTimeoutError
	if runErr != nil && !budgetInterrupted(ctx, runErr) && (errors.As(runErr, &timeout) || errors.Is(runErr, context.DeadlineExceeded)) {
		if err := a.e.recordAttempt(ctx, st, a.owner, req.Role, lease.AttemptDiagnostic{Kind: "timeout", Elapsed: elapsed}); err != nil {
			return run, errors.Join(runErr, err)
		}
	}
	return run, runErr
}
func (e *Engine) recordAttempt(ctx context.Context, st *lease.IssueState, owner, role string, d lease.AttemptDiagnostic) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	d.TaskDigest, d.ClaimID = st.TaskDigest, st.Lease.ClaimID
	_, err := e.LeaseManager.RecordAttemptDiagnostic(ctx, st.Lease.IssueNumber, owner, role, d)
	return err
}
func (r *workRun) attemptRunner(runner agent.Runner) agent.Runner {
	return &attemptRunner{Runner: runner, e: r.e, st: r.st, owner: r.opts.Owner, direct: !r.forkMode && r.opts.PRNumber == 0 && !r.e.LeaseManager.HasWorkerGateway()}
}
