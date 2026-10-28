package lease

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/tracker"
)

type lifecycleTracker struct {
	tracker.Tracker
	pr          *tracker.PullRequest
	failComment bool
}

func (t *lifecycleTracker) GetIssue(ctx context.Context, n int) (*tracker.Issue, error) {
	issue, e := t.Tracker.GetIssue(ctx, n)
	if e != nil {
		return nil, e
	}
	copy := *issue
	copy.State = "closed"
	return &copy, nil
}
func (t *lifecycleTracker) GetPullRequest(context.Context, int) (*tracker.PullRequest, error) {
	return t.pr, nil
}
func (t *lifecycleTracker) UpdateComment(ctx context.Context, id int64, body string) (*tracker.Comment, error) {
	if t.failComment {
		return nil, errors.New("projection unavailable")
	}
	return t.Tracker.UpdateComment(ctx, id, body)
}
func (t *lifecycleTracker) UpstreamRepo() string { return "octo/swarm" }
func (t *lifecycleTracker) ListPullRequests(context.Context, tracker.PullRequestFilter) ([]*tracker.PullRequest, error) {
	return nil, nil
}

func lifecycleFixture(t *testing.T) (*Manager, *lifecycleTracker, int) {
	t.Helper()
	m, lt, iss := testManager(t)
	ctx := context.Background()
	st, rev, e := m.load(ctx, iss.Number)
	if e != nil {
		t.Fatal(e)
	}
	st.PRNumber = 25
	st.PRURL = "https://github.com/octo/swarm/pull/25"
	st.Branch = "swarm/issue-1-fixture"
	st.HeadSHA = strings.Repeat("a", 40)
	st.Lease.Status = "completed"
	st.Lease.Phase = "submitted"
	raw, _ := json.Marshal(st)
	if _, e = m.store.CompareAndSwap(ctx, iss.Number, rev, raw); e != nil {
		t.Fatal(e)
	}
	tr := &lifecycleTracker{Tracker: lt, pr: &tracker.PullRequest{Number: 25, HTMLURL: st.PRURL, Head: st.Branch, HeadSHA: st.HeadSHA, Base: "main", HeadRepo: "octo/swarm", BaseRepo: "octo/swarm", State: "open"}}
	m.tracker = tr
	return m, tr, iss.Number
}
func TestReconcilePROutcomeLegacyAndIdempotence(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"open", "merged", "closed-unmerged"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			m, tr, n := lifecycleFixture(t)
			ctx := context.Background()
			if outcome != "open" {
				tr.pr.State = "closed"
			}
			if outcome == "merged" {
				stamp := time.Now().UTC()
				tr.pr.Merged = true
				tr.pr.MergedAt = &stamp
				tr.pr.MergeCommitSHA = strings.Repeat("b", 40)
			}
			// Closing the issue is deliberately independent of PR classification.
			st, e := m.ReconcileSubmission(ctx, n)
			if e != nil {
				t.Fatal(e)
			}
			if st.PROutcome.Status != outcome || st.SubmittedHeadSHA != strings.Repeat("a", 40) || st.Lease.Phase != "submitted" || st.PRNumber != 25 {
				t.Fatalf("lost evidence: %+v", st)
			}
			_, rev, _ := m.store.Load(ctx, n)
			if _, e = m.ReconcileSubmission(ctx, n); e != nil {
				t.Fatal(e)
			}
			_, again, _ := m.store.Load(ctx, n)
			if again != rev {
				t.Fatal("unchanged observation rewrote state")
			}
			comments, e := tr.ListComments(ctx, n)
			if e != nil {
				t.Fatal(e)
			}
			if len(comments) != 1 {
				t.Fatalf("duplicate audit projections: %d", len(comments))
			}
			iss, _ := tr.GetIssue(ctx, n)
			found := false
			for _, l := range iss.Labels {
				found = found || l == "swarm:pr-open"
			}
			if found != (outcome == "open") {
				t.Fatal("stale PR label")
			}
		})
	}
}
func TestReconcileProjectionRetry(t *testing.T) {
	t.Parallel()
	m, tr, n := lifecycleFixture(t)
	tr.failComment = true
	st, e := m.ReconcileSubmission(context.Background(), n)
	if e == nil || st.PROutcome == nil {
		t.Fatalf("partial failure hidden: %v", e)
	}
	_, rev, _ := m.store.Load(context.Background(), n)
	tr.failComment = false
	if _, e = m.ReconcileSubmission(context.Background(), n); e != nil {
		t.Fatal(e)
	}
	_, again, _ := m.store.Load(context.Background(), n)
	if rev != again {
		t.Fatal("repair rewrote authoritative state")
	}
}
func TestReconcileRejectsIdentityAndActiveAttempts(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"repo", "branch", "number", "url", "active", "pending"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			m, tr, n := lifecycleFixture(t)
			switch kind {
			case "repo":
				tr.pr.HeadRepo = "other/repo"
			case "branch":
				tr.pr.Head = "other"
			case "number":
				tr.pr.Number++
			case "url":
				tr.pr.HTMLURL = "other"
			case "active", "pending":
				st, rev, _ := m.load(context.Background(), n)
				if kind == "active" {
					st.Lease.Status = "active"
				} else {
					st.PRRevision = &PRRevision{Number: 25}
				}
				raw, _ := json.Marshal(st)
				if _, e := m.store.CompareAndSwap(context.Background(), n, rev, raw); e != nil {
					t.Fatal(e)
				}
			}
			_, rev, _ := m.store.Load(context.Background(), n)
			if _, e := m.ReconcileSubmission(context.Background(), n); e == nil {
				t.Fatal("unsafe reconciliation accepted")
			}
			_, after, _ := m.store.Load(context.Background(), n)
			if after != rev {
				t.Fatal("refusal mutated state")
			}
		})
	}
}
func TestCleanupReservationFencesAcquireAndStaleToken(t *testing.T) {
	t.Parallel()
	m, tr, n, _ := mergedCleanupFixture(t)
	ctx := context.Background()
	stamp := time.Now()
	tr.pr.State = "closed"
	tr.pr.Merged = true
	tr.pr.MergedAt = &stamp
	tr.pr.MergeCommitSHA = strings.Repeat("b", 40)
	_, o, e := m.InspectSubmission(ctx, n)
	if e != nil {
		t.Fatal(e)
	}
	r, e := m.BeginCleanup(ctx, n, o)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = m.Acquire(ctx, n, "local-dev", "mock", "plan", time.Hour, 1); !errors.Is(e, ErrClaimFence) {
		t.Fatalf("reservation did not fence acquire: %v", e)
	}
	if _, e = m.ApproveTask(ctx, n, "local-dev"); e == nil {
		t.Fatal("approval overwrote cleanup")
	}
	if e = m.FinishCleanup(ctx, n, "stale"); !errors.Is(e, ErrClaimFence) {
		t.Fatal(e)
	}
	if st, e := m.GetCurrentState(ctx, n); e != nil || st.Cleanup == nil {
		t.Fatal("reservation hid status")
	}
	if e = m.FinishCleanup(ctx, n, r.OperationID); e != nil {
		t.Fatal(e)
	}
	if e = m.FinishCleanup(ctx, n, r.OperationID); !errors.Is(e, ErrClaimFence) {
		t.Fatal("old operation retained authority")
	}
}

func TestCleanupRecoveryFencesOldOperation(t *testing.T) {
	t.Parallel()
	m, tr, n, _ := mergedCleanupFixture(t)
	ctx := context.Background()
	stamp := time.Now()
	tr.pr.State = "closed"
	tr.pr.Merged = true
	tr.pr.MergedAt = &stamp
	tr.pr.MergeCommitSHA = strings.Repeat("b", 40)
	_, o, e := m.InspectSubmission(ctx, n)
	if e != nil {
		t.Fatal(e)
	}
	r, e := m.BeginCleanup(ctx, n, o)
	if e != nil {
		t.Fatal(e)
	}
	if e = m.RecoverCleanup(ctx, n); e != nil {
		t.Fatal(e)
	}
	if e = m.DeleteCleanupBranch(ctx, n, r.OperationID); !errors.Is(e, ErrClaimFence) {
		t.Fatalf("recovered operation retained authority: %v", e)
	}
	st, e := m.GetCurrentState(ctx, n)
	if e != nil || st.Cleanup != nil || len(st.CleanupHistory) != 1 {
		t.Fatalf("history lost: %+v %v", st, e)
	}
}

func TestCleanupPreviewRetainsArtifactsAndLedgers(t *testing.T) {
	t.Parallel()
	m, _, n := lifecycleFixture(t)
	ctx := context.Background()
	before, rev, e := m.store.Load(ctx, n)
	if e != nil {
		t.Fatal(e)
	}
	p, e := InspectCleanup(ctx, m.repoRoot, m.tracker, m, n)
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Resources) != 7 {
		t.Fatalf("missing resource classes: %+v", p)
	}
	for _, r := range p.Resources {
		if r.Eligible {
			t.Fatal("open PR eligible")
		}
		if r.Kind == "ledger" && !strings.Contains(r.Reason, "retained") {
			t.Fatal("ledger not retained")
		}
	}
	after, again, _ := m.store.Load(ctx, n)
	if string(before) != string(after) || rev != again {
		t.Fatal("preview changed authoritative state")
	}
}

func TestReconcileOperatorAuthorization(t *testing.T) {
	t.Parallel()
	m, tr, n := lifecycleFixture(t)
	tr.Tracker = tr.Tracker.(*tracker.LocalTracker).WithUser("outsider", "NONE")
	_, before, _ := m.store.Load(context.Background(), n)
	if _, e := m.ReconcileSubmission(context.Background(), n); e == nil {
		t.Fatal("unauthorized reconciliation accepted")
	}
	_, after, _ := m.store.Load(context.Background(), n)
	if before != after {
		t.Fatal("permission failure changed state")
	}
}
