package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/node"
	"github.com/supernovae/swarmos/internal/registry"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workflow"
	"github.com/supernovae/swarmos/internal/workspace"
)

var ErrNoReadyWork = errors.New("no approved ready work matches this node")

type PulledWorkUnit struct {
	Repo         string
	RepoRoot     string
	Backend      string
	Issue        *tracker.Issue
	WorkUnit     *contract.WorkUnit
	State        *lease.IssueState
	TargetPhase  string
	WorktreePath string
	BranchName   string
}

func PullNextReadyIssue(ctx context.Context, cmd *cobra.Command, rootOpts *RootOptions, allRepos bool, opts workflow.WorkOptions, durOverride time.Duration) (*PulledWorkUnit, error) {
	if err := contract.ValidatePhase(opts.Phase); err != nil {
		return nil, err
	}
	if err := contract.ValidateGate(opts.Gate); err != nil {
		return nil, err
	}
	targets, err := rootOpts.resolveTargets(allRepos)
	if err != nil {
		return nil, err
	}
	nodeProf, err := node.LoadOrDetectProfile("")
	if err != nil {
		return nil, err
	}
	for _, target := range targets {
		tr, mgr, cfg, err := rootOpts.resolveTargetLeaseManager(cmd, target)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", target.Repo, err)
		}
		mgr.SetNodeProfile(nodeProf)
		issues, err := tr.ListIssues(ctx, tracker.IssueFilter{State: "open", Labels: []string{cfg.Security.ReadyLabel}})
		if err != nil {
			return nil, err
		}
		owner, err := tr.CurrentUser(ctx)
		if err != nil || owner == "" {
			return nil, fmt.Errorf("resolve worker identity: %w", err)
		}
		engine := workflow.NewEngineWithManager(target.Path, tr, mgr, cfg, rootOpts.Fork, rootOpts.effectiveForkRemote())
		for _, issue := range issues {
			if issue.IsEpic() || slices.Contains(issue.Labels, "swarm:pr-open") {
				continue
			}
			wu, err := contract.ParseWorkUnitFromIssue(issue.Number, issue.Title, issue.Body)
			if err != nil {
				continue
			}
			state, err := mgr.GetCurrentState(ctx, issue.Number)
			if errors.Is(err, coordination.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if state == nil || state.TaskDigest != contract.DigestWorkUnit(wu) || state.Lease.Status == "completed" ||
				state.Lease.Status == "active" && state.Lease.ExpiresAt.After(rootOpts.now()) {
				continue
			}
			if err := mgr.VerifyDependencies(ctx, wu); err != nil {
				if errors.Is(err, lease.ErrDependenciesUnmet) {
					continue
				}
				return nil, err
			}
			if !node.MatchesCapabilities(nodeProf, wu.RequiredCapabilities) {
				continue
			}
			phase := opts.Phase
			if phase == "" {
				phase = "all"
			}
			if phase == "all" && state.Lease.Status == "handoff" && state.Lease.NextPhase != "" {
				phase = state.Lease.NextPhase
			}
			if state.EffectiveGate == contract.GateHuman && phase != "plan" {
				hasPlan := false
				for _, artifact := range state.Artifacts {
					hasPlan = hasPlan || artifact.Type == contract.ArtifactDesign
				}
				if hasPlan {
					if err := lease.VerifyApproval(state, wu); err != nil {
						continue // A human decision, not another worker, unblocks this task.
					}
					if phase == "review" {
						phase = "implement"
					}
				}
			}
			if !node.SupportsPhaseCapability(nodeProf, phase) {
				continue
			}
			selection := opts
			selection.RepoRoot, selection.IssueNumber, selection.Phase = target.Path, issue.Number, phase
			preflight, err := engine.Preflight(ctx, selection)
			if err != nil {
				return nil, err
			}
			duration := durOverride
			if duration <= 0 {
				duration, err = time.ParseDuration(preflight.Config.Lease.Duration)
				if err != nil {
					return nil, err
				}
			}
			lock, err := lease.LockClaim(target.Path, issue.Number)
			if err != nil {
				continue
			}
			role := "planner"
			if phase == "review" {
				role = "reviewer"
			}
			if phase == "implement" {
				role = "developer"
			}
			claimed, err := mgr.Acquire(ctx, issue.Number, owner, preflight.Agents[role], phase, duration, preflight.Config.Lease.MaxRenewals)
			if err != nil {
				lease.UnlockClaim(lock)
				if errors.Is(err, lease.ErrAlreadyClaimed) || errors.Is(err, lease.ErrClaimRaceLost) {
					continue
				}
				return nil, err
			}
			setup := func() (*PulledWorkUnit, error) {
				if err := lease.SaveClaimToken(target.Path, issue.Number, claimed.Lease.ClaimID); err != nil {
					return nil, err
				}
				checkpointRemote := "origin"
				targetBranch := claimed.Branch
				if rootOpts.Fork {
					checkpointRemote = rootOpts.effectiveForkRemote()
					if targetBranch == "" {
						targetBranch = workspace.ForkBranchName(issue.Number, claimed.Lease.ClaimID)
					}
				}
				worktree, branch, err := workspace.ProvisionFromRemoteAt(target.Path, checkpointRemote, issue.Number, issue.Title, claimed.BaseSHA, targetBranch, claimed.HeadSHA)
				if err != nil {
					return nil, err
				}
				if err := workspace.CompileHarnessBundleAt(worktree, wu, preflight.Workflow, claimed.Artifacts, role, claimed.BaseSHA); err != nil {
					return nil, err
				}
				return &PulledWorkUnit{Repo: target.Repo, RepoRoot: target.Path, Backend: target.Backend, Issue: issue,
					WorkUnit: wu, State: claimed, TargetPhase: phase, WorktreePath: worktree, BranchName: branch}, nil
			}
			pulled, err := setup()
			if err != nil {
				_, releaseErr := mgr.Release(ctx, issue.Number, owner, "claim setup failed")
				lease.UnlockClaim(lock)
				return nil, errors.Join(err, releaseErr)
			}
			lease.UnlockClaim(lock)
			return pulled, nil
		}
	}
	return nil, ErrNoReadyWork
}

func (o *RootOptions) resolveTargets(allRepos bool) ([]registry.RepoEntry, error) {
	if allRepos {
		reg := registry.New("")
		entries, err := reg.List()
		if err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			return nil, fmt.Errorf("no repositories registered (run `swarm repo add` first)")
		}
		return entries, nil
	}

	root := o.resolveRepoRoot()
	slug := o.Repo
	if slug == "" {
		slug = registry.DetectGitHubSlug(root)
		if slug == "" {
			slug = "local/repo"
		}
	}
	return []registry.RepoEntry{
		{
			Repo:    slug,
			Path:    root,
			Backend: o.Backend,
		},
	}, nil
}
