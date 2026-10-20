package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"time"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/node"
	"github.com/supernovae/swarmos/internal/registry"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
)

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

func newPullCmd(rootOpts *RootOptions) *cobra.Command {
	var (
		allRepos    bool
		agentName   string
		durationStr string
	)

	cmd := &cobra.Command{
		Use:   "pull",
		Short: "Find, claim, and provision the next unclaimed or handed-off swarm:ready issue matching node capabilities",
		RunE: func(cmd *cobra.Command, args []string) error {
			var dur time.Duration
			if durationStr != "" {
				parsed, err := time.ParseDuration(durationStr)
				if err != nil {
					return fmt.Errorf("invalid --duration %q: %w", durationStr, err)
				}
				dur = parsed
			}

			pulled, err := PullNextReadyIssue(cmd.Context(), cmd, rootOpts, allRepos, agentName, dur)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Pulled & claimed [%s] issue #%d: %s [phase=%s, expires=%s]\n",
				pulled.Repo, pulled.Issue.Number, pulled.Issue.Title, pulled.TargetPhase, pulled.State.Lease.ExpiresAt.Format(time.RFC3339))
			fmt.Fprintf(out, "  Repo Root: %s\n", pulled.RepoRoot)
			fmt.Fprintf(out, "  Branch:    %s\n", pulled.BranchName)
			fmt.Fprintf(out, "  Worktree:  %s\n", pulled.WorktreePath)
			fmt.Fprintf(out, "  Contract:  %s/.swarm/active/WORKUNIT.md\n", pulled.WorktreePath)
			return nil
		},
	}

	cmd.Flags().BoolVar(&allRepos, "all", false, "Scan all registered repositories in ~/.config/swarm/repos.yaml")
	cmd.Flags().StringVar(&agentName, "agent", "", "Agent harness (claude, codex, opencode, omp, mock)")
	cmd.Flags().StringVar(&durationStr, "duration", "", "Lease TTL duration (default 4h)")
	return cmd
}

func PullNextReadyIssue(ctx context.Context, cmd *cobra.Command, rootOpts *RootOptions, allRepos bool, agentName string, durOverride time.Duration) (*PulledWorkUnit, error) {
	targets, err := rootOpts.resolveTargets(allRepos)
	if err != nil {
		return nil, err
	}

	nodeProf, _ := node.LoadOrDetectProfile("")
	now := time.Now().UTC()

	for _, tgt := range targets {
		tr, mgr, cfg, err := rootOpts.resolveTargetLeaseManager(cmd, tgt)
		if err != nil {
			continue
		}
		if nodeProf != nil {
			mgr.SetNodeProfile(nodeProf)
		}

		agent := agentName
		if agent == "" {
			agent = cfg.DefaultAgent
			if agent == "" {
				agent = "claude"
			}
		}

		dur := durOverride
		if dur <= 0 {
			if parsed, err := time.ParseDuration(cfg.Lease.Duration); err == nil && parsed > 0 {
				dur = parsed
			} else {
				dur = 4 * time.Hour
			}
		}

		readyLabel := cfg.Security.ReadyLabel
		if readyLabel == "" {
			readyLabel = "swarm:ready"
		}

		var filterLabels []string
		if cfg.Security.RequireReadyLabel {
			filterLabels = []string{readyLabel}
		}

		issues, err := tr.ListIssues(ctx, tracker.IssueFilter{
			State:  "open",
			Labels: filterLabels,
		})
		if err != nil {
			continue
		}

		owner, _ := tr.CurrentUser(ctx)
		if owner == "" {
			owner = "local-dev"
		}

		for _, iss := range issues {
			if slices.Contains(iss.Labels, "swarm:pr-open") {
				continue
			}
			if err := mgr.VerifyTrustPolicy(iss); err != nil {
				continue
			}
			wu, err := contract.ParseWorkUnitFromIssueWithRoot(iss.Number, iss.Title, iss.Body, tgt.Path)
			if err != nil {
				continue
			}

			// Verify depends_on prerequisites are satisfied
			if err := mgr.VerifyDependencies(ctx, wu); err != nil {
				continue
			}

			// Verify volunteer node capability requirements match
			if !node.MatchesCapabilities(nodeProf, wu.RequiredCapabilities) {
				continue
			}

			targetPhase := "all"
			role := "planner"

			if st, err := mgr.GetCurrentState(ctx, iss.Number); err == nil && st != nil {
				if st.Lease.Status == "completed" {
					continue
				}
				if st.Lease.Status == "active" && st.Lease.ExpiresAt.After(now) {
					continue
				}
				if st.Lease.Status == "handoff" && st.Lease.NextPhase != "" {
					if !node.SupportsPhaseCapability(nodeProf, st.Lease.NextPhase) {
						continue
					}
					targetPhase = st.Lease.NextPhase
					if targetPhase == "implement" {
						role = "developer"
					} else if targetPhase == "review" {
						role = "reviewer"
					}
				}
			}

			if hasGitRemote(tgt.Path) {
				_ = exec.CommandContext(ctx, "git", "-C", tgt.Path, "fetch", "origin", "--prune").Run()
			}

			st, err := mgr.Acquire(ctx, iss.Number, owner, agent, targetPhase, dur, cfg.Lease.MaxRenewals)
			if err != nil {
				if errors.Is(err, lease.ErrAlreadyClaimed) || errors.Is(err, lease.ErrClaimRaceLost) {
					continue
				}
				return nil, err
			}

			wf, err := harness.LoadWorkflow(tgt.Path, wu.Workflow)
			if err != nil {
				return nil, err
			}

			wtPath, branchName, err := workspace.Provision(tgt.Path, iss.Number, iss.Title)
			if err != nil {
				return nil, err
			}

			if err := workspace.CompileHarnessBundle(wtPath, wu, wf, st.Artifacts, role); err != nil {
				return nil, err
			}

			return &PulledWorkUnit{
				Repo:         tgt.Repo,
				RepoRoot:     tgt.Path,
				Backend:      tgt.Backend,
				Issue:        iss,
				WorkUnit:     wu,
				State:        st,
				TargetPhase:  targetPhase,
				WorktreePath: wtPath,
				BranchName:   branchName,
			}, nil
		}
	}

	if allRepos {
		return nil, fmt.Errorf("no unclaimed or handed-off swarm:ready issues matching node capabilities found across %d registered repository(s)", len(targets))
	}
	return nil, fmt.Errorf("no unclaimed or handed-off swarm:ready issues matching node capabilities found in %s", targets[0].Repo)
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
