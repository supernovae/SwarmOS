package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/supernovae/swarmos/internal/safefs"
)

// ProvisionContext carries coordinator-owned identity for provisioning. It is
// built only from authoritative issue state, never from a worktree's files.
type ProvisionContext struct {
	// Dropped recovers a checkpoint contained in the approved base.
	Dropped *DroppedCheckpoint
	// PreviousBase is the prior approved base of a same-repository task with
	// no recorded checkpoint; non-empty means the task was re-approved. It may
	// name a later base than the branch after several re-approvals, so an
	// untouched branch and worktree at any ancestor of the newly approved base
	// are fast-forwarded to it.
	PreviousBase string
	// UpstreamRemote names the project remote that holds approved bases and
	// same-repository checkpoints; empty means origin. A checkpoint remote
	// with any other name is a fork, even when it is called origin.
	UpstreamRemote string
	// FreshStart is the attempt following a maintainer-discarded checkpoint.
	// A worktree still holding the abandoned attempt is set aside, intact,
	// so the fresh branch starts at the approved base.
	FreshStart *FreshStart
}

// ProvisionWithContext is ProvisionFromRemoteAt with explicit recovery context.
func ProvisionWithContext(repoRoot, checkpointRemote string, issueNum int, title, baseSHA, branch, headSHA string, pc ProvisionContext) (string, string, error) {
	upstream := UpstreamRemoteOrDefault(pc.UpstreamRemote)
	if pc.Dropped != nil {
		return provisionFromRemote(repoRoot, upstream, checkpointRemote, issueNum, title, baseSHA, branch, headSHA, pc.Dropped)
	}
	if fs := pc.FreshStart; fs != nil && headSHA == "" && branch == fs.Branch && (checkpointRemote == "" || checkpointRemote == upstream) {
		if err := setAsideAbandonedWorktree(repoRoot, issueNum, fs); err != nil {
			return "", "", err
		}
	}
	previous := strings.TrimSpace(pc.PreviousBase)
	if previous != "" && headSHA == "" && (checkpointRemote == "" || checkpointRemote == upstream) && issueNum > 0 && baseSHA != "" {
		if branch == "" {
			branch = BranchName(issueNum, title)
		}
		if err := advanceUntouchedBaseFrom(repoRoot, upstream, issueNum, branch, previous, baseSHA, nil); err != nil {
			return "", "", err
		}
	}
	return provisionFromRemote(repoRoot, upstream, checkpointRemote, issueNum, title, baseSHA, branch, headSHA, nil)
}

// staleActiveEvidence lists generated active files that describe the old
// base. Callers re-materialize them from authoritative state.
var staleActiveEvidence = []string{"PLAN.md", "TEST_PLAN.md", "REVIEW.md", "VALIDATION.md", "REPAIR.md", "IMPLEMENTATION.md", "REVIEW_EVIDENCE.md", "SYSTEM_PROMPT.md", "WORKUNIT.md", "workunit.json"}

// advanceUntouchedBase fast-forwards a task branch, and its worktree when one
// exists, from an earlier approved base to the new one. It does nothing
// without a previous base or when the branch is missing or already at base;
// a tip outside base or any other ineligible state is refused with a
// recovery hint before anything changes. beforeCAS lets tests
// move the branch between the eligibility checks and the ref update.
func advanceUntouchedBase(root string, issue int, branch, previous, base string, beforeCAS func() error) error {
	return advanceUntouchedBaseFrom(root, DefaultUpstreamRemote, issue, branch, previous, base, beforeCAS)
}

func advanceUntouchedBaseFrom(root, upstreamRemote string, issue int, branch, previous, base string, beforeCAS func() error) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	path := WorktreePath(absRoot, issue)
	refuse := func(cause error) error {
		return fmt.Errorf("cannot advance untouched worktree to the approved base: %w; %s", cause, RecoveryHint(issue, path, branch))
	}
	ref := "refs/heads/" + branch
	if !strings.HasPrefix(branch, fmt.Sprintf("swarm/issue-%d-", issue)) {
		return nil
	}
	if _, err := gitOutput(absRoot, "check-ref-format", ref); err != nil {
		return refuse(err)
	}
	if strings.TrimSpace(previous) == "" {
		return nil
	}
	resolvedBase, err := resolveCommit(absRoot, base)
	if err != nil {
		if fetchErr := FetchCommit(absRoot, upstreamRemote, base); fetchErr != nil {
			return nil
		}
		if resolvedBase, err = resolveCommit(absRoot, base); err != nil {
			return nil
		}
	}
	base = resolvedBase
	// Eligibility rests on the branch tip, not previous: re-approvals may have
	// moved previous past the tip, and that commit may not exist locally.
	tip, err := resolveCommit(absRoot, ref)
	if err != nil || tip == base {
		// Missing or already advanced: ordinary provisioning decides.
		return nil
	}
	if _, err := gitOutput(absRoot, "merge-base", "--is-ancestor", tip, base); err != nil {
		return refuse(fmt.Errorf("task branch %s carries commits not contained in approved base %s", tip, base))
	}
	info, statErr := os.Lstat(path)
	switch {
	case statErr == nil:
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return refuse(errors.New("worktree path must be a real directory"))
		}
		if err := verifyUntouchedWorktree(absRoot, path, branch, tip, base); err != nil {
			return refuse(err)
		}
	case errors.Is(statErr, os.ErrNotExist):
		registered, err := gitOutput(absRoot, "worktree", "list", "--porcelain")
		if err != nil {
			return refuse(err)
		}
		if strings.Contains(registered, "branch "+ref+"\n") {
			return refuse(errors.New("task branch is registered to another worktree"))
		}
	default:
		return refuse(statErr)
	}
	// Every eligibility check precedes the compare-and-swap; nothing has
	// changed yet if it fails.
	if beforeCAS != nil {
		if err := beforeCAS(); err != nil {
			return refuse(err)
		}
	}
	if _, err := gitOutput(absRoot, "update-ref", "-m", "swarm: advance untouched task to approved base", ref, base, tip); err != nil {
		return refuse(fmt.Errorf("branch moved during advancement: %w", err))
	}
	if statErr != nil {
		return nil
	}
	if err := checkoutAdvancedBase(path, branch, tip, base); err != nil {
		// Roll back only if the branch is still exactly where we put it.
		if _, rollbackErr := gitOutput(absRoot, "update-ref", ref, tip, base); rollbackErr != nil {
			return fmt.Errorf("advanced branch %s to %s but could not update worktree %q (%v) or restore the branch (%v); %s", branch, base, path, err, rollbackErr, RecoveryHint(issue, path, branch))
		}
		return refuse(err)
	}
	return nil
}

// verifyUntouchedWorktree confirms the worktree holds nothing but the
// branch tip and SwarmOS-managed changes, and that checking out base
// cannot overwrite any untracked or ignored file.
func verifyUntouchedWorktree(root, path, branch, previous, base string) error {
	if err := verifyWorktree(path, branch, previous); err != nil {
		return err
	}
	common, err := gitOutput(path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	expected, err := gitOutput(root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || !samePath(strings.TrimSpace(common), strings.TrimSpace(expected)) {
		return errors.New("worktree belongs to another repository")
	}
	for _, marker := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "REBASE_HEAD", "rebase-merge", "rebase-apply", "BISECT_LOG"} {
		gitPath, err := gitOutput(path, "rev-parse", "--path-format=absolute", "--git-path", marker)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(strings.TrimSpace(gitPath)); err == nil {
			return fmt.Errorf("a Git operation is in progress (%s)", marker)
		}
	}
	if err := verifyRecoveryIndex(path, previous); err != nil {
		return err
	}
	if err := verifyUncheckpointedWorktree(path, previous); err != nil {
		return err
	}
	return verifyCheckoutCollisions(path, previous, base)
}

// verifyCheckoutCollisions refuses when a path base adds already exists, or a
// parent of it is not a directory. Git would overwrite ignored files silently.
func verifyCheckoutCollisions(path, previous, base string) error {
	changes, err := gitOutput(path, "diff", "--name-status", "-z", "--no-renames", previous, base)
	if err != nil {
		return err
	}
	parts := strings.Split(changes, "\x00")
	for i := 0; i+1 < len(parts); i += 2 {
		if parts[i] != "A" {
			continue
		}
		name := parts[i+1]
		for p := name; p != "." && p != ""; p = filepath.Dir(p) {
			info, err := os.Lstat(filepath.Join(path, filepath.FromSlash(p)))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if p == name || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("untracked file %s would be overwritten by the approved base", p)
			}
		}
	}
	return nil
}

// checkoutAdvancedBase moves the index and files from previous to base after
// the branch CAS. The verified marker-only changes are dropped first; the
// active bundle is regenerated by the caller from authoritative state.
func checkoutAdvancedBase(path, branch, previous, base string) error {
	if err := verifyWorktree(path, branch, base); err != nil {
		return err
	}
	if _, err := gitOutput(path, "read-tree", previous); err != nil {
		return err
	}
	if err := CleanEphemeralHarnessMarkers(path); err != nil {
		return err
	}
	_, _ = gitOutput(path, "update-index", "-q", "--refresh")
	if err := verifyCheckoutCollisions(path, previous, base); err != nil {
		return err
	}
	if _, err := gitOutput(path, "read-tree", "-m", "-u", previous, base); err != nil {
		return err
	}
	if err := verifyWorktree(path, branch, base); err != nil {
		return err
	}
	safe, err := safefs.Open(path)
	if err != nil {
		return err
	}
	defer safe.Close()
	for _, name := range staleActiveEvidence {
		err := safe.Remove(filepath.Join(".swarm", "active", name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// RecoveryHint names the manual recovery for a task worktree SwarmOS cannot
// reuse. Edits are saved and commits kept on a renamed branch before removal.
func RecoveryHint(issue int, path, branch string) string {
	return fmt.Sprintf("to start over from the approved base, copy any edits you want to keep outside the worktree, then run git worktree remove --force -- %s and git branch -m %s %s (which keeps its commits), and retry swarm issue work %d",
		shellQuote(path), shellQuote(branch), shellQuote(fmt.Sprintf("swarm-backup/issue-%d", issue)), issue)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
