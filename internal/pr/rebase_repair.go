package pr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/safefs"
	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workspace"
)

const repairTask = `Repair only the allowed paths listed in .swarm/active/REPAIR_PATHS.md so the trusted checks in VALIDATION.md pass. Read PLAN.md, TEST_PLAN.md and REBASE_PLAN.md. Preserve the approved behavior. Do not edit any control or instruction file. This snapshot has no Git metadata; the coordinator commits accepted edits and reruns validation.`

func repairAllowed(p string, paths []string, rv *resolver) bool {
	if isControlPath(p) || p == ".git" || strings.HasPrefix(p, ".git/") || p == "AGENTS.md" || p == "CLAUDE.md" || !slices.Contains(paths, p) {
		return false
	}
	if !rv.unit.AllowHarnessChanges {
		for _, pattern := range rv.protected {
			if validation.MatchProtectedPath(pattern, p) {
				return false
			}
		}
	}
	return true
}

func verifyRepairTree(dir string, before map[string]planFile, paths []string, rv *resolver) (map[string]planFile, error) {
	after, err := snapshotPlanTree(dir)
	if err != nil {
		return nil, err
	}
	if err := verifySnapshotDirectories(dir, before, paths, rv); err != nil {
		return nil, err
	}
	changed := false
	for p, a := range after {
		b, ok := before[p]
		if ok && a.mode == b.mode && bytes.Equal(a.data, b.data) {
			continue
		}
		if !repairAllowed(p, paths, rv) {
			return nil, fmt.Errorf("repair changed disallowed path %s", p)
		}
		changed = true
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			if !repairAllowed(p, paths, rv) {
				return nil, fmt.Errorf("repair removed disallowed path %s", p)
			}
			changed = true
		}
	}
	if !changed {
		return nil, fmt.Errorf("repair produced no changes")
	}
	return after, nil
}

func exportRepair(ctx context.Context, worktree, dir string, st *lease.IssueState) error {
	paths, err := workspace.TrackedRepairFiles(ctx, worktree)
	if err != nil {
		return err
	}
	src, err := safefs.Open(worktree)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := safefs.Open(dir)
	if err != nil {
		return err
	}
	defer dst.Close()
	for _, p := range paths {
		if isControlPath(p) {
			continue
		}
		info, err := os.Lstat(filepath.Join(worktree, filepath.FromSlash(p)))
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("repair snapshot requires regular file %s", p)
		}
		data, err := src.ReadFile(filepath.FromSlash(p), 32<<20)
		if err != nil {
			return err
		}
		if err = dst.WriteFile(filepath.FromSlash(p), data, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return restoreApprovedPlan(dir, st)
}

// importRepair checks the entire output before importing any file. If a write
// fails, every previously changed file is restored through the same safe root.
func importRepair(worktree string, before, after map[string]planFile, paths []string, rv *resolver) (err error) {
	root, err := safefs.Open(worktree)
	if err != nil {
		return err
	}
	defer root.Close()
	var written []string
	defer func() {
		if err == nil {
			return
		}
		for _, p := range written {
			var e error
			if b, ok := before[p]; ok {
				e = root.WriteFile(filepath.FromSlash(p), b.data, b.mode.Perm())
			} else {
				e = root.Remove(filepath.FromSlash(p))
			}
			err = errors.Join(err, e)
		}
	}()
	for _, p := range paths {
		if !repairAllowed(p, paths, rv) {
			continue
		}
		b, bok := before[p]
		a, aok := after[p]
		if bok == aok && b.mode == a.mode && bytes.Equal(b.data, a.data) {
			continue
		}
		written = append(written, p)
		if !aok {
			err = root.Remove(filepath.FromSlash(p))
		} else {
			err = root.WriteFile(filepath.FromSlash(p), a.data, a.mode.Perm())
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) finishResolvedRebase(ctx context.Context, r *Result, owner string, rv *resolver) (*Result, error) {
	cfg, err := harness.LoadConfigAt(s.Root, r.State.BaseSHA)
	if err != nil {
		return r, err
	}
	paths, err := workspace.RepairPaths(ctx, r.Worktree, r.State.BaseSHA)
	if err != nil {
		return r, err
	}
	if r.State.LastRebase != nil {
		paths = append(paths, r.State.LastRebase.ConflictedPaths...)
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	if r.State.LastRebase != nil && r.State.LastRebase.RepairScopeSet {
		paths = append([]string(nil), r.State.LastRebase.RepairPaths...)
	} else {
		st, e := s.Manager.UpdateRebaseContext(ctx, r.IssueNumber, owner, r.PR.Number, lease.RebaseContext{Status: "completed", RepairPaths: &paths})
		if e != nil {
			return r, e
		}
		r.State = st
	}
	stop := func(cause error) (*Result, error) {
		evidence := ""
		if r.Validation != nil {
			evidence = r.Validation.Artifact.Body
		}
		return r, fmt.Errorf("%w\n%s\nLast local checkpoint: %s. Run swarm %spr work %d to repair manually, then swarm %spr validate %d; publication remains swarm %spr submit %d", cause, evidence, r.HeadSHA, forkFlag(s.Fork), r.PR.Number, forkFlag(s.Fork), r.PR.Number, forkFlag(s.Fork), r.PR.Number)
	}
	for {
		checked, valErr := s.validateClaim(ctx, r, owner, r.State, s.Progress, false, false)
		if checked != nil {
			r = checked
		}
		err = valErr
		if err == nil && r.Validation != nil && r.Validation.Passed {
			r.SubmitCommand = fmt.Sprintf("swarm %spr submit %d", forkFlag(s.Fork), r.PR.Number)
			return r, nil
		}
		if ctx.Err() != nil {
			return stop(ctx.Err())
		}
		// Infrastructure failures do not constitute repairable check failures.
		if r.Validation == nil || r.Validation.Passed || !errors.Is(err, validation.ErrRequiredCheckFailed) {
			return stop(err)
		}
		for _, check := range r.Validation.CheckOutcomes {
			if check.InfrastructureError {
				return stop(fmt.Errorf("validation infrastructure failed: %s", check.Name))
			}
		}
		st := r.State
		if st.LastRebase == nil || st.LastRebase.RepairAttempts >= cfg.Lease.RebaseRepairAttempts() {
			return stop(fmt.Errorf("rebase repair limit reached"))
		}
		// Count before execution so cancelled/rejected turns cannot reset the budget.
		attempt := st.LastRebase.RepairAttempts + 1
		st, err = s.Manager.UpdateRebaseContext(ctx, r.IssueNumber, owner, r.PR.Number, lease.RebaseContext{Status: "completed", RepairAttempt: &attempt})
		if err != nil {
			return stop(err)
		}
		r.State = st
		dir, err := os.MkdirTemp("", "swarm-repair-")
		if err != nil {
			return stop(err)
		}
		repairErr := func() error {
			defer os.RemoveAll(dir)
			if err := exportRepair(ctx, r.Worktree, dir, st); err != nil {
				return err
			}
			var plan strings.Builder
			for _, p := range st.LastRebase.Plans {
				fmt.Fprintf(&plan, "# Stop %s\n%s\n", p.Commit, p.Body)
			}
			for name, body := range map[string]string{"VALIDATION.md": r.Validation.Artifact.Body, "REBASE_PLAN.md": plan.String(), "REPAIR_PATHS.md": strings.Join(paths, "\n"), "SYSTEM_PROMPT.md": repairTask, "WORKUNIT.md": renderResolverWorkUnit(rv.unit)} {
				if err := safefs.WriteFile(dir, filepath.Join(".swarm/active", name), []byte(body), 0644); err != nil {
					return err
				}
			}
			before, err := snapshotPlanTree(dir)
			if err != nil {
				return err
			}
			_, err = rv.runner.Run(ctx, agent.RunRequest{Agent: rv.agentName, Role: "developer", Mode: rv.mode, WorktreePath: dir, SystemPrompt: ".swarm/active/SYSTEM_PROMPT.md", TaskPrompt: repairTask, Local: rv.local, Dependencies: rv.deps})
			if err != nil {
				return fmt.Errorf("repair agent failed: %s", strings.ReplaceAll(err.Error(), dir, "<repair workspace>"))
			}
			after, err := verifyRepairTree(dir, before, paths, rv)
			if err != nil {
				return err
			}
			if err = s.Manager.CheckClaim(ctx, r.IssueNumber, owner); err != nil {
				return err
			}
			head, dirty, rebasing, err := workspace.PRHead(ctx, r.Worktree)
			if err != nil {
				return err
			}
			if dirty || rebasing || head != r.HeadSHA {
				return fmt.Errorf("candidate changed during repair")
			}
			if err = importRepair(r.Worktree, before, after, paths, rv); err != nil {
				return err
			}
			head, err = workspace.Checkpoint(ctx, r.Worktree, r.Branch, workspace.CheckpointOptions{WorkUnit: rv.unit, Purpose: "repair", Detail: cfg.CommitMessageDetail()})
			if err != nil {
				return err
			}
			st, err = s.Manager.RecordPRHead(ctx, r.IssueNumber, owner, r.PR.Number, r.Branch, head)
			if err != nil {
				return err
			}
			r.State = st
			r.HeadSHA = head
			return nil
		}()
		if repairErr != nil {
			return stop(repairErr)
		}
	}
}

func forkFlag(fork bool) string {
	if fork {
		return "--fork "
	}
	return ""
}
