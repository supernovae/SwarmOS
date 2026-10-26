package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"time"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/node"
	"github.com/supernovae/swarmos/internal/registry"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workflow"
	"github.com/supernovae/swarmos/internal/workspace"
)

var errNoReadyWork = errors.New("no approved ready work matches this node")

type pulledWorkUnit struct {
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

func pullNextReadyIssue(ctx context.Context, cmd *cobra.Command, rootOpts *RootOptions, allRepos bool, opts workflow.WorkOptions, durOverride time.Duration) (*pulledWorkUnit, error) {
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
		issues, err = orderReadyIssues(ctx, tr, issues, cmd.ErrOrStderr())
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
			if state == nil || state.Lease.Status == "completed" ||
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
			if phase == "all" || phase == "plan" {
				if err := mgr.CheckPlanReplan(state); err != nil {
					if errors.Is(err, lease.ErrPlanReplanLimit) {
						continue
					}
					return nil, err
				}
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
			preflight, err := engine.PreflightForClaim(ctx, selection)
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
			role := workflow.LeadRole(phase)
			var claimed *lease.IssueState
			err = opts.SetupProgress.ReportSetupPhase(ctx, issue.Number, "claim", func() error {
				claimed, err = mgr.Acquire(ctx, issue.Number, owner, preflight.Agents[role], phase, duration, preflight.Config.Lease.MaxRenewals)
				return err
			})
			if err != nil {
				lease.UnlockClaim(lock)
				var refusal *lease.ClaimBaseApprovalError
				if errors.As(err, &refusal) {
					fmt.Fprintln(progressWriter(cmd), refusal)
					continue
				}
				if errors.Is(err, lease.ErrApprovalRequired) {
					fmt.Fprintf(progressWriter(cmd), "Issue #%d: %v\n", issue.Number, err)
					continue
				}
				if errors.Is(err, lease.ErrAlreadyClaimed) || errors.Is(err, lease.ErrClaimRaceLost) {
					continue
				}
				return nil, err
			}
			if claimed.ClaimPreviousBase != "" {
				fmt.Fprintf(progressWriter(cmd), "Issue #%d: base %s -> %s\n", issue.Number, claimed.ClaimPreviousBase, claimed.BaseSHA)
			}
			setup := func() (*pulledWorkUnit, error) {
				if err := lease.SaveClaimToken(target.Path, issue.Number, claimed.Lease.ClaimID, claimed.BaseSHA); err != nil {
					return nil, err
				}
				checkpointRemote, targetBranch := workspace.ClaimCheckpointFrom(mgr.UpstreamRemote(), rootOpts.Fork, rootOpts.effectiveForkRemote(), issue.Number, issue.Title, claimed.Lease.ClaimID, claimed.Branch)
				var worktree, branch string
				err = opts.SetupProgress.ReportSetupPhase(ctx, issue.Number, "workspace", func() error {
					pc := claimed.ProvisionContext(rootOpts.Fork)
					pc.UpstreamRemote = mgr.UpstreamRemote()
					worktree, branch, err = workspace.ProvisionWithContext(target.Path, checkpointRemote, issue.Number, issue.Title, claimed.BaseSHA, targetBranch, claimed.HeadSHA, pc)
					return err
				})
				if err != nil {
					return nil, err
				}
				if err := opts.SetupProgress.ReportSetupPhase(ctx, issue.Number, "bundle", func() error {
					wf, err := harness.LoadWorkflowAt(target.Path, wu.Workflow, claimed.BaseSHA)
					if err != nil {
						return err
					}
					return workspace.CompileHarnessBundleAt(worktree, wu, wf, claimed.Artifacts, role, claimed.BaseSHA)
				}); err != nil {
					return nil, err
				}
				return &pulledWorkUnit{Repo: target.Repo, RepoRoot: target.Path, Backend: target.Backend, Issue: issue,
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
	return nil, errNoReadyWork
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
		slug = o.detectUpstreamSlug(root)
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

// orderReadyIssues permutes only supplied candidates. Each epic retains its
// numeric slots, avoiding a cyclic comparator for mixed standalone/sibling work.
func orderReadyIssues(ctx context.Context, tr tracker.Tracker, issues []*tracker.Issue, stderr io.Writer) ([]*tracker.Issue, error) {
	out := append([]*tracker.Issue(nil), issues...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	groups := make(map[int][]int)
	var parents []int
	warn := func(n int, err error) {
		fmt.Fprintf(stderr, "warning: cannot read sub-issue order for #%d; using issue-number order: %v\n", n, err)
	}
	for i, issue := range out {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		parent, err := tr.ParentIssueNumber(ctx, issue.Number)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			warn(issue.Number, err)
			continue
		}
		if parent != 0 {
			if len(groups[parent]) == 0 {
				parents = append(parents, parent)
			}
			groups[parent] = append(groups[parent], i)
		}
	}
	for _, parent := range parents {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		slots := groups[parent]
		if len(slots) < 2 {
			continue
		}
		epic, err := tr.GetIssue(ctx, parent)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			warn(parent, err)
			continue
		}
		if epic == nil {
			warn(parent, fmt.Errorf("missing parent"))
			continue
		}
		if !epic.IsEpic() {
			continue
		}
		children, err := tr.ListSubIssues(ctx, parent)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			warn(parent, err)
			continue
		}
		candidates := make(map[int]*tracker.Issue)
		for _, slot := range slots {
			candidates[out[slot].Number] = out[slot]
		}
		seen := make(map[int]bool)
		var ordered []*tracker.Issue
		valid := true
		for _, child := range children {
			if child == nil || child.Number <= 0 || seen[child.Number] {
				valid = false
				break
			}
			seen[child.Number] = true
			if candidate := candidates[child.Number]; candidate != nil {
				ordered = append(ordered, candidate)
			}
		}
		if !valid || len(ordered) != len(slots) {
			warn(parent, fmt.Errorf("incomplete or duplicate sibling list"))
			continue
		}
		for i, slot := range slots {
			out[slot] = ordered[i]
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
