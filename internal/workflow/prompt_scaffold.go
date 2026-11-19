package workflow

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
)

type PromptScaffoldReport struct {
	Checkout     harness.PromptScaffoldAssessment `json:"checkout"`
	Approved     harness.PromptScaffoldAssessment `json:"approved"`
	ApprovedSHA  string                           `json:"approved_sha"`
	TargetBranch string                           `json:"target_branch,omitempty"`
	NextActions  []string                         `json:"next_actions,omitempty"`
}

func (e *Engine) inspectPromptScaffold(ctx context.Context, opts WorkOptions, pre *PreflightResult, state *lease.IssueState) *PromptScaffoldReport {
	if !opts.AssessPromptScaffold || runtime.GOOS != "linux" ||
		pre.Execution.Mode != contract.ExecutionIsolated || pre.Execution.Backend != contract.BackendNative ||
		opts.ForkMode || e.ForkMode || state.ForkRepo != "" || e.LeaseManager.HasWorkerGateway() ||
		opts.PRNumber != 0 || state.PRRevision != nil || opts.PlanCheckpoint {
		return nil
	}
	if _, candidate, present, _ := lease.CheckpointPlanHeads(state); present && state.HeadSHA != candidate && opts.Phase == "review" {
		return nil
	}
	for _, role := range [...]string{"planner", "reviewer", "developer", "tester"} {
		if name := pre.Agents[role]; name == "" || name == "interactive" {
			continue
		}
		switch SelectedRolePreview(pre.Config, role, opts).Harness {
		case "codex", "claude":
			report := &PromptScaffoldReport{Checkout: harness.AssessPromptScaffold(opts.RepoRoot)}
			report.readApproved(ctx, opts, state)
			return report
		}
	}
	return nil
}

func (r *PromptScaffoldReport) readApproved(ctx context.Context, opts WorkOptions, state *lease.IssueState) {
	r.ApprovedSHA = state.BaseSHA
	r.TargetBranch = state.BaseBranch
	r.Approved = harness.AssessPromptScaffoldAt(ctx, opts.RepoRoot, state.BaseSHA)
	r.setActions(opts, state)
}

func (r *PromptScaffoldReport) setActions(opts WorkOptions, state *lease.IssueState) {
	r.NextActions = r.Checkout.CheckoutActions()
	if r.Approved.Status == "current" {
		return
	}
	if r.Approved.Status == "unverifiable" {
		r.NextActions = append(r.NextActions, "Inspect the flagged prompts and managed metadata at the approved commit.")
	}
	target := "the target branch"
	if r.TargetBranch != "" {
		target = fmt.Sprintf("target branch %q", r.TargetBranch)
	}
	r.NextActions = append(r.NextActions, "Review the scaffold upgrade and make sure it is committed to "+target+".")
	now := opts.InspectionTime
	if now.IsZero() {
		now = time.Now()
	}
	if state.Lease.Status == "active" && state.Lease.ExpiresAt.After(now) {
		r.NextActions = append(r.NextActions, fmt.Sprintf("Have the active claim owner run swarm issue release %d before re-approval.", opts.IssueNumber))
	}
	r.NextActions = append(r.NextActions, fmt.Sprintf("After the upgrade is committed, have a maintainer run swarm issue approve %d.", opts.IssueNumber))
}
