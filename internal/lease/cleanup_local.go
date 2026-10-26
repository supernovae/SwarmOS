package lease

import (
	"context"
	"fmt"
	"strings"

	"github.com/supernovae/swarmos/internal/filelock"
	"github.com/supernovae/swarmos/internal/safefs"
	"github.com/supernovae/swarmos/internal/workspace"
)

func (m *Manager) cleanupLock() (func(), error) {
	dir, err := cleanupGitOutput(context.Background(), m.repoRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	root, err := safefs.Open(strings.TrimSpace(dir))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.OpenLock("swarm-cleanup.lock")
	if err != nil {
		return nil, err
	}
	if err = filelock.LockExclusive(f); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = filelock.Unlock(f); _ = f.Close() }, nil
}

// RetireCleanupLocalResources serializes synchronous local actions with recovery.
// Each action validates the reservation and fresh tracker evidence under the lock.
func (m *Manager) RetireCleanupLocalResources(ctx context.Context, n int, token string, p *CleanupPreview) error {
	unlock, err := m.cleanupLock()
	if err != nil {
		return err
	}
	defer unlock()
	for index := range p.Resources {
		r := &p.Resources[index]
		if !r.Eligible || (r.Kind != "worktree" && r.Kind != "local-code-branch") {
			continue
		}
		st, revision, err := m.reservedCleanup(ctx, n, token)
		if err != nil {
			return err
		}
		if m.gateway != nil {
			return fmt.Errorf("cleanup requires a repository operator")
		}
		if err = m.tracker.Authorize(ctx, "approve-task"); err != nil {
			return err
		}
		o, err := m.observeSubmission(ctx, st)
		if err != nil {
			return err
		}
		if reason := m.cleanupPolicy(ctx, st, o, true); reason != "" {
			return fmt.Errorf("cleanup refused: %s", reason)
		}
		if o.FinalHeadSHA != st.Cleanup.ExpectedHead || o.MergeCommitSHA != st.Cleanup.Evidence.MergeCommitSHA {
			return fmt.Errorf("cleanup evidence changed")
		}

		original := (r.Kind == "worktree" && r.Name == workspace.WorktreePath(m.repoRoot, n)) || (r.Kind == "local-code-branch" && r.Name == st.Branch)
		if original {
			if why := m.cleanupHeadReason(ctx, n, st, revision, r.ExpectedHead, st.Cleanup.ExpectedHead); why != "" {
				r.Eligible = false
				r.Reason = "retained: " + why
				continue
			}
		} else if r.ExpectedHead != st.Cleanup.ExpectedHead {
			return fmt.Errorf("cleanup evidence changed")
		}
		if r.Kind == "worktree" {
			branch := st.Branch
			if r.Name == workspace.PRWorktreePath(m.repoRoot, st.PRNumber) {
				branch = workspace.PRBranchName(st.PRNumber)
			}
			if r.Name != workspace.WorktreePath(m.repoRoot, n) && r.Name != workspace.PRWorktreePath(m.repoRoot, st.PRNumber) {
				return fmt.Errorf("unexpected cleanup worktree")
			}
			err = workspace.RetireCleanupWorktree(ctx, m.repoRoot, r.Name, branch, r.ExpectedHead)
		} else {
			if r.Name != st.Branch && r.Name != workspace.PRBranchName(st.PRNumber) {
				return fmt.Errorf("unexpected cleanup branch")
			}
			err = workspace.DeleteCleanupLocalBranch(ctx, m.repoRoot, r.Name, r.ExpectedHead)
		}
		r.Eligible = false
		if err != nil {
			r.Reason = "retained: " + err.Error()
		} else {
			r.Retired = true
			r.Reason = "retired"
		}
	}
	return nil
}
