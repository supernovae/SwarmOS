package lease

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
)

// countingCleanupTracker keeps the upstream identity of lifecycleTracker so
// snapshot previews cannot fall back to local-backend semantics.
type countingCleanupTracker struct {
	*lifecycleTracker
	listed     []*tracker.PullRequest
	listErr    error
	getPRs     int
	listPRs    int
	listIssues int
}

func (c *countingCleanupTracker) GetPullRequest(ctx context.Context, n int) (*tracker.PullRequest, error) {
	c.getPRs++
	return c.lifecycleTracker.GetPullRequest(ctx, n)
}

func (c *countingCleanupTracker) ListPullRequests(_ context.Context, filter tracker.PullRequestFilter) ([]*tracker.PullRequest, error) {
	c.listPRs++
	if c.listErr != nil {
		return nil, c.listErr
	}
	var out []*tracker.PullRequest
	for _, pr := range append([]*tracker.PullRequest{c.pr}, c.listed...) {
		if filter.State == "all" || pr.State == filter.State || (filter.State == "" && pr.State == "open") {
			copy := *pr
			out = append(out, &copy)
		}
	}
	return out, nil
}

func (c *countingCleanupTracker) ListIssues(ctx context.Context, filter tracker.IssueFilter) ([]*tracker.Issue, error) {
	c.listIssues++
	return c.lifecycleTracker.Tracker.ListIssues(ctx, filter)
}

func countingCleanupFixture(t *testing.T) (*Manager, *countingCleanupTracker, int) {
	t.Helper()
	m, lt, n, _ := mergedCleanupFixture(t)
	c := &countingCleanupTracker{lifecycleTracker: lt}
	m.tracker = c
	return m, c, n
}

func TestCleanupBulkSnapshotMatchesFreshInspection(t *testing.T) {
	m, c, n := countingCleanupFixture(t)
	ctx := context.Background()
	if _, ok := m.tracker.(interface{ UpstreamRepo() string }); !ok || m.cleanupLocal() {
		t.Fatal("double lost upstream identity")
	}
	fresh, err := InspectCleanup(ctx, m.repoRoot, m.tracker, m, n)
	if err != nil {
		t.Fatal(err)
	}
	c.getPRs, c.listPRs = 0, 0
	snap := m.SnapshotCleanup(ctx)
	for range 3 {
		if _, err = InspectCleanupSnapshot(ctx, m.repoRoot, m, n, snap); err != nil {
			t.Fatal(err)
		}
	}
	got, err := InspectCleanupSnapshot(ctx, m.repoRoot, m, n, snap)
	if err != nil {
		t.Fatal(err)
	}
	if c.getPRs != 0 || c.listPRs != 1 {
		t.Fatalf("snapshot preview made per-issue lookups: get=%d list=%d", c.getPRs, c.listPRs)
	}
	got.Outcome.ObservedAt = fresh.Outcome.ObservedAt
	if !reflect.DeepEqual(got, fresh) {
		t.Fatalf("snapshot preview differs:\n%+v\n%+v", got, fresh)
	}
	if !got.RemoteApplicable || !got.Resources[0].Eligible {
		t.Fatalf("merged remote branch not eligible: %+v", got.Resources[0])
	}
}

func TestCleanupBulkSnapshotFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*countingCleanupTracker)
		blocked string
		err     string
	}{
		{name: "listing error", mutate: func(c *countingCleanupTracker) { c.listErr = errors.New("rate limited") }, err: "rate limited"},
		{name: "missing PR", mutate: func(c *countingCleanupTracker) {
			// Only a different PR is listed; the recorded one is absent.
			c.listed = []*tracker.PullRequest{{Number: 99, State: "closed", Head: "other"}}
			c.pr = &tracker.PullRequest{Number: 98, State: "closed"}
		}, err: "absent from the tracker listing"},
		{name: "open PR uses branch", mutate: func(c *countingCleanupTracker) {
			c.listed = []*tracker.PullRequest{{Number: 40, State: "open", Head: c.pr.Head, HeadRepo: "octo/swarm"}}
		}, blocked: "branch is used by an open PR"},
		{name: "open PR incomplete identity", mutate: func(c *countingCleanupTracker) {
			c.listed = []*tracker.PullRequest{{Number: 41, State: "open", Head: "unrelated"}}
		}, blocked: "open PR repository identity is incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, c, n := countingCleanupFixture(t)
			ctx := context.Background()
			tc.mutate(c)
			p, err := InspectCleanupSnapshot(ctx, m.repoRoot, m, n, m.SnapshotCleanup(ctx))
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("want error %q, got %v", tc.err, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.Blocked != tc.blocked {
				t.Fatalf("blocked %q, want %q", p.Blocked, tc.blocked)
			}
			for _, r := range p.Resources {
				if r.Eligible {
					t.Fatalf("blocked preview granted eligibility: %+v", r)
				}
			}
		})
	}
}

func TestCleanupBulkAuthoritativeLinks(t *testing.T) {
	m, c, n := countingCleanupFixture(t)
	ctx := context.Background()
	// PR 25 is the issue's recorded PR; its closing reference is a valid hint.
	c.pr.Body = "Closes #1"
	// PR 30 forges a link to the same issue, which records PR 25 instead.
	c.listed = []*tracker.PullRequest{{Number: 30, State: "closed", Body: "Fixes #1", IssueNumber: n}}
	root := m.repoRoot
	worktrees := []workspace.CleanupWorktree{
		{Path: workspace.WorktreePath(root, n), Kind: "issue", Number: n, Present: true},
		{Path: workspace.PRWorktreePath(root, 25), Kind: "pr", Number: 25, Present: true},
		{Path: workspace.PRWorktreePath(root, 30), Kind: "pr", Number: 30, Present: true},
		{Path: workspace.PRWorktreePath(root, 31), Kind: "pr", Number: 31, Present: true},
		{Path: workspace.WorktreePath(root, 77), Kind: "issue", Number: 77, Present: true},
		{Path: root + "/.swarm/worktrees/scratch", Present: true},
	}
	snap := m.SnapshotCleanup(ctx)
	candidates, unresolved := m.ResolveCleanupWorktrees(ctx, snap, worktrees)
	if len(candidates) != 1 || candidates[0].IssueNumber != n || len(candidates[0].Worktrees) != 2 {
		t.Fatalf("candidates: %+v", candidates)
	}
	reasons := map[string]string{}
	for _, u := range unresolved {
		reasons[u.Path] = u.Reason
	}
	want := map[string]string{
		workspace.PRWorktreePath(root, 30): "no verified issue link",
		workspace.PRWorktreePath(root, 31): "no verified issue link",
		workspace.WorktreePath(root, 77):   "no authoritative coordination state",
		root + "/.swarm/worktrees/scratch": "unrecognized managed worktree name",
	}
	if !reflect.DeepEqual(reasons, want) {
		t.Fatalf("unresolved: %+v", reasons)
	}
	if c.listIssues != 1 || c.listPRs != 1 || c.getPRs != 0 {
		t.Fatalf("lookups not batched: issues=%d listPRs=%d getPRs=%d", c.listIssues, c.listPRs, c.getPRs)
	}

	// A PR worktree alone resolves through its hint without listing issues.
	c.listIssues = 0
	candidates, unresolved = m.ResolveCleanupWorktrees(ctx, snap, worktrees[1:2])
	if len(candidates) != 1 || candidates[0].IssueNumber != n || len(unresolved) != 0 || c.listIssues != 0 {
		t.Fatalf("hinted link: %+v %+v issues=%d", candidates, unresolved, c.listIssues)
	}
}
