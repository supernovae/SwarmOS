package lease

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"github.com/supernovae/swarmos/internal/testutil/gittransport"
	"github.com/supernovae/swarmos/internal/workspace"
)

func networkManager(t *testing.T) (*Manager, int, string) {
	t.Helper()
	m, _, issue := testManager(t)
	ctx := context.Background()
	remote := gitfixture.Init(t, "", "--bare", "--initial-branch=main")
	gitfixture.Run(t, m.repoRoot, "remote", "add", "origin", remote)
	gitfixture.Run(t, m.repoRoot, "push", "origin", "HEAD:refs/heads/main")
	m.SetStore(m.repoRoot, coordination.NewGitStore(m.repoRoot))
	if _, err := m.ApproveTask(ctx, issue.Number, "local-dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 3); err != nil {
		t.Fatal(err)
	}
	return m, issue.Number, remote
}
func TestMutateDoesNotRetryUnknownPublication(t *testing.T) {
	m, n, remote := networkManager(t)
	tr := gittransport.New(t, remote)
	tr.Configure(t, m.repoRoot, "origin")
	tr.Set("receive", "land-hang")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	tr.Watch(15*time.Second, func() { _, err = m.Renew(ctx, n, "local-dev", time.Hour) })
	if !errors.Is(err, coordination.ErrPublicationUnknown) {
		t.Errorf("renew: %v", err)
	}
	if tr.Receives() != 1 || !tr.Landed() {
		t.Errorf("renew retries=%d landed=%v", tr.Receives(), tr.Landed())
	}
	tr.Release()
	tr.Join()
	tr.Set("receive", "pass")
	st, err := m.GetCurrentState(context.Background(), n)
	if err != nil || st.Lease.Renewals != 1 {
		t.Errorf("renewal applied more than once: %+v %v", st, err)
	}
}
func TestRecordCheckpointLandedThenRetry(t *testing.T) {
	for _, successor := range []bool{false, true} {
		t.Run(map[bool]string{false: "landed", true: "successor"}[successor], func(t *testing.T) {
			t.Parallel()
			m, n, remote := networkManager(t)
			ctx := context.Background()
			st, err := m.GetCurrentState(ctx, n)
			if err != nil {
				t.Fatal(err)
			}
			path, branch, err := workspace.ProvisionAt(m.repoRoot, n, "test", st.BaseSHA, "", "")
			if err != nil {
				t.Fatal(err)
			}
			gitfixture.Run(t, path, "commit", "--allow-empty", "-m", "candidate")
			head := strings.TrimSpace(gitfixture.Run(t, path, "rev-parse", "HEAD"))
			tr := gittransport.New(t, remote)
			tr.Configure(t, m.repoRoot, "origin")
			tr.Set("receive", "land-hang")
			bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			tr.Watch(15*time.Second, func() { _, err = m.RecordCheckpoint(bounded, n, "local-dev", branch, head) })
			if !errors.Is(err, coordination.ErrPublicationUnknown) || !tr.Landed() {
				t.Fatalf("checkpoint: %v landed=%v", err, tr.Landed())
			}
			tr.Release()
			tr.Join()
			tr.Set("receive", "pass")
			current, landedRev, err := m.load(ctx, n)
			if err != nil || current.HeadSHA != head {
				t.Fatalf("checkpoint not authoritative: %+v %v", current, err)
			}
			if successor {
				// A second store writes a successor claim under the current authoritative lease.
				other := t.TempDir()
				gitfixture.Run(t, other, "clone", remote, ".")
				s := coordination.NewGitStore(other)
				current.Lease.ClaimID = "successor-claim"
				raw := mustNetworkJSON(t, current)
				if _, err = s.CompareAndSwap(ctx, n, landedRev, raw); err != nil {
					t.Fatal(err)
				}
			}
			_, err = m.RecordCheckpoint(ctx, n, "local-dev", branch, head)
			if successor {
				if !errors.Is(err, ErrClaimFence) {
					t.Errorf("successor was not fenced: %v", err)
				}
			} else if err != nil {
				t.Errorf("landed retry: %v", err)
			}
			if got := strings.TrimSpace(gitfixture.Run(t, remote, "rev-parse", "refs/heads/"+branch)); got != head {
				t.Error("remote checkpoint changed")
			}
			if got := strings.TrimSpace(gitfixture.Run(t, path, "rev-parse", "HEAD")); got != head {
				t.Error("candidate removed")
			}
			if !successor {
				after, rev, e := m.load(ctx, n)
				if e != nil || after.HeadSHA != head {
					t.Errorf("retry reload: %v", e)
				}
				parent := strings.TrimSpace(gitfixture.Run(t, m.repoRoot, "rev-parse", rev+"^"))
				if parent != landedRev {
					t.Error("retry applied more than once")
				}
			}
		})
	}
}
func TestCompletePRRevisionUnknownMessage(t *testing.T) {
	m, n, remote := networkManager(t)
	ctx := context.Background()
	base := strings.TrimSpace(gitfixture.Run(t, m.repoRoot, "rev-parse", "HEAD"))
	branch := "swarm/issue-1-test"
	gitfixture.Run(t, m.repoRoot, "push", "origin", base+":refs/heads/"+branch)
	st, rev, err := m.load(ctx, n)
	if err != nil {
		t.Fatal(err)
	}
	st.Branch = branch
	st.PRNumber = 37
	if _, err = m.save(ctx, n, rev, st); err != nil {
		t.Fatal(err)
	}
	if _, err = m.BeginPRRevision(ctx, n, "local-dev", 37, base, base, "swarm/pr-37"); err != nil {
		t.Fatal(err)
	}
	gitfixture.Run(t, m.repoRoot, "checkout", "-b", "swarm/pr-37")
	gitfixture.Run(t, m.repoRoot, "commit", "--allow-empty", "-m", "candidate")
	candidate := strings.TrimSpace(gitfixture.Run(t, m.repoRoot, "rev-parse", "HEAD"))
	if _, err = m.RecordPRHead(ctx, n, "local-dev", 37, "swarm/pr-37", candidate); err != nil {
		t.Fatal(err)
	}
	tr := gittransport.New(t, remote)
	tr.Configure(t, m.repoRoot, "origin")
	tr.Set("receive", "hang")
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tr.Watch(15*time.Second, func() { _, err = m.CompletePRRevision(bounded, n, "local-dev", 37, base, candidate) })
	if !errors.Is(err, coordination.ErrPublicationUnknown) || !strings.Contains(err.Error(), "rerun swarm pr submit") || strings.Contains(err.Error(), "without changing either remote ref") {
		t.Errorf("guidance: %v", err)
	}
}
func TestReconcileFetchNetworkBounded(t *testing.T) {
	repo := gitfixture.Init(t, "")
	remote := gitfixture.Init(t, "", "--bare")
	gitfixture.Run(t, repo, "remote", "add", "origin", remote)
	tr := gittransport.New(t, remote)
	tr.Configure(t, repo, "origin")
	tr.Set("upload", "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	tr.Watch(15*time.Second, func() { _, err = checkpointGit(ctx, filepath.Clean(repo), "fetch", "origin") })
	if !errors.Is(err, gitcredential.ErrInterrupted) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("fetch: %v", err)
	}
}

func mustNetworkJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
