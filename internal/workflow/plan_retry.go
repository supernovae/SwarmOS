package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/safefs"
	"github.com/supernovae/swarmos/internal/sandbox"
	"github.com/supernovae/swarmos/internal/workspace"
)

// ErrPlanRetryRecovery means a retained retry must settle before planning resumes.
var ErrPlanRetryRecovery = errors.New("planner retry outcome awaiting recovery")

type planRetryRecord struct {
	Checkout string
	Digest   string
	Outcome  lease.PlanRetryOutcome
}

func (e *Engine) directGitHubPlanRetry(opts WorkOptions) bool {
	return !e.ForkMode && !opts.ForkMode && opts.PRNumber == 0 && !e.LeaseManager.HasWorkerGateway() &&
		strings.HasPrefix(e.Tracker.BackendName(), "github (")
}

func (e *Engine) planRetryLocation(n int) (string, string, error) {
	root, err := filepath.Abs(e.repoRoot)
	if err != nil {
		return "", "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", err
	}
	return workspace.WorktreePath(root, n), e.Tracker.BackendName() + "\n" + root, nil
}

func planRetryFiles(root string, n int) ([2]string, [2]string, error) {
	var bodies, digests [2]string
	wt, err := filepath.Rel(root, workspace.WorktreePath(root, n))
	if err != nil {
		return bodies, digests, err
	}
	repo, err := safefs.Open(root)
	if err != nil {
		return bodies, digests, err
	}
	defer repo.Close()
	active, err := repo.Sub(filepath.Join(wt, ".swarm", "active"), false, 0)
	if err != nil {
		return bodies, digests, fmt.Errorf("open managed planner output directory: %w", err)
	}
	defer active.Close()
	var readErr error
	for i, name := range []string{"PLAN.md", "TEST_PLAN.md"} {
		data, err := active.ReadFile(name, 1<<20)
		switch {
		case err == nil:
			bodies[i] = string(data)
			digests[i] = fmt.Sprintf("%x", sha256.Sum256(data))
			if strings.TrimSpace(bodies[i]) == "" {
				readErr = errors.Join(readErr, fmt.Errorf("did not produce .swarm/active/%s", name))
			} else if !utf8.Valid(data) {
				readErr = errors.Join(readErr, fmt.Errorf(".swarm/active/%s must be valid UTF-8", name))
			}
		case errors.Is(err, os.ErrNotExist):
			digests[i] = "missing"
			readErr = errors.Join(readErr, fmt.Errorf("missing .swarm/active/%s", name))
		default:
			readErr = errors.Join(readErr, fmt.Errorf("cannot read .swarm/active/%s: %w", name, err))
		}
	}
	return bodies, digests, readErr
}

func (r *workRun) settlePlannerOutcome(planner string, run *agent.RunResult, runErr error) (bool, error) {
	n, owner := r.opts.IssueNumber, r.opts.Owner
	checkout, repo, locationErr := r.e.planRetryLocation(n)
	o := lease.PlanRetryOutcome{Reservation: *r.st.PlanRetryReservation, Owner: owner, Refund: refundableRetry(run, runErr)}
	bodies, digests, readErr := planRetryFiles(r.e.repoRoot, n)
	o.FileDigests = digests
	switch {
	case runErr != nil:
		o.Failure = "failed"
		var timeout *sandbox.AgentTimeoutError
		if !budgetInterrupted(r.ctx, runErr) && (errors.As(runErr, &timeout) || errors.Is(runErr, context.DeadlineExceeded)) {
			o.Failure = "timeout"
		}
		var classified interface{ FailureClass() string }
		if errors.As(runErr, &classified) {
			if cause := agent.FailureCause(classified.FailureClass()); cause != "" {
				runErr = fmt.Errorf("%w; %s", runErr, cause)
			}
		}
		observeRun(run, r.st, false)
		if run != nil && run.Telemetry != nil && run.Telemetry.Role == "planner" && run.Telemetry.FailureClass != "" {
			o.Telemetry, _ = run.Telemetry.Encode()
		}
	case readErr != nil:
		o.Failure = "invalid-output"
		runErr = fmt.Errorf("planner agent %q: %w", planner, readErr)
	case canonicalReport(bodies[0]) == r.rejectedPlan && canonicalReport(bodies[1]) == r.rejectedTest:
		o.Failure = "unchanged"
		runErr = fmt.Errorf("%w: %w; revise PLAN.md or TEST_PLAN.md before review", ErrPlanReviewRejected, lease.ErrPlanUnchanged)
	default:
		observeRun(run, r.st, false)
		meta := runMetadata(run, nil)
		o.Plan = contract.Artifact{Type: contract.ArtifactDesign, Producer: planner + ":planner", Summary: fmt.Sprintf("Implementation plan for #%d", n), Body: guidedReport(bodies[0], run), Metadata: meta}
		o.Test = contract.Artifact{Type: contract.ArtifactTestPlan, Producer: planner + ":planner", Summary: fmt.Sprintf("Test plan covering %d acceptance criteria", len(r.pre.WorkUnit.Acceptance)), Body: guidedReport(bodies[1], run), Metadata: meta}
		if err := r.e.LeaseManager.ValidatePlanRetryPair(r.st, n, o.Plan, o.Test); err != nil {
			o.Failure = "invalid-output"
			o.Plan, o.Test = contract.Artifact{}, contract.Artifact{}
			runErr = fmt.Errorf("planner output: %w", err)
			if errors.Is(err, lease.ErrPlanUnchanged) {
				o.Failure = "unchanged"
				runErr = errors.Join(ErrPlanReviewRejected, runErr)
			}
		}
	}
	saveErr := locationErr
	if saveErr == nil && (digests[0] == "" || digests[1] == "") {
		saveErr = fmt.Errorf("retained planner files cannot be verified: %w", readErr)
	}
	if saveErr == nil {
		var data []byte
		var digest string
		digest, saveErr = o.Digest()
		if saveErr == nil {
			data, saveErr = json.Marshal(planRetryRecord{Checkout: checkout, Digest: digest, Outcome: o})
		}
		if saveErr == nil {
			saveErr = agent.SavePlanRetryRecovery(checkout, repo, n, o.Reservation.ID, data)
		}
	}
	st, err := r.e.settlePlanRetry(r.coordinatorCtx, r.opts, o)
	if err != nil {
		r.retrySettlementFailed = true
		err = errors.Join(runErr, fmt.Errorf("settle planner retry: %w", err))
		if saveErr != nil {
			return true, errors.Join(err, fmt.Errorf("save private planner retry evidence: %w", saveErr))
		}
		if !errors.Is(err, lease.ErrClaimFence) {
			err = planRetryRecoveryError(n, o.Reservation.ID, err)
		}
		return true, err
	}
	r.st, r.res.State = st, st
	if saveErr != nil {
		warning := fmt.Errorf("could not save private planner retry evidence: %w", saveErr)
		r.res.PlanRetryRecoveryWarning = warning.Error()
		if runErr != nil {
			runErr = errors.Join(runErr, warning)
		}
	}
	if o.Failure != "" {
		if saveErr == nil {
			runErr = errors.Join(runErr, agent.RemovePlanRetryRecovery(checkout, repo, n, o.Reservation.ID))
		}
		if o.Refund {
			if paused, err := r.pauseInterrupted(runErr, "plan", false); paused {
				return true, err
			}
		}
		r.retrySettlementFailed = !o.Refund
		return true, runErr
	}
	done, err := r.finishRecordedPlan()
	if err == nil && saveErr == nil {
		err = agent.RemovePlanRetryRecovery(checkout, repo, n, o.Reservation.ID)
	}
	return done || err != nil, err
}

const planRetrySettlementTimeout = 5 * time.Second

func (e *Engine) settlePlanRetry(ctx context.Context, opts WorkOptions, o lease.PlanRetryOutcome) (*lease.IssueState, error) {
	ctx = context.WithoutCancel(ctx)
	write := func(rev string) (*lease.IssueState, error, bool) {
		attempt, cancel := context.WithTimeout(ctx, planRetrySettlementTimeout)
		defer cancel()
		st, err := e.LeaseManager.SettlePlanRetry(attempt, opts.IssueNumber, opts.Owner, rev, o)
		ambiguous := err != nil && (attempt.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, coordination.ErrPublicationUnknown))
		return st, err, ambiguous
	}
	var prior error
	if opts.RecoverPlanRetry == "" {
		st, err, ambiguous := write("")
		if err == nil || (!ambiguous && !errors.Is(err, coordination.ErrConflict)) {
			return st, err
		}
		prior = err
	}
	replayed := false
	for conflicts := 0; ; conflicts++ {
		inspection, cancel := context.WithTimeout(ctx, planRetrySettlementTimeout)
		st, rev, err := e.LeaseManager.InspectPlanRetry(inspection, opts.IssueNumber, opts.Owner)
		cancel()
		if err != nil {
			return nil, errors.Join(prior, err)
		}
		settled, err := lease.MatchPlanRetryOutcome(st, opts.Owner, o)
		if err != nil {
			return nil, errors.Join(prior, err)
		}
		if settled {
			projection, cancel := context.WithTimeout(ctx, planRetrySettlementTimeout)
			defer cancel()
			e.LeaseManager.ProjectSettledPlanRetry(projection, opts.IssueNumber, st, o.Failure == "")
			return st, nil
		}
		if replayed || conflicts == 4 {
			return nil, prior
		}
		st, err, ambiguous := write(rev)
		if err == nil || (!ambiguous && !errors.Is(err, coordination.ErrConflict)) {
			return st, err
		}
		prior = errors.Join(prior, err)
		replayed = ambiguous
	}
}

func planRetryRecoveryError(n int, id string, err error) error {
	return &IssueRecoveryError{
		Err:       fmt.Errorf("%w: %w; recover with swarm issue work %d --phase plan --recover-plan-retry %s", ErrPlanRetryRecovery, err, n, id),
		Operation: "work", IssueNumber: n, Arguments: []string{"--phase", "plan", "--recover-plan-retry", id},
	}
}

func loadPlanRetryRecord(checkout, repo string, n int, id string) (lease.PlanRetryOutcome, error) {
	var record planRetryRecord
	data, err := agent.LoadPlanRetryRecovery(checkout, repo, n, id)
	if err != nil {
		return record.Outcome, err
	}
	if err := json.Unmarshal(data, &record); err != nil || record.Checkout != checkout || record.Outcome.Reservation.ID != id ||
		record.Outcome.Reservation.ClaimID == "" || record.Outcome.Reservation.Binding == "" || record.Outcome.Owner == "" {
		return lease.PlanRetryOutcome{}, fmt.Errorf("private planner retry evidence does not match the managed checkout: %w", lease.ErrClaimFence)
	}
	digest, err := record.Outcome.Digest()
	if err != nil || digest != record.Digest {
		return lease.PlanRetryOutcome{}, fmt.Errorf("private planner retry evidence has invalid content: %w", lease.ErrClaimFence)
	}
	return record.Outcome, nil
}

func (e *Engine) guardPendingPlanRetry(ctx context.Context, opts WorkOptions) error {
	if opts.ClaimID == "" || (opts.Phase != "" && opts.Phase != "plan" && opts.Phase != "all") || !e.directGitHubPlanRetry(opts) {
		return nil
	}
	checkout, repo, err := e.planRetryLocation(opts.IssueNumber)
	if err != nil {
		return err
	}
	hasRecord, err := agent.HasPlanRetryRecovery(checkout, repo, opts.IssueNumber)
	if err != nil || !hasRecord {
		return err
	}
	if opts.Owner == "" {
		opts.Owner, err = e.Tracker.CurrentUser(ctx)
		if err != nil {
			return err
		}
	}
	e.LeaseManager.BindClaim(opts.IssueNumber, opts.ClaimID)
	st, _, err := e.LeaseManager.InspectPlanRetry(ctx, opts.IssueNumber, opts.Owner)
	if errors.Is(err, lease.ErrClaimFence) {
		return nil
	}
	if err != nil {
		return err
	}
	var ids []string
	if st.PlanRetryReservation != nil {
		ids = append(ids, st.PlanRetryReservation.ID)
	}
	if st.PlanRetryReceipt != nil {
		ids = append(ids, st.PlanRetryReceipt.ID)
	}
	for _, id := range ids {
		o, err := loadPlanRetryRecord(checkout, repo, opts.IssueNumber, id)
		if errors.Is(err, lease.ErrClaimFence) || errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err = lease.MatchPlanRetryOutcome(st, opts.Owner, o); err == nil {
			return planRetryRecoveryError(opts.IssueNumber, id, fmt.Errorf("a previous planner retry finished"))
		}
		if !errors.Is(err, lease.ErrClaimFence) {
			return err
		}
	}
	return nil
}

func (e *Engine) recoverPlanRetry(ctx context.Context, opts WorkOptions) (*WorkResult, error) {
	if !e.directGitHubPlanRetry(opts) || opts.Phase != "plan" || opts.Handoff || opts.PlanCheckpoint || opts.ManualClaim || opts.InspectOnly || opts.ExpectedHead != "" || opts.CandidateHead != "" {
		return nil, fmt.Errorf("planner retry recovery requires direct GitHub issue work with --phase plan")
	}
	if opts.ClaimID == "" {
		return nil, lease.ErrClaimFence
	}
	var err error
	if opts.Owner == "" {
		opts.Owner, err = e.Tracker.CurrentUser(ctx)
		if err != nil {
			return nil, err
		}
	}
	n := opts.IssueNumber
	e.LeaseManager.BindClaim(n, opts.ClaimID)
	st, _, err := e.LeaseManager.InspectPlanRetry(ctx, n, opts.Owner)
	if err != nil {
		return nil, err
	}
	checkout, repo, err := e.planRetryLocation(n)
	if err != nil {
		return nil, err
	}
	id := opts.RecoverPlanRetry
	o, err := loadPlanRetryRecord(checkout, repo, n, id)
	if err != nil {
		return nil, err
	}
	if _, err := lease.MatchPlanRetryOutcome(st, opts.Owner, o); err != nil {
		return nil, err
	}
	_, digests, _ := planRetryFiles(e.repoRoot, n)
	if digests[0] == "" || digests[1] == "" || digests != o.FileDigests {
		return nil, fmt.Errorf("retained planner files changed or cannot be verified: %w", lease.ErrClaimFence)
	}
	st, err = e.settlePlanRetry(ctx, opts, o)
	if err != nil {
		if errors.Is(err, lease.ErrClaimFence) {
			return nil, err
		}
		return nil, planRetryRecoveryError(n, id, errors.Join(recoveredPlannerFailure(o), err))
	}
	res := &WorkResult{IssueNumber: n, WorktreePath: checkout, BranchName: st.Branch, BaseSHA: st.BaseSHA, HeadSHA: st.HeadSHA, State: st}
	r := &workRun{e: e, ctx: ctx, coordinatorCtx: ctx, opts: opts, st: st, res: res}
	if o.Failure == "" {
		if _, err := r.finishRecordedPlan(); err != nil {
			return res, err
		}
	}
	if err := agent.RemovePlanRetryRecovery(checkout, repo, n, id); err != nil {
		return res, err
	}
	return res, recoveredPlannerFailure(o)
}

func recoveredPlannerFailure(o lease.PlanRetryOutcome) error {
	switch o.Failure {
	case "":
		return nil
	case "unchanged":
		return fmt.Errorf("recovered planner retry: %w", lease.ErrPlanUnchanged)
	case "invalid-output":
		return fmt.Errorf("recovered planner retry produced invalid or missing reports")
	case "timeout":
		return fmt.Errorf("recovered planner retry timed out")
	}
	if run, err := contract.ParseRoleRunTelemetry(o.Telemetry); err == nil {
		if cause := agent.FailureCause(run.FailureClass); cause != "" {
			return fmt.Errorf("recovered planner retry failed: %s", cause)
		}
	}
	return fmt.Errorf("recovered planner retry failed")
}
