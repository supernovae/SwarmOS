package lease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"net/http"
	"net/http/httptest"
	"strconv"
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

type decompositionRollupTracker struct {
	tracker.Tracker
	prs          map[int]*tracker.PullRequest
	offline      bool
	closeFail    bool
	onPR         func(int)
	issueOffline bool
	parent       int
}

func (tr *decompositionRollupTracker) GetPullRequest(_ context.Context, n int) (*tracker.PullRequest, error) {
	if tr.onPR != nil {
		tr.onPR(n)
	}
	if tr.offline {
		return nil, errors.New("API offline")
	}
	return tr.prs[n], nil
}
func (tr *decompositionRollupTracker) GetIssue(ctx context.Context, n int) (*tracker.Issue, error) {
	if tr.issueOffline && n != tr.parent {
		return nil, errors.New("tracker offline")
	}
	return tr.Tracker.GetIssue(ctx, n)
}
func (tr *decompositionRollupTracker) CloseIssueCompleted(ctx context.Context, n int) error {
	if tr.closeFail {
		return errors.New("close unavailable")
	}
	return tr.Tracker.(interface {
		CloseIssueCompleted(context.Context, int) error
	}).CloseIssueCompleted(ctx, n)
}
func (tr *decompositionRollupTracker) UpstreamRepo() string { return "octo/swarm" }
func decompositionRollupFixture(t *testing.T) (*Manager, *decompositionRollupTracker, int, []int) {
	t.Helper()
	m, lt, parent := testManager(t)
	tr := &decompositionRollupTracker{Tracker: lt, prs: map[int]*tracker.PullRequest{}, parent: parent.Number}
	m.tracker = tr
	st, rev, err := m.load(t.Context(), parent.Number)
	if err != nil {
		t.Fatal(err)
	}
	st.Decomposition = &Decomposition{ID: "operation", Actor: "local-dev", Complete: true}
	st.Lease.Status = "decomposed"
	var children []int
	for i := 0; i < 2; i++ {
		wu := &contract.WorkUnit{ID: "child", Objective: "Deliver child", Acceptance: []string{"Child works"}, Workflow: "feature", ParentIssue: parent.Number}
		issue, err := lt.CreateIssue(t.Context(), "child", contract.FormatIssueMarkdown(wu), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = lt.LinkSubIssue(t.Context(), parent.Number, issue.Number); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		head := strings.Repeat("a", 40)
		branch := fmt.Sprintf("swarm/issue-%d-child", issue.Number)
		url := fmt.Sprintf("https://github.com/octo/swarm/pull/%d", issue.Number)
		child := &IssueState{Lease: contract.Lease{IssueNumber: issue.Number, Status: "completed", Phase: "submitted"}, PRNumber: issue.Number, PRURL: url, Branch: branch, SubmittedHeadSHA: head, BaseBranch: "main"}
		raw, _ := json.Marshal(child)
		if _, err = m.store.CompareAndSwap(t.Context(), issue.Number, "", raw); err != nil {
			t.Fatal(err)
		}
		tr.prs[issue.Number] = &tracker.PullRequest{Number: issue.Number, HTMLURL: url, Head: branch, HeadSHA: head, Base: "main", HeadRepo: "octo/swarm", BaseRepo: "octo/swarm", State: "closed", Merged: true, MergeCommitSHA: strings.Repeat("b", 40), MergedAt: &now, IssueNumber: issue.Number}
		st.Decomposition.Children = append(st.Decomposition.Children, DecompositionChild{Number: issue.Number, WorkUnit: *wu})
		children = append(children, issue.Number)
	}
	raw, _ := json.Marshal(st)
	if _, err = m.store.CompareAndSwap(t.Context(), parent.Number, rev, raw); err != nil {
		t.Fatal(err)
	}
	return m, tr, parent.Number, children
}
func TestDecompositionReconcileMergeEvidence(t *testing.T) {
	for _, scenario := range []string{"merged", "closed-unmerged", "open", "superseded", "missing sha", "missing time", "wrong issue", "wrong branch", "wrong repo", "wrong base", "wrong head", "offline", "active attempt"} {
		t.Run(scenario, func(t *testing.T) {
			m, tr, parent, children := decompositionRollupFixture(t)
			p := tr.prs[children[1]]
			switch scenario {
			case "closed-unmerged", "superseded":
				p.Merged = false
				p.MergedAt = nil
				p.MergeCommitSHA = ""
			case "open":
				p.State = "open"
				p.Merged = false
				p.MergedAt = nil
			case "missing sha":
				p.MergeCommitSHA = ""
			case "missing time":
				p.MergedAt = nil
			case "wrong issue":
				p.IssueNumber = 99
			case "wrong branch":
				p.Head = "unrelated"
			case "wrong repo":
				p.HeadRepo = "attacker/repo"
			case "wrong base":
				p.Base = "other"
			case "wrong head":
				p.HeadSHA = strings.Repeat("c", 40)
			case "offline":
				tr.offline = true
			case "active attempt":
				child, rev, _ := m.load(t.Context(), children[1])
				child.Lease.Status = "active"
				raw, _ := json.Marshal(child)
				_, _ = m.store.CompareAndSwap(t.Context(), children[1], rev, raw)
			}
			before, childRev, _ := m.store.Load(t.Context(), children[0])
			discovered, err := m.DecompositionParent(t.Context(), children[0])
			if err != nil || discovered != parent {
				t.Fatalf("discover: %d %v", discovered, err)
			}
			_, err = m.ReconcileDecomposition(t.Context(), parent)
			if (err == nil) != (scenario == "merged") {
				t.Fatalf("result: %v", err)
			}
			issue, _ := tr.Tracker.GetIssue(t.Context(), parent)
			if (issue.State == "closed") != (scenario == "merged") {
				t.Fatal(issue.State)
			}
			after, afterRev, _ := m.store.Load(t.Context(), children[0])
			if string(before) != string(after) || childRev != afterRev {
				t.Fatal("rollup changed child")
			}
			if scenario == "merged" {
				if _, err = m.ReconcileDecomposition(t.Context(), parent); err != nil {
					t.Fatal(err)
				}
				if issue.StateReason != "completed" {
					t.Fatal(issue)
				}
			}
		})
	}
}
func TestDecompositionReconcileCloseRetry(t *testing.T) {
	m, tr, parent, _ := decompositionRollupFixture(t)
	tr.closeFail = true
	if _, err := m.ReconcileDecomposition(t.Context(), parent); err == nil {
		t.Fatal("close failure ignored")
	}
	st, _, _ := m.load(t.Context(), parent)
	if !st.Decomposition.CompletionIntent || st.Decomposition.Closed {
		t.Fatal(st.Decomposition)
	}
	tr.closeFail = false
	if _, err := m.ReconcileDecomposition(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
}

type decompositionHTTPTransport func(*http.Request) (*http.Response, error)

func (f decompositionHTTPTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestDecompositionGitHubReconcile(t *testing.T) {
	for _, scenario := range []string{"merged", "closed-unmerged", "open", "superseded", "offline", "missing time", "close failure"} {
		t.Run(scenario, func(t *testing.T) {
			m, tr, parent, children := decompositionRollupFixture(t)
			p := tr.prs[children[1]]
			switch scenario {
			case "closed-unmerged", "superseded":
				p.Merged = false
				p.MergedAt = nil
			case "open":
				p.State = "open"
				p.Merged = false
				p.MergedAt = nil
			case "missing time":
				p.MergedAt = nil
			}
			client, err := api.NewRESTClient(api.ClientOptions{Host: "github.com", AuthToken: "fixture", Transport: decompositionHTTPTransport(func(r *http.Request) (*http.Response, error) {
				rec := httptest.NewRecorder()
				rec.Header().Set("Content-Type", "application/json")
				write := func(value any) {
					if err := json.NewEncoder(rec).Encode(value); err != nil {
						t.Fatal(err)
					}
				}
				path := r.URL.Path
				switch {
				case path == "/user":
					write(map[string]string{"login": "local-dev"})
				case strings.Contains(path, "/collaborators/"):
					write(map[string]string{"permission": "write"})
				case strings.Contains(path, "/git/ref/heads/"):
					write(map[string]any{"object": map[string]string{"sha": strings.Repeat("a", 40)}})
				case strings.Contains(path, "/pulls/"):
					if scenario == "offline" {
						return nil, errors.New("offline")
					}
					n, _ := strconv.Atoi(path[strings.LastIndex(path, "/")+1:])
					p := tr.prs[n]
					write(map[string]any{"number": p.Number, "html_url": p.HTMLURL, "state": p.State, "merged": p.Merged, "merge_commit_sha": p.MergeCommitSHA, "merged_at": p.MergedAt, "head": map[string]any{"ref": p.Head, "sha": p.HeadSHA, "repo": map[string]string{"full_name": p.HeadRepo}}, "base": map[string]any{"ref": p.Base, "repo": map[string]string{"full_name": p.BaseRepo}}})
				case strings.HasSuffix(path, "/parent"):
					write(map[string]int{"number": parent})
				case strings.Contains(path, "/issues/"):
					n, _ := strconv.Atoi(path[strings.LastIndex(path, "/")+1:])
					if r.Method == "PATCH" {
						if scenario == "close failure" {
							rec.WriteHeader(503)
							write(map[string]string{"message": "unavailable"})
							break
						}
						if err := tr.CloseIssueCompleted(r.Context(), n); err != nil {
							t.Fatal(err)
						}
					}
					issue, err := tr.Tracker.GetIssue(r.Context(), n)
					if err != nil {
						t.Fatal(err)
					}
					write(map[string]any{"number": issue.Number, "title": issue.Title, "body": issue.Body, "state": issue.State, "state_reason": issue.StateReason})
				default:
					t.Fatalf("unexpected HTTP request %s %s", r.Method, path)
				}
				response := rec.Result()
				response.Request = r
				return response, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			m.tracker = tracker.NewGitHubTrackerWithClient("octo", "swarm", client)
			discovered, err := m.DecompositionParent(t.Context(), children[0])
			if err != nil || discovered != parent {
				t.Fatalf("lookup: %d %v", discovered, err)
			}
			_, err = m.ReconcileDecomposition(t.Context(), parent)
			if (err == nil) != (scenario == "merged") {
				t.Fatalf("reconcile: %v", err)
			}
			issue, _ := tr.Tracker.GetIssue(t.Context(), parent)
			if (issue.State == "closed") != (scenario == "merged") {
				t.Fatal(issue.State)
			}
		})
	}
}

func TestDecompositionReconcileCrossChildRevisionChange(t *testing.T) {
	m, tr, parent, children := decompositionRollupFixture(t)
	original, beforeRev, err := m.store.Load(t.Context(), children[0])
	if err != nil {
		t.Fatal(err)
	}
	tr.onPR = func(n int) {
		if n != children[1] {
			return
		}
		child, rev, err := m.load(t.Context(), children[0])
		if err != nil {
			t.Fatal(err)
		}
		child.Lease.Status = "active"
		child.Lease.ClaimID = "successor"
		raw, _ := json.Marshal(child)
		if _, err = m.store.CompareAndSwap(t.Context(), children[0], rev, raw); err != nil {
			t.Fatal(err)
		}
	}
	_, err = m.ReconcileDecomposition(t.Context(), parent)
	if !errors.Is(err, coordination.ErrConflict) {
		t.Fatalf("expected cross-child conflict, got %v", err)
	}
	st, _, _ := m.load(t.Context(), parent)
	issue, _ := tr.GetIssue(t.Context(), parent)
	if st.Decomposition.CompletionIntent || st.Decomposition.Closed || issue.State != "open" {
		t.Fatal("stale snapshot completed parent")
	}
	after, afterRev, _ := m.store.Load(t.Context(), children[0])
	if beforeRev == afterRev || string(original) == string(after) {
		t.Fatal("injection did not change child")
	}
	child, _, _ := m.load(t.Context(), children[0])
	if child.Lease.ClaimID != "successor" || child.Lease.Status != "active" {
		t.Fatal("rollup changed successor claim")
	}
}

func TestDecompositionAdmissionOpenChildrenAndFallback(t *testing.T) {
	m, tr, parent, children := decompositionRollupFixture(t)
	if err := tr.CloseIssueCompleted(t.Context(), children[0]); err != nil {
		t.Fatal(err)
	}
	st, _, _ := m.load(t.Context(), parent)
	before, rev, err := m.store.Load(t.Context(), parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, offline := range []bool{false, true} {
		tr.issueOffline = offline
		want := fmt.Sprintf("open child issues [%d]", children[1])
		if offline {
			want = fmt.Sprintf("tracker state unavailable; recorded child issues %v", children)
		}
		for _, entry := range []string{"preflight", "claim", "fork claim"} {
			var err error
			switch entry {
			case "preflight":
				err = m.DecompositionAdmission(t.Context(), st)
			case "claim":
				_, err = m.Acquire(t.Context(), parent, "local-dev", "mock", "plan", time.Hour, 1)
			case "fork claim":
				_, err = m.AcquireFork(t.Context(), parent, "local-dev", "mock", "plan", "contributor/repo", time.Hour, 1)
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("offline=%t entry=%s: %v", offline, entry, err)
			}
		}
	}
	after, afterRev, err := m.store.Load(t.Context(), parent)
	if err != nil || rev != afterRev || string(before) != string(after) {
		t.Fatal("refusal changed parent state", err)
	}
}
