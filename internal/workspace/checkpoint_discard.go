package workspace

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/supernovae/swarmos/internal/safefs"
)

// FreshStart is coordinator-owned identity for the attempt after a maintainer
// discarded a task's checkpoint. The next attempt starts on Branch at the
// approved base; the abandoned branch and any worktree on it are kept.
type FreshStart struct {
	Branch           string `json:"branch"`
	AbandonedBranch  string `json:"abandoned_branch"`
	AbandonedHeadSHA string `json:"abandoned_head_sha"`
}

// FreshStartBranch names the branch for the attempt that follows discarding
// head. Each discarded head yields a distinct branch.
func FreshStartBranch(issue int, head string) string {
	if len(head) > 12 {
		head = head[:12]
	}
	return fmt.Sprintf("swarm/issue-%d-restart-%s", issue, head)
}

// DiscardCheckpointHint names the maintainer command that abandons an
// unsubmitted checkpoint so the next claim starts from the approved base.
func DiscardCheckpointHint(issue int, head string) string {
	return fmt.Sprintf("if this unsubmitted checkpoint is an abandoned attempt, a maintainer can run swarm issue discard-checkpoint %d --expected-head %s to keep it under a retention ref and start the next claim from the approved base", issue, head)
}

// setAsideAbandonedWorktree moves the issue worktree, when it still holds the
// abandoned attempt, to an unused sibling path with git worktree move. Files,
// index, branch, and commits are kept exactly; nothing is reset or removed.
// A worktree already on the fresh branch, or none at all, is left alone.
func setAsideAbandonedWorktree(root string, issue int, fs *FreshStart) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	path := WorktreePath(absRoot, issue)
	refuse := func(cause error) error {
		return fmt.Errorf("cannot set aside the abandoned worktree %q: %w; move it out of the way yourself, keeping any edits, then retry", path, cause)
	}
	prefix := fmt.Sprintf("swarm/issue-%d-", issue)
	if issue <= 0 || !strings.HasPrefix(fs.Branch, prefix) || !strings.HasPrefix(fs.AbandonedBranch, prefix) || len(fs.AbandonedHeadSHA) != 40 {
		return refuse(errors.New("invalid fresh-start identity"))
	}
	if _, err := hex.DecodeString(fs.AbandonedHeadSHA); err != nil {
		return refuse(errors.New("invalid fresh-start identity"))
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return refuse(err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return refuse(errors.New("worktree path must be a real directory"))
	}
	gitEntry, err := os.Lstat(filepath.Join(path, ".git"))
	if err != nil || !gitEntry.Mode().IsRegular() {
		return refuse(errors.New("path is not a registered linked worktree"))
	}
	common, err := gitOutput(path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return refuse(err)
	}
	expected, err := gitOutput(absRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || !samePath(strings.TrimSpace(common), strings.TrimSpace(expected)) {
		return refuse(errors.New("worktree belongs to another repository"))
	}
	if top, err := gitOutput(path, "rev-parse", "--show-toplevel"); err != nil || !samePath(strings.TrimSpace(top), path) {
		return refuse(errors.New("path is not the top of a git worktree"))
	}
	for _, marker := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "REBASE_HEAD", "rebase-merge", "rebase-apply", "BISECT_LOG"} {
		gitPath, err := gitOutput(path, "rev-parse", "--path-format=absolute", "--git-path", marker)
		if err != nil {
			return refuse(err)
		}
		if _, err := os.Lstat(strings.TrimSpace(gitPath)); err == nil {
			return refuse(fmt.Errorf("a Git operation is in progress (%s)", marker))
		}
	}
	branch, err := gitOutput(path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return refuse(errors.New("worktree HEAD is detached"))
	}
	switch strings.TrimSpace(branch) {
	case fs.Branch:
		// Already the fresh attempt; ordinary provisioning verifies it.
		return nil
	case fs.AbandonedBranch:
	default:
		return refuse(fmt.Errorf("worktree is on branch %q, neither the abandoned %q nor the fresh %q", strings.TrimSpace(branch), fs.AbandonedBranch, fs.Branch))
	}
	if _, err := gitOutput(path, "merge-base", "--is-ancestor", fs.AbandonedHeadSHA, "HEAD"); err != nil {
		return refuse(fmt.Errorf("worktree HEAD does not contain the discarded checkpoint %s", fs.AbandonedHeadSHA))
	}
	safe, err := safefs.Open(absRoot)
	if err != nil {
		return refuse(err)
	}
	defer safe.Close()
	parents, err := safe.Sub(filepath.Join(".swarm", "worktrees"), false, 0)
	if err != nil {
		return refuse(err)
	}
	defer parents.Close()
	// git worktree move nests the worktree inside an existing directory, so
	// the destination must not exist; an earlier archive is never reused.
	archive := ""
	for k := 1; k <= 100 && archive == ""; k++ {
		name := fmt.Sprintf("issue-%d-discarded-%s", issue, fs.AbandonedHeadSHA[:12])
		if k > 1 {
			name = fmt.Sprintf("%s-%d", name, k)
		}
		candidate := filepath.Join(filepath.Dir(path), name)
		if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			archive = candidate
		} else if err != nil {
			return refuse(err)
		}
	}
	if archive == "" {
		return refuse(errors.New("no unused archive path"))
	}
	if _, err := gitOutput(absRoot, "worktree", "move", "--", path, archive); err != nil {
		return refuse(err)
	}
	return nil
}
