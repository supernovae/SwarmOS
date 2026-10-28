package lease

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/supernovae/swarmos/internal/filelock"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
		out, e := gitfixture.Command(t, "", append([]string{"-C", m.repoRoot}, args...)...).CombinedOutput()
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
	// Keep the foreign issue directory while retiring an independent registered
	// PR worktree on the GitHub/atomic-remote path.
	prPath := workspace.PRWorktreePath(m.repoRoot, 25)
	branch := workspace.PRBranchName(25)
	c := gitfixture.Command(t, "", "-C", m.repoRoot, "worktree", "add", "-b", branch, prPath, "HEAD")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	p, e := InspectCleanup(ctx, m.repoRoot, m.tracker, m, n)
	if e != nil {
		t.Fatal(e)
	}
	if !p.Resources[0].Eligible {
		t.Fatalf("merged branch ineligible: %+v", p.Resources[0])
	}
	eligible := 0
	for _, resource := range p.Resources[1:] {
		if resource.Eligible {
			eligible++
		}
	}
	if eligible != 2 {
		t.Fatalf("registered clean PR resources not eligible: %+v", p.Resources)
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
	if e = m.RetireCleanupLocalResources(ctx, n, r.OperationID, p); e != nil {
		t.Fatal(e)
	}
	if _, err := os.Stat(prPath); !os.IsNotExist(err) {
		t.Fatalf("registered PR worktree survived: %v", err)
	}
	if e = m.FinishCleanup(ctx, n, r.OperationID); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(path)
	if e != nil || string(b) != "task evidence" {
		t.Fatal("cleanup discarded artifacts")
	}
	out, e := gitfixture.Command(t, "", "--git-dir="+remote, "for-each-ref", "refs/heads/swarm/issue-1-fixture").CombinedOutput()
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
					c := gitfixture.Command(t, "", args...)
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

type cleanupIdentityTracker struct {
	*lifecycleTracker
	upstream string
}

func (t *cleanupIdentityTracker) UpstreamRepo() string { return t.upstream }

func TestCleanupGitHubIdentityRefusal(t *testing.T) {
	for _, kind := range []string{"head-empty", "base-empty", "both-empty", "upstream-empty", "origin-mismatch", "valid"} {
		t.Run(kind, func(t *testing.T) {
			for _, entry := range []string{"preview", "apply"} {
				t.Run(entry, func(t *testing.T) {
					m, tr, n, _ := mergedCleanupFixture(t)
					ctx := context.Background()
					_, observed, err := m.InspectSubmission(ctx, n)
					if err != nil {
						t.Fatal(err)
					}
					provider := &cleanupIdentityTracker{lifecycleTracker: tr, upstream: "octo/swarm"}
					m.tracker = provider
					switch kind {
					case "head-empty":
						tr.pr.HeadRepo = ""
					case "base-empty":
						tr.pr.BaseRepo = ""
					case "both-empty":
						tr.pr.HeadRepo = ""
						tr.pr.BaseRepo = ""
					case "upstream-empty":
						provider.upstream = ""
					case "origin-mismatch":
						c := gitfixture.Command(t, "", "-C", m.repoRoot, "config", "remote.origin.swarmSlug", "other/repo")
						if out, e := c.CombinedOutput(); e != nil {
							t.Fatalf("%s %v", out, e)
						}
					}
					before, rev, err := m.store.Load(ctx, n)
					if err != nil {
						t.Fatal(err)
					}
					if entry == "preview" {
						p, e := InspectCleanup(ctx, m.repoRoot, tr, m, n)
						err = e
						if e == nil && p.Blocked != "" {
							err = errors.New(p.Blocked)
						}
					} else {
						_, err = m.BeginCleanup(ctx, n, observed)
					}
					if kind == "valid" {
						if err != nil {
							t.Fatal(err)
						}
						return
					}
					if err == nil {
						t.Fatal("invalid GitHub identity accepted")
					}
					after, next, e := m.store.Load(ctx, n)
					if e != nil || next != rev || string(after) != string(before) {
						t.Fatal("identity refusal mutated state")
					}
					if _, err = os.Stat(filepath.Join(m.repoRoot, ".git", "swarm-cleanup.lock")); !os.IsNotExist(err) {
						t.Fatal("identity refusal acquired local mutation lock")
					}
				})
			}
		})
	}
}

type cleanupLocalTracker struct {
	tracker.Tracker
	pr      *tracker.PullRequest
	barrier func()
}

func (t *cleanupLocalTracker) GetPullRequest(context.Context, int) (*tracker.PullRequest, error) {
	return t.pr, nil
}
func (t *cleanupLocalTracker) ListPullRequests(context.Context, tracker.PullRequestFilter) ([]*tracker.PullRequest, error) {
	if t.barrier != nil {
		t.barrier()
	}
	return nil, nil
}

func TestCleanupLocalActionRecoverySerialization(t *testing.T) {
	m, tr, n, _ := mergedCleanupFixture(t)
	ctx := context.Background()
	local := &cleanupLocalTracker{Tracker: tr.Tracker, pr: tr.pr}
	m.tracker = local
	p, err := InspectCleanup(ctx, m.repoRoot, local, m, n)
	if err != nil {
		t.Fatal(err)
	}
	r, err := m.BeginCleanup(ctx, n, p.Outcome)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	local.barrier = func() { once.Do(func() { close(entered); <-release }) }
	// One eligible absent branch suffices to exercise the synchronous action lock.
	p.Resources = []CleanupResource{{Kind: "local-code-branch", Name: tr.pr.Head, ExpectedHead: r.ExpectedHead, Eligible: true}}
	actionDone := make(chan error, 1)
	go func() { actionDone <- m.RetireCleanupLocalResources(ctx, n, r.OperationID, p) }()
	<-entered
	dir, err := cleanupGitOutput(ctx, m.repoRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(strings.TrimSpace(dir), "swarm-cleanup.lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = filelock.TryLockExclusive(f); err == nil {
		filelock.Unlock(f)
		t.Fatal("local action does not hold operation lock")
	}
	f.Close()
	recovered := make(chan error, 1)
	go func() { recovered <- m.RecoverCleanup(ctx, n) }()
	close(release)
	if err = <-actionDone; err != nil {
		t.Fatal(err)
	}
	if err = <-recovered; err != nil {
		t.Fatal(err)
	}
	p.Resources[0].Eligible = true
	if err = m.RetireCleanupLocalResources(ctx, n, r.OperationID, p); !errors.Is(err, ErrClaimFence) {
		t.Fatalf("old local action retained authority: %v", err)
	}
}

type cleanupFinishFailStore struct{ coordination.Store }

func (s cleanupFinishFailStore) CompareAndSwap(ctx context.Context, n int, rev string, raw []byte) (string, error) {
	var st IssueState
	if err := json.Unmarshal(raw, &st); err != nil {
		return "", err
	}
	if st.Cleanup == nil {
		return "", errors.New("injected finish failure")
	}
	return s.Store.CompareAndSwap(ctx, n, rev, raw)
}
func TestCleanupFinishFailurePreservesReservation(t *testing.T) {
	m, _, n, _ := mergedCleanupFixture(t)
	ctx := context.Background()
	p, err := InspectCleanup(ctx, m.repoRoot, m.tracker, m, n)
	if err != nil {
		t.Fatal(err)
	}
	r, err := m.BeginCleanup(ctx, n, p.Outcome)
	if err != nil {
		t.Fatal(err)
	}
	m.store = cleanupFinishFailStore{m.store}
	if err = m.FinishCleanup(ctx, n, r.OperationID); err == nil {
		t.Fatal("finish failure hidden")
	}
	st, _, err := m.reservedCleanup(ctx, n, r.OperationID)
	if err != nil || st.Cleanup == nil {
		t.Fatal("finish failure lost reservation")
	}
}
