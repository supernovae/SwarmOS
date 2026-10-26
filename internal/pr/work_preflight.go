package pr

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
)

// WorkPreflightResult is advisory; a pass never grants task or plan approval.
type WorkPreflightResult struct {
	Status         string   `json:"status"`
	Mergeable      *bool    `json:"mergeable"`
	MergeableState string   `json:"mergeable_state"`
	Mergeability   string   `json:"mergeability"`
	TargetBase     string   `json:"target_base"`
	ApprovedBase   string   `json:"approved_base"`
	BaseResult     string   `json:"base_result"`
	Blockers       []string `json:"blockers"`
	Warnings       []string `json:"warnings"`
	Commands       []string `json:"commands"`
	pausedRebase   bool
}

func mergeability(p *tracker.PullRequest) string {
	if (p.Mergeable != nil && !*p.Mergeable) || (p.MergeableState == "dirty" && p.Mergeable == nil) {
		return "conflicting"
	}
	if p.Mergeable == nil || p.MergeableState == "unknown" {
		return "unknown"
	}
	return "clean"
}

func (s *Service) workCommand(r *Result, operation, flags string) string {
	cmd := fmt.Sprintf("swarm pr %s %d --backend github", operation, r.PR.Number)
	if flags != "" {
		cmd += " " + flags
	}
	if s.Fork {
		cmd += fmt.Sprintf(" --fork --fork-remote %q", s.ForkRemote)
	}
	return cmd
}

func (s *Service) localRebaseSupersedes(ctx context.Context, r *Result) bool {
	rev, ev := r.State.PRRevision, r.State.LastRebase
	if rev == nil || rev.Number != r.PR.Number || rev.Intent != "rebase" || ev == nil || ev.Status != "completed" || ev.NewHead != rev.LocalHeadSHA || ev.NewBase != r.BaseSHA || ev.NewBase != r.PR.BaseSHA {
		return false
	}
	head, dirty, rebasing, err := workspace.InspectPRHead(ctx, r.Worktree)
	if err != nil || dirty || rebasing || head != ev.NewHead {
		return false
	}
	ancestor, err := workspace.BaseIsAncestor(ctx, r.Worktree, ev.NewBase, head)
	if err != nil || !ancestor {
		return false
	}
	_, err = workspace.RebasePatchIdentity(ctx, r.Worktree, ev.OldBase, ev.OldHead, ev.NewBase, head)
	return err == nil
}

func (s *Service) checkWork(ctx context.Context, r *Result) error {
	w := &WorkPreflightResult{Status: "ready", Mergeable: r.PR.Mergeable, MergeableState: r.PR.MergeableState, Mergeability: mergeability(r.PR), ApprovedBase: r.BaseSHA, TargetBase: r.PR.BaseSHA, BaseResult: "matching"}
	r.WorkPreflight = w
	if s.Tracker.BackendName() != "github" {
		w.Status = "skipped"
		w.BaseResult = "skipped"
		w.Mergeability = "skipped"
		return nil
	}
	if r.PR.BaseSHA != r.BaseSHA {
		w.BaseResult = "moved"
	}
	// Inspect only an existing worktree; never prepare one during the advisory check.
	if _, _, rebasing, err := workspace.InspectPRHead(ctx, r.Worktree); err == nil && rebasing {
		w.Status, w.pausedRebase = "blocked", true
		w.Blockers = append(w.Blockers, PausedRebaseError(ctx, r.Worktree, r.PR.Number).Error())
		w.Commands = []string{s.workCommand(r, "rebase", "--continue"), s.workCommand(r, "rebase", "--continue --resolve"), s.workCommand(r, "rebase", "--abort")}
		return nil
	}
	budget, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	initial := *r.PR
	for attempt := 0; ; attempt++ {
		p := r.PR
		w.Mergeable, w.MergeableState, w.TargetBase = p.Mergeable, p.MergeableState, p.BaseSHA
		w.Mergeability = mergeability(p)
		if p.BaseSHA != r.BaseSHA {
			w.BaseResult = "moved"
			w.Blockers = append(w.Blockers, fmt.Sprintf("PR #%d target base %s differs from issue #%d approved baseline %s", p.Number, p.BaseSHA, r.IssueNumber, r.BaseSHA))
		}
		if w.Mergeability == "conflicting" {
			if s.localRebaseSupersedes(ctx, r) {
				w.Warnings = append(w.Warnings, "published head superseded by local rebase")
			} else {
				raw := "null"
				if p.Mergeable != nil {
					raw = fmt.Sprint(*p.Mergeable)
				}
				w.Blockers = append(w.Blockers, fmt.Sprintf("PR #%d is conflicting: mergeable=%s mergeable_state=%q", p.Number, raw, p.MergeableState))
			}
		}
		if len(w.Blockers) > 0 || w.Mergeability != "unknown" {
			break
		}
		if attempt == 2 {
			break
		}
		wait := s.workWait
		if wait == nil {
			wait = func(ctx context.Context) error {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Second):
					return nil
				}
			}
		}
		if err := wait(budget); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, context.DeadlineExceeded) {
				break
			}
			return err
		}
		fresh, err := s.Tracker.GetPullRequest(budget, p.Number)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, context.DeadlineExceeded) && budget.Err() != nil {
				break
			}
			return err
		}
		if fresh == nil || fresh.Number != initial.Number || fresh.HeadSHA != initial.HeadSHA || fresh.Head != initial.Head || fresh.HeadRepo != initial.HeadRepo || fresh.BaseRepo != initial.BaseRepo || fresh.Base != initial.Base || !strings.EqualFold(fresh.State, "open") || fresh.Merged {
			return fmt.Errorf("PR changed during mergeability check; retry swarm pr work %d", initial.Number)
		}
		r.PR = fresh
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.Mergeability == "unknown" {
		w.Warnings = append(w.Warnings, "mergeability was unknown after bounded retries; continuing with existing approval checks")
	}
	if len(w.Blockers) > 0 {
		w.Status = "blocked"
		if w.BaseResult == "moved" {
			releaseSuffix := ""
			if s.Fork {
				releaseSuffix = fmt.Sprintf(" --fork --fork-remote %q", s.ForkRemote)
			}
			w.Commands = append(w.Commands, fmt.Sprintf("If a claim is active: swarm issue release %d --reason \"re-approve against the new base for PR #%d\" --backend github", r.IssueNumber, r.PR.Number)+releaseSuffix, fmt.Sprintf("Maintainer (upstream): swarm issue approve %d --backend github", r.IssueNumber))
		}
		w.Commands = append(w.Commands, s.workCommand(r, "rebase", ""), "After resolving and staging conflicts: "+s.workCommand(r, "rebase", "--continue"), "Or resolve with the developer agent: "+s.workCommand(r, "rebase", "--continue --resolve"), s.workCommand(r, "validate", ""), s.workCommand(r, "submit", ""))
	}
	return nil
}

// stopError explains a blocked preflight. A paused rebase must be finished
// first; a moved base then needs task approval before any new rebase;
// otherwise the blockers are conflicts.
func (w *WorkPreflightResult) stopError(r *Result) error {
	err := fmt.Errorf("%s\n%s", strings.Join(w.Blockers, "\n"), strings.Join(w.Commands, "\n"))
	if !w.pausedRebase && w.BaseResult == "moved" {
		return &lease.TaskApprovalError{IssueNumber: r.IssueNumber, Err: err}
	}
	return &ConflictError{PRNumber: r.PR.Number, Paused: w.pausedRebase, Err: err}
}
