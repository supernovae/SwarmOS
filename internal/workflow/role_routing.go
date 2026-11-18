package workflow

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/workspace"
)

type RoleRoutingChange struct {
	Approved RolePreview `json:"approved"`
	Target   RolePreview `json:"target"`
}

type RoleRoutingComparison struct {
	Branch      string              `json:"branch,omitempty"`
	ApprovedSHA string              `json:"approved_sha"`
	TargetSHA   string              `json:"target_sha,omitempty"`
	Reason      string              `json:"reason"`
	Unavailable bool                `json:"unavailable,omitempty"`
	Changes     []RoleRoutingChange `json:"changes,omitempty"`
	Guidance    string              `json:"guidance,omitempty"`
}

func (e *Engine) compareRoleRouting(ctx context.Context, opts WorkOptions, pre *PreflightResult, state *lease.IssueState) *RoleRoutingComparison {
	if !opts.CompareRoleRouting || runtime.GOOS != "linux" || e.Tracker.BackendName() != "github" ||
		pre.Execution.Mode != contract.ExecutionIsolated || pre.Execution.Backend != contract.BackendNative ||
		opts.ForkMode || e.ForkMode || state.ForkRepo != "" || e.LeaseManager.HasWorkerGateway() ||
		opts.PRNumber != 0 || state.PRRevision != nil || opts.PlanCheckpoint {
		return nil
	}

	var selected []RolePreview
	for _, role := range [...]string{"planner", "reviewer", "developer", "tester"} {
		if name := pre.Agents[role]; name == "" || name == "interactive" {
			continue
		}
		row := SelectedRolePreview(pre.Config, role, opts)
		if row.Harness == "codex" || row.Harness == "opencode" {
			selected = append(selected, row)
		}
	}
	if len(selected) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	branch, sha, err := workspace.ResolveTrustedTargetBranch(ctx, opts.RepoRoot, e.upstreamRemote(), state.BaseBranch)
	result := &RoleRoutingComparison{Branch: branch, ApprovedSHA: state.BaseSHA, TargetSHA: sha}
	if err != nil {
		result.Unavailable, result.Reason = true, "trusted target branch could not be resolved"
		return result
	}
	if sha == state.BaseSHA {
		return nil
	}
	cfg, err := harness.LoadConfigAtContext(ctx, opts.RepoRoot, sha)
	if err != nil {
		result.Unavailable, result.Reason = true, "trusted target branch config could not be loaded"
		return result
	}
	for _, approved := range selected {
		target := SelectedRolePreview(cfg, approved.Role, opts)
		if target.Harness == "" {
			result.Changes = nil
			result.Unavailable, result.Reason = true, "target role routing could not be resolved"
			return result
		}
		// Source explains the selection and can change without changing the route.
		if approved.Entry != target.Entry || approved.Harness != target.Harness ||
			approved.Model != target.Model || approved.Effort != target.Effort {
			result.Changes = append(result.Changes, RoleRoutingChange{Approved: approved, Target: target})
		}
	}
	if len(result.Changes) == 0 {
		return nil
	}
	result.Reason = "effective role routing differs from the approved policy"
	result.Guidance = fmt.Sprintf("After any active claim is released, run swarm issue approve %d to adopt the target branch policy.", opts.IssueNumber)
	return result
}
