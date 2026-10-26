package cli

import (
	"errors"
	"strconv"

	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/pr"
	"github.com/supernovae/swarmos/internal/workflow"
	"github.com/supernovae/swarmos/internal/workspace"
)

// Next-action reasons emitted by issue work, issue submit, pr work, pr rebase,
// and pr submit when they stop short of success.
const (
	ReasonChangesRequested = "changes_requested"
	ReasonNeedsApproval    = "needs_approval"
	ReasonConflict         = "conflict"
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
	err    error
	action *NextAction
}

func (e *stopError) Error() string { return e.err.Error() }
func (e *stopError) Unwrap() error { return e.err }

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
	case ReasonChangesRequested:
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
	var conflict *pr.ConflictError
	if errors.As(err, &conflict) {
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
	if o.Backend != "" {
		argv = append(argv, "--backend", o.Backend)
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
	if action := c.classify(err); action != nil {
		return &stopError{err: err, action: action}
	}
	return err
}
