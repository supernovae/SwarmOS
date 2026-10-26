package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/supernovae/swarmos/internal/gitcredential"
)

// HeadObservation distinguishes a missing ref from an unsuccessful lookup.
type HeadObservation struct {
	SHA     string `json:"sha,omitempty"`
	Missing bool   `json:"missing,omitempty"`
	Error   string `json:"error,omitempty"`
}

type CheckpointState struct {
	Worktree       HeadObservation `json:"worktree"`
	LocalBranch    HeadObservation `json:"local_branch"`
	RemoteBranch   HeadObservation `json:"remote_branch"`
	Recorded       HeadObservation `json:"recorded"`
	Branch         string          `json:"branch"`
	AttachedBranch string          `json:"attached_branch,omitempty"`
	WorktreePath   string          `json:"worktree_path"`
	Dirty          bool            `json:"dirty"`
	Rebasing       bool            `json:"rebasing"`
	Error          string          `json:"error,omitempty"`
}

func (h HeadObservation) String() string {
	if h.Error != "" {
		return "error: " + h.Error
	}
	if h.Missing {
		return "missing"
	}
	return h.SHA
}

func (s CheckpointState) String() string {
	return fmt.Sprintf("worktree HEAD=%s (branch=%s), local branch=%s, remote branch=%s, recorded checkpoint=%s; dirty=%t rebase=%t",
		s.Worktree.String(), s.AttachedBranch, s.LocalBranch.String(), s.RemoteBranch.String(), s.Recorded.String(), s.Dirty, s.Rebasing)
}

// InspectCheckpoint observes all four heads without fetching or updating refs,
// the index, working files, or authoritative state.
func InspectCheckpoint(ctx context.Context, root, remote string, n int, branch, recorded string) CheckpointState {
	s := CheckpointState{Branch: branch, WorktreePath: WorktreePath(root, n), Recorded: HeadObservation{SHA: recorded, Missing: recorded == ""}}
	observe := func(dir, ref string) HeadObservation {
		cmd, _, err := gitcredential.Command(ctx, dir, "rev-parse", "--verify", "--quiet", ref)
		if err != nil {
			return HeadObservation{Error: err.Error()}
		}
		raw, err := cmd.Output()
		out := string(raw)
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == 1 {
				return HeadObservation{Missing: true}
			}
			return HeadObservation{Error: err.Error()}
		}
		return HeadObservation{SHA: strings.TrimSpace(out)}
	}
	s.LocalBranch = observe(root, "refs/heads/"+branch)
	if info, err := os.Lstat(s.WorktreePath); errors.Is(err, os.ErrNotExist) {
		s.Worktree.Missing = true
	} else if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		s.Worktree.Error = "worktree must be a real directory"
	} else {
		top, err := gitOutputContext(ctx, s.WorktreePath, "rev-parse", "--show-toplevel")
		common, commonErr := gitOutputContext(ctx, s.WorktreePath, "rev-parse", "--git-common-dir")
		rootCommon, rootErr := gitOutputContext(ctx, root, "rev-parse", "--git-common-dir")
		common = strings.TrimSpace(common)
		rootCommon = strings.TrimSpace(rootCommon)
		if !filepath.IsAbs(common) {
			common = filepath.Join(s.WorktreePath, common)
		}
		if !filepath.IsAbs(rootCommon) {
			rootCommon = filepath.Join(root, rootCommon)
		}
		entry, entryErr := os.Lstat(filepath.Join(s.WorktreePath, ".git"))
		if err != nil || !samePath(strings.TrimSpace(top), s.WorktreePath) || commonErr != nil || rootErr != nil || !samePath(common, rootCommon) || entryErr != nil || !entry.Mode().IsRegular() {
			s.Worktree.Error = "directory is not the task worktree"
		} else {
			s.Worktree = observe(s.WorktreePath, "HEAD")
			attached, err := gitOutputContext(ctx, s.WorktreePath, "symbolic-ref", "--quiet", "--short", "HEAD")
			if err == nil {
				s.AttachedBranch = strings.TrimSpace(attached)
			}
			dirty, err := hasStatusChanges(ctx, s.WorktreePath)
			if err != nil {
				s.Error = err.Error()
			} else {
				s.Dirty = dirty
			}
			for _, name := range []string{"rebase-merge", "rebase-apply"} {
				path, err := gitOutputContext(ctx, s.WorktreePath, "rev-parse", "--git-path", name)
				if err != nil {
					s.Error = err.Error()
					continue
				}
				path = strings.TrimSpace(path)
				if !filepath.IsAbs(path) {
					path = filepath.Join(s.WorktreePath, path)
				}
				if _, err := os.Lstat(path); err == nil {
					s.Rebasing = true
				} else if !errors.Is(err, os.ErrNotExist) {
					s.Error = err.Error()
				}
			}
		}
	}
	out, err := gitOutputContext(ctx, root, "ls-remote", "--refs", remote, "refs/heads/"+branch)
	if err != nil {
		s.RemoteBranch.Error = err.Error()
	} else {
		fields := strings.Fields(out)
		if len(fields) == 0 {
			s.RemoteBranch.Missing = true
		} else if len(fields) != 2 || fields[1] != "refs/heads/"+branch {
			s.RemoteBranch.Error = "invalid remote ref response"
		} else {
			s.RemoteBranch.SHA = fields[0]
		}
	}
	return s
}

type CheckpointDivergenceError struct {
	Issue int
	State CheckpointState
}

func (e *CheckpointDivergenceError) Error() string {
	return fmt.Sprintf("checkpoint divergence: %s; preserve local work, then inspect with swarm issue status %d; reconcile explicitly with swarm issue reconcile %d --checkpoint --expected-head %s --candidate-head %s --dry-run", e.State.String(), e.Issue, e.Issue, e.State.Recorded.SHA, e.State.Worktree.SHA)
}

// CommitsBeyond counts local commits beyond a resolved base.
func CommitsBeyond(ctx context.Context, root, base string) (int, error) {
	resolved, err := gitOutputContext(ctx, root, "rev-parse", "--verify", base+"^{commit}")
	if err != nil {
		return 0, err
	}
	out, err := gitOutputContext(ctx, root, "rev-list", "--count", strings.TrimSpace(resolved)+"..HEAD")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}
