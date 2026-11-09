package cli

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/pr"
	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workflow"
	"github.com/supernovae/swarmos/internal/workspace"
)

// Next-action reasons emitted by issue work, issue submit, pr work, pr rebase,
// and pr submit when they stop short of success.
const (
	ReasonPartialImplementation = "partial_implementation"
	ReasonChangesRequested      = "changes_requested"
	ReasonPlanNoProgress        = "plan_no_progress"
	ReasonNeedsApproval         = "needs_approval"
	ReasonConflict              = "conflict"
	// ReasonPreExistingFailures stops a run whose failing tests all fail at
	// the approved base; a maintainer inspects the status and decides.
	ReasonPreExistingFailures = "pre_existing_failures"
	// ReasonValidationRepairExhausted and ReasonValidationRepairNoChanges stop
	// automatic validation repair; another implement run may continue it.
	ReasonValidationRepairExhausted = "validation_repair_exhausted"
	ReasonValidationRepairNoChanges = "validation_repair_no_changes"
)

// Process exit codes. 2 is left unused so it stays free for usage errors.
const (
	ExitOK               = 0
	ExitFailure          = 1
	ExitChangesRequested = 3
	ExitNeedsApproval    = 4
	ExitConflict         = 5
)

// NextAction is advisory recovery output. It never grants authority: the
// command it names still passes every approval, claim, and policy check.
type NextAction struct {
	Reason        string   `json:"reason"`
	Argv          []string `json:"argv"`
	HumanRequired bool     `json:"human_required"`
}

// stopError carries a classified stop to the process exit code.
type stopError struct {
	err      error
	action   *NextAction
	guidance string
}

func (e *stopError) Error() string { return e.err.Error() + e.guidance + renderNextAction(e.action) }
func (e *stopError) Unwrap() error { return e.err }

// approvalRefusalError adds advice while preserving the ordinary refusal exit.
type approvalRefusalError struct {
	err    error
	action *NextAction
}

func (e *approvalRefusalError) Error() string { return e.err.Error() + renderNextAction(e.action) }
func (e *approvalRefusalError) Unwrap() error { return e.err }

// ExitCode maps a command error to the process exit status.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var stop *stopError
	if !errors.As(err, &stop) || stop.action == nil {
		return ExitFailure
	}
	switch stop.action.Reason {
	case ReasonChangesRequested, ReasonPlanNoProgress, ReasonValidationRepairExhausted, ReasonValidationRepairNoChanges:
		return ExitChangesRequested
	case ReasonNeedsApproval:
		return ExitNeedsApproval
	case ReasonConflict:
		return ExitConflict
	}
	return ExitFailure
}

func nextActionOf(err error) *NextAction {
	var stop *stopError
	if errors.As(err, &stop) {
		return stop.action
	}
	var noReady *noReadyError
	if errors.As(err, &noReady) {
		return noReady.action
	}
	var refusal *approvalRefusalError
	if errors.As(err, &refusal) {
		return refusal.action
	}
	return nil
}

// stopContext names the command that stopped so recovery argv targets the
// same issue or PR with the same repository selection.
type stopContext struct {
	opts     *RootOptions
	resource string // "issue" or "pr"
	number   int    // the issue or PR number the command ran on
}

// classify returns the recovery for a typed stop, or nil when err needs no
// next step. A paused human gate is a successful run and is not a stop. A
// reviewer rejection outranks an approval stop, and task approval outranks
// plan approval.
func (c stopContext) classify(err error) *NextAction {
	if err == nil {
		return nil
	}
	var partial *workflow.PartialImplementationError
	if c.resource == "issue" && (c.opts == nil || !c.opts.Fork) && errors.As(err, &partial) {
		number := c.number
		if number <= 0 {
			number = partial.IssueNumber
		}
		if number > 0 {
			return &NextAction{Reason: ReasonPartialImplementation, Argv: c.argv(false, "issue", "work", strconv.Itoa(number), "--phase", "plan")}
		}
	}
	if c.number <= 0 {
		var recovery *workflow.IssueRecoveryError
		var task *lease.TaskApprovalError
		var base *lease.ClaimBaseApprovalError
		var plan *lease.HumanPlanApprovalError
		var trusted *lease.PlanApprovalError
		var conflict *pr.ConflictError
		if !errors.As(err, &recovery) && !errors.As(err, &task) && !errors.As(err, &base) && !errors.As(err, &plan) && !errors.As(err, &trusted) && !errors.As(err, &conflict) {
			return nil
		}
	}
	var recovery *workflow.IssueRecoveryError
	if errors.As(err, &recovery) {
		reason := ReasonChangesRequested
		if recovery.Human {
			reason = ReasonNeedsApproval
		}
		if errors.Is(err, workflow.ErrPreExistingFailures) {
			reason = ReasonPreExistingFailures
		}
		number := c.number
		if number == 0 {
			number = recovery.IssueNumber
		}
		if number <= 0 {
			return nil
		}
		args := []string{"issue", recovery.Operation, strconv.Itoa(number)}
		args = append(args, recovery.Arguments...)
		return &NextAction{Reason: reason, Argv: c.argv(recovery.Human, args[0], args[1:]...), HumanRequired: recovery.Human}
	}
	var retry *workflow.TestRetryAvailableError
	if errors.As(err, &retry) && (c.opts == nil || !c.opts.Fork) {
		return &NextAction{Reason: "tester_retry_available", Argv: c.argv(false, c.resource, "work", strconv.Itoa(c.number), "--phase", "test")}
	}
	if errors.Is(err, workflow.ErrPreExistingFailures) {
		return &NextAction{Reason: ReasonPreExistingFailures, Argv: c.argv(false, c.resource, "status", strconv.Itoa(c.number)), HumanRequired: true}
	}
	if errors.Is(err, workflow.ErrValidationRepairExhausted) {
		return &NextAction{Reason: ReasonValidationRepairExhausted, Argv: c.argv(false, c.resource, "work", strconv.Itoa(c.number), "--phase", "implement")}
	}
	if errors.Is(err, workflow.ErrValidationRepairNoChanges) {
		return &NextAction{Reason: ReasonValidationRepairNoChanges, Argv: c.argv(false, c.resource, "work", strconv.Itoa(c.number), "--phase", "implement")}
	}
	if errors.Is(err, lease.ErrPlanUnchanged) {
		return &NextAction{Reason: ReasonPlanNoProgress, Argv: c.argv(false, c.resource, "work", strconv.Itoa(c.number), "--phase", "plan")}
	}
	if errors.Is(err, workflow.ErrPlanReviewRejected) {
		return &NextAction{Reason: ReasonChangesRequested, Argv: c.argv(false, c.resource, "work", strconv.Itoa(c.number), "--phase", "plan")}
	}
	var task *lease.TaskApprovalError
	var base *lease.ClaimBaseApprovalError
	switch {
	case errors.As(err, &task):
		return c.taskApproval(task.IssueNumber)
	case errors.As(err, &base):
		return c.taskApproval(base.IssueNumber)
	}
	var plan *lease.HumanPlanApprovalError
	if errors.As(err, &plan) {
		return c.planApproval(plan.IssueNumber)
	}
	var trusted *lease.PlanApprovalError
	if c.opts != nil && c.opts.Fork && errors.As(err, &trusted) {
		return c.planApproval(trusted.IssueNumber)
	}
	var conflict *pr.ConflictError
	if errors.As(err, &conflict) {
		if conflict.PRNumber <= 0 {
			return nil
		}
		number := strconv.Itoa(conflict.PRNumber)
		switch {
		case conflict.Manual:
			return &NextAction{Reason: ReasonConflict, Argv: c.argv(false, "pr", "rebase", number, "--continue"), HumanRequired: true}
		case conflict.Paused:
			return &NextAction{Reason: ReasonConflict, Argv: c.argv(false, "pr", "rebase", number, "--continue", "--resolve")}
		}
		return &NextAction{Reason: ReasonConflict, Argv: c.argv(false, "pr", "rebase", number, "--resolve")}
	}
	return nil
}

func (c stopContext) taskApproval(issue int) *NextAction {
	if issue <= 0 {
		return nil
	}
	return &NextAction{Reason: ReasonNeedsApproval, Argv: c.argv(true, "issue", "approve", strconv.Itoa(issue)), HumanRequired: true}
}

func (c stopContext) planApproval(issue int) *NextAction {
	if issue <= 0 {
		return nil
	}
	return &NextAction{Reason: ReasonNeedsApproval, Argv: c.argv(true, "issue", "review-plan", strconv.Itoa(issue), "--approve"), HumanRequired: true}
}

// argv builds a literal command line from trusted options only. Maintainer
// commands run upstream, so they drop the worker's fork flags and local root.
func (c stopContext) argv(maintainer bool, resource string, args ...string) []string {
	argv := []string{"swarm"}
	o := c.opts
	if o == nil {
		o = &RootOptions{}
	}
	if o.Repo != "" {
		argv = append(argv, "--repo", o.Repo)
	}
	if o.RepoRoot != "" && !(maintainer && o.Fork) {
		argv = append(argv, "--root", o.RepoRoot)
	}
	if remote := o.UpstreamRemote; remote != "" && !(maintainer && o.Fork) {
		argv = append(argv, "--upstream-remote", remote)
	}
	if !maintainer {
		if o.Fork {
			argv = append(argv, "--fork")
		}
		if o.ForkRemote != "" && (o.Fork || resource == "pr" && o.ForkRemote != workspace.DefaultForkRemote) {
			argv = append(argv, "--fork-remote", o.ForkRemote)
		}
	}
	return append(append(argv, resource), args...)
}

// stop wraps err with its recovery so ExitCode can report it. Unclassified
// errors are returned unchanged.
func (c stopContext) stop(err error) error {
	var existing *stopError
	if errors.As(err, &existing) {
		return err
	}
	var noReady *noReadyError
	if errors.As(err, &noReady) && noReady.action != nil {
		return &stopError{err: noReady, action: noReady.action, guidance: noReady.guidance}
	}
	if action := c.classify(err); action != nil {
		stop := &stopError{err: err, action: action}
		if c.opts != nil && c.opts.Fork && action.HumanRequired && isMaintainerAction(action) {
			stop.guidance = "\nAsk an upstream maintainer; contributor review is advisory."
			if c.opts.Repo == "" {
				stop.guidance += "\nRun this command from the upstream repository checkout."
			}
			stop.guidance += "\nResume worker commands with --fork after approval."
		}
		return stop
	}
	return err
}

func actionOperation(a *NextAction) (string, string, []string) {
	if a == nil {
		return "", "", nil
	}
	for i := 1; i < len(a.Argv); {
		if a.Argv[i] == "--fork" {
			i++
			continue
		}
		if strings.HasPrefix(a.Argv[i], "--") {
			i += 2
			continue
		}
		if i+1 < len(a.Argv) {
			return a.Argv[i], a.Argv[i+1], a.Argv[i+2:]
		}
		break
	}
	return "", "", nil
}
func isMaintainerAction(a *NextAction) bool {
	resource, operation, _ := actionOperation(a)
	return resource == "issue" && (operation == "approve" || operation == "review-plan" || operation == "edit")
}

func renderNextAction(a *NextAction) string {
	if a == nil {
		return ""
	}
	guidance := ""
	if a.Reason == ReasonPartialImplementation {
		_, _, args := actionOperation(a)
		if len(args) > 0 {
			guidance = "\nOr edit the WorkUnit with swarm issue edit " + args[0] + "."
		}
	}
	resource, operation, _ := actionOperation(a)
	if resource == "issue" && operation == "release" {
		guidance = "\nThe active claim owner must run the next step."
	}
	if isMaintainerAction(a) {
		guidance = "\nAsk an authorized maintainer to run the next step."
		_, operation, args := actionOperation(a)
		if operation == "review-plan" && slices.Contains(args, "--approve") {
			guidance = "\nAsk an independent maintainer to review the current plan."
		}
	}
	return guidance + "\nNext step: " + formatCommand(a.Argv)
}

func formatCommand(argv []string) string {
	args := make([]string, len(argv))
	for i, arg := range argv {
		if arg != "" && !strings.ContainsAny(arg, " \t\n\r'\"`$\\;|&<>()*?[]{}!#~") {
			args[i] = arg
		} else {
			args[i] = "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
		}
	}
	return strings.Join(args, " ")
}

// messageOnly removes only presentation contributed by stop wrappers.
func messageOnly(err error) string {
	if err == nil {
		return ""
	}
	if refusal, ok := err.(*approvalRefusalError); ok {
		return messageOnly(refusal.err)
	}
	if noReady, ok := err.(*noReadyError); ok {
		return noReady.text
	}
	if stop, ok := err.(*stopError); ok {
		return messageOnly(stop.err)
	}
	var refusal *approvalRefusalError
	if errors.As(err, &refusal) {
		return strings.Replace(err.Error(), refusal.Error(), messageOnly(refusal.err), 1)
	}
	var stop *stopError
	if errors.As(err, &stop) {
		return strings.Replace(err.Error(), stop.Error(), messageOnly(stop.err), 1)
	}
	return err.Error()
}

func needsHumanPlanDecision(st *lease.IssueState, wu *contract.WorkUnit, cfg *contract.SwarmConfig) bool {
	return st != nil && st.EffectiveGate == contract.GateHuman && lease.TaskApprovalCurrent(st, wu, cfg) && lease.CompletePlanPair(st) && ((st.PendingPlanReview == nil && st.HumanPlanReview == nil) || lease.HasRevisedPlan(st)) && (st.Lease.Status != "completed" || st.PRNumber != 0) && lease.VerifyHumanPlanApproval(st) != nil
}

// statusAction uses the same typed recovery classifications as execution.
// Submitted state and approval blockers outrank advisory recorded readiness.
func (c stopContext) statusAction(st *lease.IssueState, wu *contract.WorkUnit, cfg *contract.SwarmConfig, human *NextAction, readiness *validation.Readiness, owner, token string, now time.Time) *NextAction {
	n := strconv.Itoa(c.number)
	action := func(reason, op string, args ...string) *NextAction {
		return &NextAction{Reason: reason, Argv: c.argv(false, "issue", append([]string{op, n}, args...)...)}
	}
	if st.PRNumber != 0 || st.Lease.Status == "completed" {
		return action("submitted", "status")
	}
	if !lease.TaskApprovalCurrent(st, wu, cfg) {
		return c.taskApproval(c.number)
	}
	if human != nil {
		return human
	}
	if st.Lease.Status == "active" && st.Lease.ExpiresAt.After(now) && (owner == "" || owner != st.Lease.Owner || token == "" || token != st.Lease.ClaimID) {
		a := action("claim_unavailable", "status")
		a.HumanRequired = true
		return a
	}
	if err := lease.VerifyApproval(st, wu); err != nil {
		if a := c.classify(err); a != nil {
			return a
		}
		if _, rejected := lease.SupersededReview(st); rejected {
			return action("plan_review_required", "work", "--phase", "plan")
		}
		return action("plan_review_required", "work")
	}
	if c.resource == "issue" && (c.opts == nil || !c.opts.Fork) && currentPartialImplementation(st) {
		return action(ReasonPartialImplementation, "work", "--phase", "plan")
	}
	if repair := validation.RepairFromArtifact(st.Artifacts, st.BaseSHA); recordedValidationFailed(st) && repair != nil && repair.HeadSHA == st.HeadSHA {
		if repair.AllPreExisting || repair.OutsideCandidate() {
			return c.classify(workflow.ErrPreExistingFailures)
		}
		return c.classify(workflow.ErrValidationRepairExhausted)
	}
	if recordedValidationFailed(st) {
		return c.classify(workflow.ErrValidationRepairExhausted)
	}
	live := st.Lease.Status == "active" && st.Lease.ExpiresAt.After(now)
	if live && readiness != nil && readiness.RequiredChecks == "passed" && readiness.AcceptanceEvidence.State == "complete" {
		return action("ready_to_submit", "submit")
	}
	if live && !recordedValidationFailed(st) && workflow.RecordedImplementation(st) != nil && readiness != nil && readiness.AcceptanceEvidence.State != "complete" {
		return action("tester_retry_available", "work", "--phase", "test")
	}
	return action("resumable_work", "work")
}

// recordedValidationFailed considers the latest coordinator report for this
// checkpoint; stale failures cannot outrank the current continuation.
func recordedValidationFailed(st *lease.IssueState) bool {
	for i := len(st.Artifacts) - 1; i >= 0; i-- {
		a := st.Artifacts[i]
		if a.Type != contract.ArtifactValidation {
			continue
		}
		return a.Verdict == "FAIL" && a.Metadata["validation_trusted_origin"] == "coordinator" && a.Metadata["head_sha"] == st.HeadSHA && a.Metadata["base_sha"] == st.BaseSHA
	}
	return false
}

// Only the latest implementation can supply current partial recovery advice.
func currentPartialImplementation(st *lease.IssueState) bool {
	if st == nil || st.PRNumber != 0 || st.PRRevision != nil {
		return false
	}
	for i := len(st.Artifacts) - 1; i >= 0; i-- {
		a := st.Artifacts[i]
		if a.Type != contract.ArtifactImplementation {
			continue
		}
		if a.Verdict != "FAIL" || a.Metadata["implementation_outcome"] != "partial" || st.HeadSHA == "" || a.Metadata["head_sha"] != st.HeadSHA {
			return false
		}
		for k, v := range lease.ReviewBindings(st) {
			if k == "review_claim_id" {
				continue
			}
			if v == "" || a.Metadata[k] != v {
				return false
			}
		}
		return true
	}
	return false
}
