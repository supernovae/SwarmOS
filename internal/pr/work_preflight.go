package pr

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
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
	stopCommands   []string
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
	cmd := fmt.Sprintf("swarm pr %s %d", operation, r.PR.Number)
	if flags != "" {
		cmd += " " + flags
	}
	cmd += " --backend " + s.Tracker.BackendName()
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

func (s *Service) checkWork(ctx context.Context, r *Result, phases ...string) error {
	phase := ""
	if len(phases) > 0 {
		phase = phases[0]
	}
	planning := s.recoveryPlanning(r, phase)
	w := &WorkPreflightResult{Status: "ready", Mergeable: r.PR.Mergeable, MergeableState: r.PR.MergeableState, Mergeability: mergeability(r.PR), ApprovedBase: r.BaseSHA, TargetBase: r.PR.BaseSHA, BaseResult: "matching"}
	r.WorkPreflight = w
	// Inspect only an existing worktree; never prepare one during the advisory check.
	if _, _, rebasing, err := workspace.InspectPRHead(ctx, r.Worktree); err == nil && rebasing {
		w.Status, w.pausedRebase = "blocked", true
		w.Blockers = append(w.Blockers, PausedRebaseError(ctx, r.Worktree, r.PR.Number).Error())
		w.Commands = []string{s.workCommand(r, "rebase", "--continue"), s.workCommand(r, "rebase", "--continue --resolve"), s.workCommand(r, "rebase", "--abort")}
		return nil
	}

	if !s.Fork && r.State.PRRevision != nil && r.PR.BaseSHA != r.BaseSHA {
		w.Status, w.BaseResult = "blocked", "moved"
		w.Blockers = append(w.Blockers, "PR target differs from approved baseline")
		w.Commands = s.recoveryCommands(ctx, r, true)
		w.stopCommands = append([]string(nil), w.Commands...)
		return nil
	}
	if !s.Fork && r.State.PRRevision != nil {
		defer func() {
			if len(w.Commands) == 0 && !w.pausedRebase {
				w.Commands = s.recoveryCommands(ctx, r, false)
				w.stopCommands = append([]string(nil), w.Commands...)
			}
		}()
	}
	if rev := r.State.PRRevision; rev != nil {
		ancestor, err := workspace.BaseIsAncestor(ctx, s.Root, r.BaseSHA, rev.LocalHeadSHA)
		if err != nil {
			return fmt.Errorf("inspect pending PR revision: %w; run swarm pr rebase %d --resolve", err, r.PR.Number)
		}
		if !ancestor && !planning {
			w.Status = "blocked"
			w.Blockers = append(w.Blockers, "pending PR revision does not descend from the approved baseline")
			if s.Fork || s.Manager.HasWorkerGateway() {
				w.Commands = []string{fmt.Sprintf("swarm pr rebase %d --resolve", r.PR.Number)}
			} else {
				w.Commands = s.recoveryCommands(ctx, r, true)
			}
			w.stopCommands = append([]string(nil), w.Commands...)
			return nil
		}
	}
	if s.Tracker.BackendName() != "github" {
		w.Status, w.BaseResult, w.Mergeability = "skipped", "skipped", "skipped"
		return nil
	}
	if r.PR.BaseSHA != r.BaseSHA {
		w.BaseResult = "moved"
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
			if planning || s.localRebaseSupersedes(ctx, r) {
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
			w.stopCommands = append(w.stopCommands, w.Commands[len(w.Commands)-2])
		}
		w.Commands = append(w.Commands, s.workCommand(r, "rebase", ""), "After resolving and staging conflicts: "+s.workCommand(r, "rebase", "--continue"), "Or resolve with the developer agent: "+s.workCommand(r, "rebase", "--continue --resolve"), s.workCommand(r, "validate", ""), s.workCommand(r, "submit", ""))
		w.stopCommands = append(w.stopCommands, s.workCommand(r, "rebase", ""), "After resolving and staging conflicts: "+s.workCommand(r, "rebase", "--continue"), s.workCommand(r, "validate", ""), s.workCommand(r, "submit", ""))
		if !w.pausedRebase {
			w.stopCommands = append(w.stopCommands, "Or resolve with the developer agent: "+s.workCommand(r, "rebase", "--continue --resolve"))
		}

	}
	return nil
}

// stopError explains a blocked preflight. A paused rebase must be finished
// first; a moved base then needs task approval before any new rebase;
// otherwise the blockers are conflicts.
func (w *WorkPreflightResult) stopError(r *Result) error {
	err := fmt.Errorf("%s\n%s", strings.Join(w.Blockers, "\n"), strings.Join(w.stopCommands, "\n"))
	if !w.pausedRebase && w.BaseResult == "moved" {
		return &lease.TaskApprovalError{IssueNumber: r.IssueNumber, Err: err}
	}
	return &ConflictError{PRNumber: r.PR.Number, Paused: w.pausedRebase, Err: err}
}

// recoveryPlanning is deliberately unavailable to status and code phases.
// verifyLink and workflow preflight independently require current task approval.
func (s *Service) recoveryPlanning(r *Result, phase string) bool {
	rev := r.State.PRRevision
	return (phase == "plan" || phase == "review") && !s.Fork && !s.Manager.HasWorkerGateway() &&
		strings.EqualFold(r.PR.HeadRepo, r.PR.BaseRepo) && rev != nil && rev.Number == r.PR.Number && rev.Intent == "work" &&
		rev.RemoteHeadSHA == r.PR.HeadSHA && rev.LocalHeadSHA != "" && r.BaseSHA == r.PR.BaseSHA
}

func (s *Service) recoveryCommands(ctx context.Context, r *Result, rebase bool) []string {
	commands := []string{}
	if r.PR.BaseSHA != r.BaseSHA {
		backend := s.Tracker.BackendName()
		commands = append(commands, fmt.Sprintf("If a claim is active: swarm issue release %d --reason %q --backend %s", r.IssueNumber, "approve current PR base", backend),
			fmt.Sprintf("Maintainer (upstream): swarm issue approve %d --backend %s", r.IssueNumber, backend))
	}
	issue, err := s.Tracker.GetIssue(ctx, r.IssueNumber)
	var unit *contract.WorkUnit
	if err == nil {
		unit, err = contract.ParseWorkUnitFromIssue(issue.Number, issue.Title, issue.Body)
	}
	if err != nil || lease.VerifyApproval(r.State, unit) != nil {
		if r.PR.BaseSHA != r.BaseSHA || !currentPRPlanPair(r.State) {
			commands = append(commands, s.workCommand(r, "work", "--phase plan"))
		}
		if r.State.EffectiveGate == contract.GateHuman {
			commands = append(commands, fmt.Sprintf("Independent maintainer: swarm issue review-plan %d --approve --backend %s", r.IssueNumber, s.Tracker.BackendName()))
		} else {
			commands = append(commands, s.workCommand(r, "work", "--phase review"))
		}
	}
	if rebase {
		commands = append(commands, s.workCommand(r, "rebase", "--resolve"))
	}
	return commands
}

func currentPRPlanPair(st *lease.IssueState) bool {
	var plan, test bool
	for _, a := range st.Artifacts {
		if a.Type != contract.ArtifactDesign && a.Type != contract.ArtifactTestPlan {
			continue
		}
		current := a.Body != "" && a.Metadata["superseded_by"] == "" && a.Metadata["content_sha256"] == fmt.Sprintf("%x", sha256.Sum256([]byte(a.Body)))
		if a.Type == contract.ArtifactDesign {
			plan = current
		} else {
			test = current
		}
	}
	return plan && test
}
