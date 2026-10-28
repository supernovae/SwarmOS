package lease

import (
	"context"
	"errors"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/workspace"
)

// Exercise the approval/acquire/provision/publication boundary with the real
// atomic Git store and a local bare remote, including a publication-time race.
func TestDroppedCheckpointPublicationFence(t *testing.T) {
	for _, scenario := range []string{"publish", "remote moved", "stale claim", "state conflict"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			m, _, issue := testManager(t)
			git := func(args ...string) string {
				t.Helper()
				cmd := gitfixture.Command(t, "", args...)
				cmd.Dir = m.repoRoot
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			remote := filepath.Join(t.TempDir(), "remote.git")
			git("init", "--bare", "--initial-branch=main", remote)
			git("remote", "add", "origin", remote)
			// The fixture may use a different default branch; advertise its branch.
			main := git("symbolic-ref", "--short", "HEAD")
			git("--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/"+main)
			git("push", "origin", main)
			m.SetStore(m.repoRoot, coordination.NewGitStore(m.repoRoot))
			if _, err := m.ApproveTask(ctx, issue.Number, "local-dev"); err != nil {
				t.Fatal(err)
			}
			st, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 1)
			if err != nil {
				t.Fatal(err)
			}
			old := st.BaseSHA
			path, branch, err := workspace.ProvisionAt(m.repoRoot, issue.Number, issue.Title, old, "", "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.RecordCheckpoint(ctx, issue.Number, "local-dev", branch, old); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Release(ctx, issue.Number, "local-dev", "test"); err != nil {
				t.Fatal(err)
			}
			git("-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "--allow-empty", "-m", "new base")
			base := git("rev-parse", "HEAD")
			git("push", "origin", main)
			approved, err := m.ApproveTask(ctx, issue.Number, "local-dev")
			if err != nil {
				t.Fatal(err)
			}
			if approved.DroppedCheckpoint == nil {
				t.Fatal("approval retained empty checkpoint")
			}
			if workspaceHead := git("rev-parse", branch); workspaceHead != old {
				t.Fatal("approval moved branch")
			}
			claimed, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 1)
			if err != nil {
				t.Fatal(err)
			}
			if claimed.DroppedCheckpoint == nil {
				t.Fatal("acquire lost recovery")
			}
			gotPath, _, err := workspace.ProvisionFromRemoteAt(m.repoRoot, "origin", issue.Number, "renamed", base, "", "", claimed.DroppedCheckpoint)
			if err != nil {
				t.Fatal(err)
			}
			if gotPath != path {
				t.Fatal("changed worktree")
			}
			before, err := m.GetCurrentState(ctx, issue.Number)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "remote moved" {
				git("push", "origin", base+":refs/heads/"+branch)
			}
			if scenario == "stale claim" {
				m.BindClaim(issue.Number, "stale")
			}
			if scenario == "state conflict" {
				// Replace the coordination revision with a concurrent, valid state write.
				m.SetStore(m.repoRoot, &conflictingRecoveryStore{GitStore: coordination.NewGitStore(m.repoRoot)})
			}
			result, err := m.RecordCheckpoint(ctx, issue.Number, "local-dev", branch, base)
			if scenario == "publish" {
				if err != nil {
					t.Fatal(err)
				}
				if result.DroppedCheckpoint != nil || result.HeadSHA != base {
					t.Fatal("recovery not consumed")
				}
				if !strings.HasPrefix(git("ls-remote", "origin", "refs/heads/"+branch), base) {
					t.Fatal("branch not published")
				}
				return
			}
			if err == nil {
				t.Fatal("publication escaped fence")
			}
			if !errors.Is(err, ErrClaimFence) {
				t.Fatalf("lost fence error: %v", err)
			}
			after, err := m.GetCurrentState(ctx, issue.Number)
			if err != nil {
				t.Fatal(err)
			}
			if after.DroppedCheckpoint == nil || after.DroppedCheckpoint.HeadSHA != old || after.HeadSHA != before.HeadSHA {
				t.Fatal("failed publication consumed recovery")
			}
			expected := old
			if scenario == "remote moved" {
				expected = base
			}
			if !strings.HasPrefix(git("ls-remote", "origin", "refs/heads/"+branch), expected) {
				t.Fatal("failed publication moved branch")
			}
		})
	}
}

type conflictingRecoveryStore struct{ *coordination.GitStore }

func (s *conflictingRecoveryStore) CompareAndSwapAndPublishExpected(ctx context.Context, n int, old string, next []byte, branch, head, expected string) (string, error) {
	raw, rev, err := s.Load(ctx, n)
	if err != nil {
		return "", err
	}
	if _, err := s.CompareAndSwap(ctx, n, rev, append(raw, ' ')); err != nil {
		return "", err
	}
	return s.GitStore.CompareAndSwapAndPublishExpected(ctx, n, old, next, branch, head, expected)
}
