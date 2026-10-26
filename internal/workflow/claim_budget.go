package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/supernovae/swarmos/internal/claimpause"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/workspace"
)

// ClaimBudgetPause is a successful handoff, with no completion verdict.
type ClaimBudgetPause struct {
	Budget          *lease.ClaimBudget `json:"budget"`
	Reason          string             `json:"reason"`
	ContinueCommand string             `json:"continue_command"`
}

type budgetAdmissionError struct {
	budget      *lease.ClaimBudget
	state       *lease.IssueState
	issueNumber int
	prNumber    int
	phase       string
}

func (e *budgetAdmissionError) Error() string {
	resource, number := "issue", e.issueNumber
	if e.prNumber != 0 {
		resource, number = "pr", e.prNumber
	}
	return fmt.Sprintf("%s. Continue: swarm %s work %d --phase %s", e.budgetReason(), resource, number, e.phase)
}
func (e *budgetAdmissionError) budgetReason() string {
	return fmt.Sprintf("paused for claim budget: %s available (reserve %s), %d renewals used, %d remaining", e.budget.Available, e.budget.Threshold, e.budget.RenewalsUsed, e.budget.RenewalsRemaining)
}

func (e *Engine) admitStage(ctx context.Context, issue int, owner, phase string) error {
	paused := context.Cause(ctx) == claimpause.ErrBudget
	if paused {
		cleanup, cancel := context.WithTimeout(claimpause.CoordinatorContext(ctx), 30*time.Second)
		defer cancel()
		ctx = cleanup
	}
	if err := e.LeaseManager.CheckClaim(ctx, issue, owner); err != nil {
		return err
	}
	st, err := e.LeaseManager.GetCurrentState(ctx, issue)
	if err != nil {
		return err
	}
	b, err := e.LeaseManager.Budget(st)
	if err != nil {
		return err
	}
	if !paused && (b == nil || b.Available >= b.Threshold) {
		return nil
	}
	st, err = e.LeaseManager.Handoff(ctx, issue, owner, phase, phase, "claim budget")
	if err != nil {
		return err
	}
	return &budgetAdmissionError{budget: b, state: st, issueNumber: issue, phase: phase}
}

func setBudgetPause(res *WorkResult, b *lease.ClaimBudget, phase string, opts WorkOptions) {
	resource, number := "issue", opts.IssueNumber
	if opts.PRNumber != 0 {
		resource, number = "pr", opts.PRNumber
	}
	res.ClaimBudgetPause = &ClaimBudgetPause{Budget: b, Reason: (&budgetAdmissionError{budget: b}).budgetReason(), ContinueCommand: fmt.Sprintf("swarm %s work %d --phase %s", resource, number, phase)}
	res.HandedOffToPhase = phase
	res.ValidationPassed = false
}

func budgetInterrupted(ctx context.Context, err error) bool {
	var preservation *claimpause.PreservationError
	if errors.As(err, &preservation) {
		return false
	}
	return context.Cause(ctx) == claimpause.ErrBudget && (err == nil || errors.Is(err, context.Canceled) || errors.Is(err, claimpause.ErrBudget))
}

func (e *Engine) pauseWork(parent context.Context, res *WorkResult, opts WorkOptions, wu *contract.WorkUnit, phase string, checkpoint, forkMode bool, forkRemote string) error {
	cleanup, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	if err := e.LeaseManager.CheckClaim(cleanup, opts.IssueNumber, opts.Owner); err != nil {
		return err
	}
	st, err := e.LeaseManager.GetCurrentState(cleanup, opts.IssueNumber)
	if err != nil {
		return err
	}
	b, err := e.LeaseManager.Budget(st)
	if err != nil {
		return err
	}
	if checkpoint {
		head, err := workspace.Checkpoint(cleanup, res.WorktreePath, res.BranchName, workspace.CheckpointOptions{WorkUnit: wu, Purpose: "implement", Detail: e.Config.CommitMessageDetail()})
		if err != nil {
			return fmt.Errorf("claim-budget checkpoint: %w", err)
		}
		if opts.PRNumber != 0 {
			st, err = e.LeaseManager.RecordPRHead(cleanup, opts.IssueNumber, opts.Owner, opts.PRNumber, res.BranchName, head)
		} else {
			if forkMode {
				if err = e.LeaseManager.CheckClaim(cleanup, opts.IssueNumber, opts.Owner); err != nil {
					return err
				}
				if err = workspace.PushForkCheckpoint(cleanup, opts.RepoRoot, forkRemote, res.BranchName, head); err != nil {
					return err
				}
			}
			st, err = e.LeaseManager.RecordCheckpoint(cleanup, opts.IssueNumber, opts.Owner, res.BranchName, head)
		}
		if err != nil {
			return fmt.Errorf("publish claim-budget checkpoint: %w", err)
		}
		res.State, res.HeadSHA = st, head
	}
	st, err = e.LeaseManager.Handoff(cleanup, opts.IssueNumber, opts.Owner, phase, phase, "claim budget")
	if err != nil {
		return err
	}
	res.State = st
	setBudgetPause(res, b, phase, opts)
	return nil
}

func (e *Engine) pauseAdmission(parent context.Context, opts WorkOptions, phase string) error {
	st, err := e.LeaseManager.GetCurrentState(parent, opts.IssueNumber)
	if err != nil {
		return err
	}
	b, err := e.LeaseManager.Budget(st)
	if err != nil {
		return err
	}
	st, err = e.LeaseManager.Handoff(parent, opts.IssueNumber, opts.Owner, phase, phase, "claim budget")
	if err != nil {
		return err
	}
	return &budgetAdmissionError{budget: b, state: st, issueNumber: opts.IssueNumber, prNumber: opts.PRNumber, phase: phase}
}

func implementationProven(st *lease.IssueState, head string) bool {
	if st.NextPhaseReason != "claim budget" || head == "" {
		return false
	}
	for _, a := range st.Artifacts {
		if a.Type != contract.ArtifactImplementation || a.Verdict != "PASS" || a.Metadata["head_sha"] != head {
			continue
		}
		valid := true
		for k, v := range lease.ReviewBindings(st) {
			if k == "review_claim_id" {
				continue
			}
			if v == "" || a.Metadata[k] != v {
				valid = false
			}
		}
		if valid {
			return true
		}
	}
	return false
}
