package lease

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/coordination"
)

func mergedCleanupFixture(t *testing.T) (*Manager, *lifecycleTracker, int, string) {
	t.Helper()
	m, tr, n := lifecycleFixture(t)
	ctx := context.Background()
	git := func(args ...string) string {
		t.Helper()
		out, e := exec.Command("git", append([]string{"-C", m.repoRoot}, args...)...).CombinedOutput()
		if e != nil {
			t.Fatalf("git %v: %s %v", args, out, e)
		}
		return strings.TrimSpace(string(out))
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	git("init", "--bare", remote)
	git("remote", "add", "origin", remote)
	git("config", "remote.origin.swarmSlug", "octo/swarm")
	branch := git("symbolic-ref", "--short", "HEAD")
	git("push", "origin", "HEAD:refs/heads/"+branch)
	git("--git-dir="+remote, "symbolic-ref", "HEAD", "refs/heads/"+branch)
	st, _, e := m.load(ctx, n)
	if e != nil {
		t.Fatal(e)
	}
	st.HeadSHA = git("rev-parse", "HEAD")
	tr.pr.HeadSHA = st.HeadSHA
	tr.pr.Base = branch
	stamp := time.Now().UTC()
	tr.pr.State = "closed"
	tr.pr.Merged = true
	tr.pr.MergedAt = &stamp
	tr.pr.MergeCommitSHA = strings.Repeat("b", 40)
	git("push", "origin", st.HeadSHA+":refs/heads/"+st.Branch)
	m.SetStore(m.repoRoot, coordination.NewGitStore(m.repoRoot))
	raw, _ := json.Marshal(st)
	if _, e = m.store.CompareAndSwap(ctx, n, "", raw); e != nil {
		t.Fatal(e)
	}
	return m, tr, n, remote
}

func TestCleanupApplyMergedSubmissionRetainsLocalArtifacts(t *testing.T) {
	m, _, n, remote := mergedCleanupFixture(t)
	ctx := context.Background()
	// Preserve both untracked and ignored active artifacts byte for byte.
	path := filepath.Join(m.repoRoot, ".swarm", "worktrees", "issue-1", ".swarm", "active", "PLAN.md")
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(path, []byte("task evidence"), 0600); e != nil {
		t.Fatal(e)
	}
	p, e := InspectCleanup(ctx, m.repoRoot, m.tracker, m, n)
	if e != nil {
		t.Fatal(e)
	}
	if !p.Resources[0].Eligible {
		t.Fatalf("merged branch ineligible: %+v", p.Resources[0])
	}
	for _, r := range p.Resources[1:] {
		if r.Eligible {
			t.Fatal("local resource unexpectedly eligible")
		}
	}
	r, e := m.BeginCleanup(ctx, n, p.Outcome)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = m.AcquireFork(ctx, n, "local-dev", "mock", "plan", "fork/repo", time.Hour, 1); !errors.Is(e, ErrClaimFence) {
		t.Fatalf("fork reservation lost fence: %v", e)
	}
	if e = m.DeleteCleanupBranch(ctx, n, r.OperationID); e != nil {
		t.Fatal(e)
	}
	if e = m.FinishCleanup(ctx, n, r.OperationID); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(path)
	if e != nil || string(b) != "task evidence" {
		t.Fatal("cleanup discarded artifacts")
	}
	out, e := exec.Command("git", "--git-dir="+remote, "for-each-ref", "refs/heads/swarm/issue-1-fixture").CombinedOutput()
	if e != nil || len(out) != 0 {
		t.Fatalf("remote branch survived: %s %v", out, e)
	}
	st, e := m.GetCurrentState(ctx, n)
	if e != nil || st.PROutcome.Status != "merged" || st.PRNumber != 25 || len(st.CleanupHistory) != 1 || !st.CleanupHistory[0].RemoteDeleted {
		t.Fatalf("history unreadable: %+v %v", st, e)
	}
	repeated, e := InspectCleanup(ctx, m.repoRoot, m.tracker, m, n)
	if e != nil || !repeated.Resources[0].Absent {
		t.Fatalf("repeat cleanup not absent: %+v %v", repeated, e)
	}
}

func TestCleanupEligibilityAdvancedBranchAndChangedPRHead(t *testing.T) {
	for _, kind := range []string{"branch", "pr"} {
		t.Run(kind, func(t *testing.T) {
			m, tr, n, _ := mergedCleanupFixture(t)
			ctx := context.Background()
			if kind == "pr" {
				tr.pr.HeadSHA = strings.Repeat("c", 40)
			} else {
				for _, args := range [][]string{{"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "advance"}, {"push", "origin", "HEAD:refs/heads/" + tr.pr.Head}} {
					c := exec.Command("git", args...)
					c.Dir = m.repoRoot
					if out, e := c.CombinedOutput(); e != nil {
						t.Fatalf("%s %v", out, e)
					}
				}
			}
			p, e := InspectCleanup(ctx, m.repoRoot, m.tracker, m, n)
			if e != nil {
				t.Fatal(e)
			}
			if p.Resources[0].Eligible {
				t.Fatal("advanced evidence eligible")
			}
		})
	}
}
