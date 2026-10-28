package lease

import (
	"context"
	"errors"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
)

type discardFixture struct {
	m      *Manager
	lt     *tracker.LocalTracker
	issue  *tracker.Issue
	remote string
	git    func(dir string, args ...string) string
}

// newDiscardFixture approves a task on the local FileStore backend, or for the
// github variant on the real atomic GitStore in layout with a bare upstream
// remote. The FileStore ignores layout.
func newDiscardFixture(t *testing.T, github bool, layout coordination.RefLayout) discardFixture {
	t.Helper()
	m, lt, issue := testManager(t)
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := gitfixture.Command(t, "", append([]string{"-C", dir, "-c", "user.name=Test", "-c", "user.email=test@example.test"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	remote := ""
	if github {
		remote = filepath.Join(t.TempDir(), "remote.git")
		git(m.repoRoot, "init", "--bare", "--initial-branch=main", remote)
		git(m.repoRoot, "remote", "add", "origin", remote)
		main := git(m.repoRoot, "symbolic-ref", "--short", "HEAD")
		git(remote, "symbolic-ref", "HEAD", "refs/heads/"+main)
		git(m.repoRoot, "push", "origin", main)
		m.SetStore(m.repoRoot, coordination.NewGitStoreWithLayout(m.repoRoot, "", layout))
	}
	if _, err := m.ApproveTask(context.Background(), issue.Number, "local-dev"); err != nil {
		t.Fatal(err)
	}
	return discardFixture{m: m, lt: lt, issue: issue, remote: remote, git: git}
}

func (f discardFixture) provision(st *IssueState) (string, string, error) {
	remote, branch := workspace.ClaimCheckpointFrom(f.m.UpstreamRemote(), false, "", f.issue.Number, f.issue.Title, st.Lease.ClaimID, st.Branch)
	pc := st.ProvisionContext(false)
	pc.UpstreamRemote = f.m.UpstreamRemote()
	return workspace.ProvisionWithContext(f.m.repoRoot, remote, f.issue.Number, f.issue.Title, st.BaseSHA, branch, st.HeadSHA, pc)
}

// commit records a file change in the worktree and returns its SHA.
func (f discardFixture) commit(t *testing.T, path, name string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(path, name), []byte(name), 0o644); err != nil {
		t.Fatal(err)
	}
	f.git(path, "add", name)
	f.git(path, "commit", "-m", name)
	return f.git(path, "rev-parse", "HEAD")
}

// abandon reproduces #105: claim, checkpoint unique work C, release, edit the
// WorkUnit, and re-approve on a new base B that does not contain C.
func (f discardFixture) abandon(t *testing.T) (c, branch, base string) {
	t.Helper()
	ctx := context.Background()
	n := f.issue.Number
	st, err := f.m.Acquire(ctx, n, "local-dev", "mock", "plan", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	path, branch, err := f.provision(st)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []contract.Artifact{{Type: contract.ArtifactDesign, Producer: "mock", Summary: "plan", Body: "plan"}, {Type: contract.ArtifactTestPlan, Producer: "mock", Summary: "tests", Body: "tests"}} {
		if _, err := f.m.UpdateArtifact(ctx, n, "local-dev", a); err != nil {
			t.Fatal(err)
		}
	}
	c = f.commit(t, path, "abandoned.txt")
	if _, err := f.m.RecordCheckpoint(ctx, n, "local-dev", branch, c); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Release(ctx, n, "local-dev", "abandoned"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.lt.UpdateIssueBody(ctx, n, strings.Replace(testWorkUnit(), "Harden coordination state", "Harden coordination state again", 1)); err != nil {
		t.Fatal(err)
	}
	f.git(f.m.repoRoot, "commit", "--allow-empty", "-m", "new base")
	base = f.git(f.m.repoRoot, "rev-parse", "HEAD")
	if f.remote != "" {
		f.git(f.m.repoRoot, "push", "origin", "HEAD")
	}
	st, err = f.m.ApproveTask(ctx, n, "local-dev")
	if err != nil {
		t.Fatal(err)
	}
	if st.BaseSHA != base || st.HeadSHA != c {
		t.Fatalf("re-approval did not keep the divergent checkpoint: %+v", st)
	}
	return c, branch, base
}

func TestDiscardCheckpointRegression(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{"local", "github"} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newDiscardFixture(t, backend == "github", coordination.BranchLayout)
			n := f.issue.Number
			c, abandoned, base := f.abandon(t)

			// Before the discard, claim/work provisioning is stuck on C.
			st, err := f.m.Acquire(ctx, n, "local-dev", "mock", "plan", time.Hour, 1)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = f.provision(st)
			if err == nil || !strings.Contains(err.Error(), "does not descend from approved base") || !strings.Contains(err.Error(), "swarm issue discard-checkpoint "+strconv.Itoa(n)+" --expected-head "+c) {
				t.Fatalf("provisioning not refused with discard guidance: %v", err)
			}
			if _, err := f.m.Release(ctx, n, "local-dev", "claim setup failed"); err != nil {
				t.Fatal(err)
			}

			discarded, err := f.m.DiscardCheckpoint(ctx, n, "local-dev", c)
			if err != nil {
				t.Fatal(err)
			}
			ref := f.m.LedgerLayout().CheckpointRef(n, c)
			if len(discarded.DiscardedCheckpoints) != 1 {
				t.Fatalf("audit: %+v", discarded.DiscardedCheckpoints)
			}
			if d := discarded.DiscardedCheckpoints[0]; d.HeadSHA != c || d.Branch != abandoned || d.RetentionRef != ref || d.BaseSHA != base || d.DiscardedBy != "local-dev" || d.DiscardedAt.IsZero() {
				t.Fatalf("audit: %+v", d)
			}
			if discarded.HeadSHA != "" || discarded.FreshStart == nil || discarded.Branch == abandoned || discarded.Branch != discarded.FreshStart.Branch || len(discarded.Artifacts) != 0 || discarded.BaseSHA != base {
				t.Fatalf("not a fresh start: %+v", discarded)
			}
			retained := func() string {
				if backend == "github" {
					return f.git(f.remote, "rev-parse", ref)
				}
				return f.git(f.m.repoRoot, "rev-parse", ref)
			}
			if retained() != c {
				t.Fatal("abandoned checkpoint not retained")
			}
			if backend == "github" && !strings.HasPrefix(f.git(f.remote, "rev-parse", "refs/heads/"+abandoned), c) {
				t.Fatal("abandoned remote branch moved")
			}

			// After the discard, the next claim starts fresh from B.
			st, err = f.m.Acquire(ctx, n, "local-dev", "mock", "plan", time.Hour, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Artifacts) != 0 {
				t.Fatal("abandoned plan evidence carried into the new attempt")
			}
			path, branch, err := f.provision(st)
			if err != nil {
				t.Fatal(err)
			}
			if branch != discarded.Branch || f.git(path, "rev-parse", "HEAD") != base {
				t.Fatalf("fresh attempt on %s at %s", branch, f.git(path, "rev-parse", "HEAD"))
			}
			if _, err := os.Stat(filepath.Join(path, "abandoned.txt")); !os.IsNotExist(err) {
				t.Fatal("fresh attempt carries abandoned changes")
			}
			d := f.commit(t, path, "fresh.txt")
			recorded, err := f.m.RecordCheckpoint(ctx, n, "local-dev", branch, d)
			if err != nil {
				t.Fatal(err)
			}
			if recorded.HeadSHA != d || recorded.FreshStart != nil || len(recorded.DiscardedCheckpoints) != 1 {
				t.Fatalf("new checkpoint: %+v", recorded)
			}
			if backend == "github" && f.git(f.remote, "rev-parse", "refs/heads/"+branch) != d {
				t.Fatal("fresh checkpoint not published")
			}

			// Re-approval never revives the discarded checkpoint or drops history.
			if _, err := f.m.Release(ctx, n, "local-dev", "done"); err != nil {
				t.Fatal(err)
			}
			again, err := f.m.ApproveTask(ctx, n, "local-dev")
			if err != nil {
				t.Fatal(err)
			}
			if again.HeadSHA != d || len(again.DiscardedCheckpoints) != 1 || again.DroppedCheckpoint != nil {
				t.Fatalf("re-approval changed discard: %+v", again)
			}

			// The retention ref alone keeps C after its branch, worktree, and
			// reflogs are gone and objects are pruned.
			gcRepo := f.m.repoRoot
			if backend == "github" {
				gcRepo = f.remote
				f.git(f.remote, "update-ref", "-d", "refs/heads/"+abandoned)
			} else {
				archive := filepath.Join(filepath.Dir(path), "issue-"+strconv.Itoa(n)+"-discarded-"+c[:12])
				f.git(f.m.repoRoot, "worktree", "remove", "--force", archive)
				f.git(f.m.repoRoot, "branch", "-D", abandoned)
			}
			f.git(gcRepo, "reflog", "expire", "--expire=now", "--all")
			f.git(gcRepo, "gc", "--prune=now", "--quiet")
			f.git(gcRepo, "cat-file", "-e", c+"^{commit}")
			if retained() != c {
				t.Fatal("retention ref lost after gc")
			}
		})
	}
}

// TestDiscardCheckpointHiddenNamespace pins the audit and retention of a
// discard to refs/swarm/ and proves it writes nothing under refs/heads/.
func TestDiscardCheckpointHiddenNamespace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newDiscardFixture(t, true, coordination.HiddenLayout)
	n := f.issue.Number
	c, _, _ := f.abandon(t)
	branches := func() string {
		return f.git(f.remote, "for-each-ref", "--sort=refname", "--format=%(refname) %(objectname)", "refs/heads/")
	}
	before := branches()

	discarded, err := f.m.DiscardCheckpoint(ctx, n, "local-dev", c)
	if err != nil {
		t.Fatal(err)
	}
	ref := coordination.HiddenLayout.CheckpointRef(n, c)
	if len(discarded.DiscardedCheckpoints) != 1 || discarded.DiscardedCheckpoints[0].RetentionRef != ref {
		t.Fatalf("audit retention ref is not %s: %+v", ref, discarded.DiscardedCheckpoints)
	}
	if got := f.git(f.remote, "rev-parse", "--verify", ref); got != c {
		t.Fatalf("%s resolves to %q, want abandoned %s", ref, got, c)
	}
	if out := f.git(f.remote, "for-each-ref", coordination.BranchLayout.CheckpointPrefix()); out != "" {
		t.Fatalf("discard created branch-namespace retention refs:\n%s", out)
	}
	if after := branches(); after != before {
		t.Fatalf("discard changed refs/heads/:\n%s\n---\n%s", before, after)
	}
	st, err := f.m.GetCurrentState(ctx, n)
	if err != nil {
		t.Fatal(err)
	}
	if st.HeadSHA != "" || len(st.DiscardedCheckpoints) != 1 || st.DiscardedCheckpoints[0].RetentionRef != ref {
		t.Fatalf("active checkpoint not cleared: %+v", st)
	}
}

func TestDiscardCheckpointRefusals(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"active claim", "open PR", "pending PR revision", "submitted", "fork repo", "fork gateway", "unauthorized", "owner mismatch", "wrong expected head", "short expected head", "no checkpoint", "missing commit", "corrupt retention ref"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newDiscardFixture(t, false, coordination.BranchLayout)
			n := f.issue.Number
			c, _, _ := f.abandon(t)
			m, owner, expected := f.m, "local-dev", c
			edit := func(fn func(*IssueState)) {
				st, rev, err := m.load(ctx, n)
				if err != nil {
					t.Fatal(err)
				}
				fn(st)
				if _, err := m.save(ctx, n, rev, st); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "active claim":
				if _, err := m.Acquire(ctx, n, "local-dev", "mock", "plan", time.Hour, 1); err != nil {
					t.Fatal(err)
				}
			case "open PR":
				edit(func(st *IssueState) { st.PRNumber, st.PRURL = 9, "https://example.test/pull/9" })
			case "pending PR revision":
				edit(func(st *IssueState) { st.PRRevision = &PRRevision{Number: 9, Branch: st.Branch} })
			case "submitted":
				edit(func(st *IssueState) { st.SubmittedHeadSHA = c })
			case "fork repo":
				edit(func(st *IssueState) { st.ForkRepo = "contributor/fork" })
			case "fork gateway":
				m.SetWorkerGateway(refusingGateway{t})
			case "unauthorized":
				m = NewManager(f.lt.WithUser("local-dev", "NONE"), m.security)
				m.SetStore(f.m.repoRoot, f.m.store)
			case "owner mismatch":
				owner = "someone-else"
			case "wrong expected head":
				expected = strings.Repeat("a", 40)
			case "short expected head":
				expected = c[:12]
			case "no checkpoint":
				edit(func(st *IssueState) { st.HeadSHA, st.Branch = "", "" })
			case "missing commit":
				expected = strings.Repeat("1", 40)
				edit(func(st *IssueState) { st.HeadSHA = expected })
			case "corrupt retention ref":
				f.git(m.repoRoot, "update-ref", m.LedgerLayout().CheckpointRef(n, c), f.git(m.repoRoot, "rev-parse", "HEAD"))
			}
			before, _, err := f.m.store.Load(ctx, n)
			if err != nil {
				t.Fatal(err)
			}
			refsBefore := f.git(f.m.repoRoot, "for-each-ref", "refs/heads")
			if _, err := m.DiscardCheckpoint(ctx, n, owner, expected); err == nil {
				t.Fatal("discard was not refused")
			} else if scenario == "active claim" && !errors.Is(err, ErrAlreadyClaimed) {
				t.Fatalf("active claim error: %v", err)
			} else if scenario == "corrupt retention ref" && !errors.Is(err, coordination.ErrConflict) {
				t.Fatalf("corrupt ref error: %v", err)
			}
			after, _, err := f.m.store.Load(ctx, n)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatal("refused discard changed state")
			}
			if refsAfter := f.git(f.m.repoRoot, "for-each-ref", "refs/heads"); refsAfter != refsBefore {
				t.Fatalf("refused discard changed refs:\n%s\n%s", refsBefore, refsAfter)
			}
		})
	}
}

func TestDiscardCheckpointExpiredClaimIsEligible(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newDiscardFixture(t, false, coordination.BranchLayout)
	c, _, _ := f.abandon(t)
	st, err := f.m.Acquire(ctx, f.issue.Number, "local-dev", "mock", "plan", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	f.m.SetNowFunc(func() time.Time { return st.Lease.ExpiresAt.Add(time.Second) })
	if _, err := f.m.DiscardCheckpoint(ctx, f.issue.Number, "local-dev", c); err != nil {
		t.Fatal(err)
	}
}

func TestDiscardCheckpointConflictIsOneWrite(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{"local", "github"} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newDiscardFixture(t, backend == "github", coordination.BranchLayout)
			n := f.issue.Number
			c, _, _ := f.abandon(t)
			racing := &racingRetainingStore{Store: f.m.store}
			f.m.SetStore(f.m.repoRoot, racing)
			_, err := f.m.DiscardCheckpoint(ctx, n, "local-dev", c)
			if !errors.Is(err, coordination.ErrConflict) {
				t.Fatalf("stale discard not a conflict: %v", err)
			}
			if racing.retaining != 1 || racing.plain != 1 {
				t.Fatalf("discard made %d retaining and %d extra writes", racing.retaining, racing.plain-1)
			}
			st, err := f.m.GetCurrentState(ctx, n)
			if err != nil {
				t.Fatal(err)
			}
			if st.HeadSHA != c || st.FreshStart != nil || len(st.DiscardedCheckpoints) != 0 || st.NextPhaseReason != "concurrent winner" {
				t.Fatalf("stale discard overwrote the winner: %+v", st)
			}
		})
	}
}

// racingRetainingStore lands a concurrent state write just before the
// discard's own write and counts every write the discard attempts.
type racingRetainingStore struct {
	coordination.Store
	retaining, plain int
}

func (s *racingRetainingStore) CompareAndSwap(ctx context.Context, n int, old string, next []byte) (string, error) {
	s.plain++
	return s.Store.CompareAndSwap(ctx, n, old, next)
}

func (s *racingRetainingStore) CompareAndSwapRetaining(ctx context.Context, n int, old string, next []byte, heads ...string) (string, error) {
	s.retaining++
	raw, rev, err := s.Store.Load(ctx, n)
	if err != nil {
		return "", err
	}
	winner := strings.Replace(string(raw), `"lease":`, `"next_phase_reason":"concurrent winner","lease":`, 1)
	if _, err := s.CompareAndSwap(ctx, n, rev, []byte(winner)); err != nil {
		return "", err
	}
	return s.Store.(coordination.RetainingStore).CompareAndSwapRetaining(ctx, n, old, next, heads...)
}
