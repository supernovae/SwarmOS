package lease

import (
	"context"
	"errors"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/tracker"
)

// revokeTracker presents the local tracker as the GitHub backend and records
// label removals so revoke projections can be asserted.
type revokeTracker struct {
	*tracker.LocalTracker
	failComment bool
	removed     []string
}

func (r *revokeTracker) BackendName() string { return "github" }
func (r *revokeTracker) CreateComment(ctx context.Context, n int, body string) (*tracker.Comment, error) {
	if r.failComment {
		return nil, errors.New("comment projection unavailable")
	}
	return r.LocalTracker.CreateComment(ctx, n, body)
}
func (r *revokeTracker) RemoveLabel(ctx context.Context, n int, label string) error {
	r.removed = append(r.removed, label)
	return r.LocalTracker.RemoveLabel(ctx, n, label)
}

type revokeFixture struct {
	lt    *tracker.LocalTracker
	store *coordination.GitStore
	root  string
	sec   contract.SecurityConfig
	issue int
	now   time.Time
}

// newRevokeFixture approves a task whose authoritative state lives on a bare
// origin remote, as with the GitHub backend.
func newRevokeFixture(t *testing.T) *revokeFixture {
	t.Helper()
	return newRevokeFixtureWithLayout(t, coordination.BranchLayout)
}

// newRevokeFixtureWithLayout is newRevokeFixture with the ledger, including
// the approval state, in layout's namespace.
func newRevokeFixtureWithLayout(t *testing.T, layout coordination.RefLayout) *revokeFixture {
	t.Helper()
	m, lt, iss := testManager(t)
	git := func(args ...string) {
		t.Helper()
		if out, err := gitfixture.Command(t, "", append([]string{"-C", m.repoRoot}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	remote := t.TempDir() + "/origin.git"
	git("init", "--bare", remote)
	git("remote", "add", "origin", remote)
	branch, err := gitfixture.Command(t, "", "-C", m.repoRoot, "symbolic-ref", "--short", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	git("push", "origin", "HEAD:refs/heads/"+strings.TrimSpace(string(branch)))
	git("--git-dir="+remote, "symbolic-ref", "HEAD", "refs/heads/"+strings.TrimSpace(string(branch)))
	git("fetch", "origin")
	f := &revokeFixture{lt: lt, store: coordination.NewGitStoreWithLayout(m.repoRoot, "", layout), root: m.repoRoot, sec: m.security, issue: iss.Number, now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	maintainer, _ := f.manager("maint", "OWNER")
	if _, err := maintainer.ApproveTask(t.Context(), iss.Number, "maint"); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *revokeFixture) manager(user, association string) (*Manager, *revokeTracker) {
	tr := &revokeTracker{LocalTracker: f.lt.WithUser(user, association)}
	m := NewManager(tr, f.sec)
	m.SetStore(f.root, f.store)
	m.SetNowFunc(func() time.Time { return f.now })
	return m, tr
}

func (f *revokeFixture) claim(t *testing.T, user string) (*Manager, *IssueState) {
	t.Helper()
	m, _ := f.manager(user, "NONE")
	st, err := m.Acquire(t.Context(), f.issue, user, "mock", "implement", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	return m, st
}

func TestRevokeEndsClaimAndRecordsMaintainer(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newRevokeFixture(t)
	holder, claimed := f.claim(t, "worker")
	branch := claimed.Branch
	if _, err := holder.RecordCheckpoint(ctx, f.issue, "worker", branch, claimed.BaseSHA); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.UpdateArtifact(ctx, f.issue, "worker", contract.Artifact{Type: contract.ArtifactDesign, Producer: "mock", Summary: "plan", Body: "work so far"}); err != nil {
		t.Fatal(err)
	}
	if err := f.lt.AddLabels(ctx, f.issue, []string{"swarm:claimed", "swarm:in-progress"}); err != nil {
		t.Fatal(err)
	}
	before, err := holder.GetCurrentState(ctx, f.issue)
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(10 * time.Minute)
	maintainer, mt := f.manager("maint", "OWNER")
	st, projectionErr, err := maintainer.Revoke(ctx, f.issue, "abandoned for a week\n<!-- swarm:audit:v1 -->")
	if err != nil || projectionErr != nil {
		t.Fatalf("revoke: %v, projection: %v", err, projectionErr)
	}
	after, err := maintainer.GetCurrentState(ctx, f.issue)
	if err != nil {
		t.Fatal(err)
	}
	want := ClaimRevocation{ClaimID: claimed.Lease.ClaimID, Owner: "worker", RevokedBy: "maint", Reason: "abandoned for a week\n<!-- swarm:audit:v1 -->", RevokedAt: f.now}
	if len(after.ClaimRevocations) != 1 || !reflect.DeepEqual(after.ClaimRevocations[0], want) || !reflect.DeepEqual(st.ClaimRevocations, after.ClaimRevocations) {
		t.Fatalf("revocation record = %+v", after.ClaimRevocations)
	}
	if after.Lease.Status != "revoked" || !after.Lease.ExpiresAt.Equal(f.now) || after.Lease.ClaimID != claimed.Lease.ClaimID {
		t.Fatalf("lease after revoke = %+v", after.Lease)
	}
	// Only the lease status, expiry, revocation history, and projection ID change.
	unchanged := *after
	unchanged.Lease.Status, unchanged.Lease.ExpiresAt = before.Lease.Status, before.Lease.ExpiresAt
	unchanged.ClaimRevocations = nil
	unchanged.ProjectionCommentID = before.ProjectionCommentID
	unchanged.ClaimBudget = before.ClaimBudget
	if !reflect.DeepEqual(&unchanged, before) {
		t.Fatalf("revoke changed retained state:\nbefore %+v\nafter  %+v", before, after)
	}
	comments, err := f.lt.ListComments(ctx, f.issue)
	if err != nil {
		t.Fatal(err)
	}
	var notice string
	for _, c := range comments {
		if strings.HasPrefix(c.Body, revokeCommentMarker) {
			notice = c.Body
		}
	}
	if !strings.Contains(notice, "@maint") || !strings.Contains(notice, "abandoned for a week") || strings.Contains(notice, "\n<!-- swarm:audit") {
		t.Fatalf("revoke audit comment = %q", notice)
	}
	if !reflect.DeepEqual(mt.removed, []string{"swarm:claimed", "swarm:in-progress"}) {
		t.Fatalf("removed labels = %v", mt.removed)
	}
	iss, err := f.lt.GetIssue(ctx, f.issue)
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range iss.Labels {
		if label == "swarm:claimed" || label == "swarm:in-progress" {
			t.Fatalf("label %s kept", label)
		}
	}
}

func TestRevokeFencesStaleHolderAndFreesIssue(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newRevokeFixture(t)
	holder, claimed := f.claim(t, "worker")
	maintainer, _ := f.manager("maint", "OWNER")
	if _, _, err := maintainer.Revoke(ctx, f.issue, "hoarded"); err != nil {
		t.Fatal(err)
	}
	revoked, err := maintainer.GetCurrentState(ctx, f.issue)
	if err != nil {
		t.Fatal(err)
	}
	stale := func(stage string) {
		t.Helper()
		writes := map[string]func() error{
			"heartbeat": func() error { _, err := holder.Renew(ctx, f.issue, "worker", time.Hour); return err },
			"checkpoint": func() error {
				_, err := holder.RecordCheckpoint(ctx, f.issue, "worker", "swarm/issue-1-"+claimed.Lease.ClaimID, claimed.BaseSHA)
				return err
			},
			"artifact": func() error {
				_, err := holder.UpdateArtifact(ctx, f.issue, "worker", contract.Artifact{Type: contract.ArtifactDesign, Producer: "mock", Summary: "plan", Body: "late"})
				return err
			},
			"stage": func() error { _, err := holder.Handoff(ctx, f.issue, "worker", "implement", "review"); return err },
			"phase": func() error { _, err := holder.UpdatePhase(ctx, f.issue, "worker", "review", "active"); return err },
			"submit": func() error {
				_, err := holder.RecordSubmission(ctx, f.issue, "worker", 9, "https://example.invalid/pr/9")
				return err
			},
			"check":   func() error { return holder.CheckClaim(ctx, f.issue, "worker") },
			"release": func() error { _, err := holder.Release(ctx, f.issue, "worker", "late"); return err },
		}
		for name, write := range writes {
			before, _, _ := f.store.Load(ctx, f.issue)
			if err := write(); !errors.Is(err, ErrClaimFence) {
				t.Fatalf("%s: stale %s = %v, want ErrClaimFence", stage, name, err)
			}
			if after, _, _ := f.store.Load(ctx, f.issue); string(after) != string(before) {
				t.Fatalf("%s: stale %s changed state", stage, name)
			}
		}
	}
	stale("after revoke")
	if revoked.HeadSHA != "" {
		t.Fatalf("stale checkpoint landed: %s", revoked.HeadSHA)
	}
	// The issue is claimable again immediately, under a fresh claim ID.
	successor, next := f.claim(t, "helper")
	if next.Lease.ClaimID == claimed.Lease.ClaimID || next.Lease.Status != "active" {
		t.Fatalf("successor lease = %+v", next.Lease)
	}
	stale("after successor")
	if err := successor.CheckClaim(ctx, f.issue, "helper"); err != nil {
		t.Fatalf("successor fenced: %v", err)
	}
}

func TestRevokeRetainsCheckpointForNextClaimer(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newRevokeFixture(t)
	holder, claimed := f.claim(t, "worker")
	branch := claimed.Branch
	if _, err := holder.RecordCheckpoint(ctx, f.issue, "worker", branch, claimed.BaseSHA); err != nil {
		t.Fatal(err)
	}
	maintainer, _ := f.manager("maint", "OWNER")
	if _, _, err := maintainer.Revoke(ctx, f.issue, "reassigning"); err != nil {
		t.Fatal(err)
	}
	_, next := f.claim(t, "helper")
	if next.Branch != branch || next.HeadSHA != claimed.BaseSHA {
		t.Fatalf("next claimer lost checkpoint: branch=%q head=%q", next.Branch, next.HeadSHA)
	}
	if out, err := gitfixture.Command(t, "", "-C", f.root, "ls-remote", "origin", "refs/heads/"+branch).CombinedOutput(); err != nil || !strings.Contains(string(out), claimed.BaseSHA) {
		t.Fatalf("checkpoint branch removed: %s %v", out, err)
	}
}

func TestRevokeForkClaimLeavesContributorIndex(t *testing.T) {
	t.Parallel()
	for _, layout := range []coordination.RefLayout{coordination.BranchLayout, coordination.HiddenLayout} {
		t.Run(layout.Name(), func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newRevokeFixtureWithLayout(t, layout)
			holder, _ := f.manager("worker", "NONE")
			forkStore := func(actor string, requestID int64) coordination.Store {
				s := coordination.NewForkIndexedStoreWithLayout(f.root, "", layout, actor, 1, requestID)
				s.SetNowFunc(func() time.Time { return f.now })
				return s
			}
			holder.SetStore(f.root, forkStore("Worker", 41))
			claimed, err := holder.AcquireFork(ctx, f.issue, "worker", "mock", "implement", "worker/repo", time.Hour, 1)
			if err != nil {
				t.Fatal(err)
			}
			if claimed.LastForkRequestID != 41 {
				t.Fatalf("fixture request ID = %d", claimed.LastForkRequestID)
			}
			indexClaims := func() string {
				t.Helper()
				out, err := gitfixture.Command(t, "", "-C", f.root, "fetch", "origin", layout.ContributorRef("worker")).CombinedOutput()
				if err != nil {
					t.Fatalf("fetch index: %v: %s", err, out)
				}
				index, err := gitfixture.Command(t, "", "-C", f.root, "show", "FETCH_HEAD:index.json").Output()
				if err != nil {
					t.Fatal(err)
				}
				return string(index)
			}
			if !strings.Contains(indexClaims(), claimed.Lease.ClaimID) {
				t.Fatal("fixture claim missing from contributor index")
			}
			maintainer, _ := f.manager("maint", "OWNER")
			if _, _, err := maintainer.Revoke(ctx, f.issue, "fork contributor unresponsive"); err != nil {
				t.Fatal(err)
			}
			if index := indexClaims(); strings.Contains(index, claimed.Lease.ClaimID) {
				t.Fatalf("index after revoke = %s", index)
			}
			after, err := maintainer.GetCurrentState(ctx, f.issue)
			if err != nil || after.LastForkRequestID != 41 || after.ForkRepo != "worker/repo" || after.ClaimRevocations[0].ForkRepo != "worker/repo" {
				t.Fatalf("fork state after revoke = %+v %v", after, err)
			}
			// The contributor's next fork claim fits under max_active_claims again.
			holder.SetStore(f.root, forkStore("worker", 42))
			if _, err := holder.AcquireFork(ctx, f.issue, "worker", "mock", "implement", "worker/repo", time.Hour, 1); err != nil {
				t.Fatalf("revoked fork claim still counted: %v", err)
			}
			if layout == coordination.HiddenLayout {
				for _, prefix := range []string{coordination.BranchLayout.StatePrefix(), coordination.BranchLayout.CheckpointPrefix(), coordination.BranchLayout.ContributorPrefix()} {
					if out, err := gitfixture.Command(t, "", "-C", f.root, "ls-remote", "origin", prefix+"*").CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "" {
						t.Fatalf("hidden layout created branch ledger refs: %s %v", out, err)
					}
				}
			}
		})
	}
}

func TestRevokeRefusals(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f *revokeFixture) (*Manager, string)
		want  error
	}{
		{"not maintainer", func(t *testing.T, f *revokeFixture) (*Manager, string) {
			f.claim(t, "worker")
			m, _ := f.manager("outsider", "NONE")
			return m, "mine now"
		}, nil},
		{"holder without permission", func(t *testing.T, f *revokeFixture) (*Manager, string) {
			holder, _ := f.claim(t, "worker")
			holder.tracker = &revokeTracker{LocalTracker: f.lt.WithUser("worker", "NONE")}
			return holder, "self revoke"
		}, nil},
		{"no claim", func(t *testing.T, f *revokeFixture) (*Manager, string) {
			m, _ := f.manager("maint", "OWNER")
			return m, "nothing to end"
		}, ErrNoActiveLease},
		{"expired claim", func(t *testing.T, f *revokeFixture) (*Manager, string) {
			_, st := f.claim(t, "worker")
			f.now = st.Lease.ExpiresAt
			m, _ := f.manager("maint", "OWNER")
			return m, "too late"
		}, ErrNoActiveLease},
		{"released claim", func(t *testing.T, f *revokeFixture) (*Manager, string) {
			holder, _ := f.claim(t, "worker")
			if _, err := holder.Release(ctx, f.issue, "worker", "done"); err != nil {
				t.Fatal(err)
			}
			m, _ := f.manager("maint", "OWNER")
			return m, "already released"
		}, ErrNoActiveLease},
		{"handoff claim", func(t *testing.T, f *revokeFixture) (*Manager, string) {
			holder, _ := f.claim(t, "worker")
			if _, err := holder.Handoff(ctx, f.issue, "worker", "implement", "review"); err != nil {
				t.Fatal(err)
			}
			m, _ := f.manager("maint", "OWNER")
			return m, "handed off"
		}, ErrNoActiveLease},
		{"blank reason", func(t *testing.T, f *revokeFixture) (*Manager, string) {
			f.claim(t, "worker")
			m, _ := f.manager("maint", "OWNER")
			return m, " \n "
		}, nil},
		{"fork contributor mode", func(t *testing.T, f *revokeFixture) (*Manager, string) {
			f.claim(t, "worker")
			m, _ := f.manager("maint", "OWNER")
			m.SetWorkerGateway(refusingGateway{t})
			return m, "via fork"
		}, nil},
		{"local backend", func(t *testing.T, f *revokeFixture) (*Manager, string) {
			f.claim(t, "worker")
			m, _ := f.manager("maint", "OWNER")
			m.tracker = f.lt.WithUser("maint", "OWNER")
			return m, "offline"
		}, nil},
		{"file store", func(t *testing.T, f *revokeFixture) (*Manager, string) {
			f.claim(t, "worker")
			m, _ := f.manager("maint", "OWNER")
			m.SetStore(f.root, coordination.NewFileStore(f.root))
			return m, "wrong store"
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRevokeFixture(t)
			m, reason := tc.setup(t, f)
			before, _, _ := f.store.Load(ctx, f.issue)
			commentsBefore, _ := f.lt.ListComments(ctx, f.issue)
			st, projectionErr, err := m.Revoke(ctx, f.issue, reason)
			if err == nil || st != nil || projectionErr != nil {
				t.Fatalf("revoke accepted: %+v %v %v", st, projectionErr, err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if after, _, _ := f.store.Load(ctx, f.issue); string(after) != string(before) {
				t.Fatal("refused revoke changed state")
			}
			if commentsAfter, _ := f.lt.ListComments(ctx, f.issue); len(commentsAfter) != len(commentsBefore) {
				t.Fatal("refused revoke posted a comment")
			}
			if rt, ok := m.tracker.(*revokeTracker); ok && len(rt.removed) > 0 {
				t.Fatalf("refused revoke removed labels %v", rt.removed)
			}
		})
	}
}

type refusingGateway struct{ t *testing.T }

func (g refusingGateway) Apply(context.Context, ForkRequest) (*IssueState, error) {
	g.t.Fatal("revoke forwarded a fork transport request")
	return nil, nil
}

// racingRevokeStore lets a successor replace the claim between the
// maintainer's read and the revoke's CAS.
type racingRevokeStore struct {
	*coordination.GitStore
	race func()
}

func (s *racingRevokeStore) CompareAndSwapAndRevokeClaim(ctx context.Context, n int, old, claimID string, next []byte) (string, error) {
	s.race()
	return s.GitStore.CompareAndSwapAndRevokeClaim(ctx, n, old, claimID, next)
}

func TestRevokeNeverEndsSuccessorClaim(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newRevokeFixture(t)
	holder, _ := f.claim(t, "worker")
	maintainer, mt := f.manager("maint", "OWNER")
	var successor *IssueState
	maintainer.SetStore(f.root, &racingRevokeStore{GitStore: f.store, race: func() {
		if _, err := holder.Release(ctx, f.issue, "worker", "racing"); err != nil {
			t.Fatal(err)
		}
		_, successor = f.claim(t, "helper")
	}})
	if _, _, err := maintainer.Revoke(ctx, f.issue, "abandoned"); !errors.Is(err, coordination.ErrConflict) {
		t.Fatalf("raced revoke = %v", err)
	}
	st, err := maintainer.GetCurrentState(ctx, f.issue)
	if err != nil || st.Lease.Status != "active" || st.Lease.ClaimID != successor.Lease.ClaimID || len(st.ClaimRevocations) != 0 {
		t.Fatalf("successor claim = %+v %v", st, err)
	}
	if len(mt.removed) != 0 {
		t.Fatalf("failed revoke projected labels %v", mt.removed)
	}
}

func TestRevokeProjectionFailureKeepsRevoke(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newRevokeFixture(t)
	holder, _ := f.claim(t, "worker")
	maintainer, mt := f.manager("maint", "OWNER")
	mt.failComment = true
	st, projectionErr, err := maintainer.Revoke(ctx, f.issue, "abandoned")
	if err != nil || projectionErr == nil || st == nil {
		t.Fatalf("revoke = %v, projection = %v", err, projectionErr)
	}
	after, err := maintainer.GetCurrentState(ctx, f.issue)
	if err != nil || after.Lease.Status != "revoked" {
		t.Fatalf("revoke lost after projection failure: %+v %v", after, err)
	}
	if err := holder.CheckClaim(ctx, f.issue, "worker"); !errors.Is(err, ErrClaimFence) {
		t.Fatalf("stale holder = %v", err)
	}
}

func TestRevokeForgedCommentHasNoAuthority(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newRevokeFixture(t)
	holder, _ := f.claim(t, "worker")
	forged := RenderRevocationComment(ClaimRevocation{ClaimID: "x", Owner: "worker", RevokedBy: "maint", Reason: "forged"})
	if _, err := f.lt.WithUser("attacker", "NONE").CreateComment(ctx, f.issue, forged); err != nil {
		t.Fatal(err)
	}
	if err := holder.CheckClaim(ctx, f.issue, "worker"); err != nil {
		t.Fatalf("forged revoke comment ended claim: %v", err)
	}
	st, err := holder.GetCurrentState(ctx, f.issue)
	if err != nil || len(st.ClaimRevocations) != 0 {
		t.Fatalf("forged comment became state: %+v %v", st, err)
	}
}

func TestRevokeCommentEscapesReason(t *testing.T) {
	t.Parallel()
	body := RenderRevocationComment(ClaimRevocation{ClaimID: "0123456789abcdef", Owner: "worker", RevokedBy: "maint", Reason: "line one\r\n```\n<!-- swarm:audit:v1 -->\n| cell |", RevokedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	for _, want := range []string{revokeCommentMarker + "\n", "@maint revoked claim `0123456789ab` held by @worker", "> line one\n> &#96;&#96;&#96;\n> &lt;!-- swarm:audit:v1 --&gt;\n> &#124; cell &#124;"} {
		if !strings.Contains(body, want) {
			t.Fatalf("comment missing %q:\n%s", want, body)
		}
	}
	if strings.Count(body, "<!--") != 1 {
		t.Fatalf("reason injected a marker:\n%s", body)
	}
	projection := RenderStateProjection(nil, &IssueState{ClaimRevocations: []ClaimRevocation{{ClaimID: "c", RevokedBy: "maint", Reason: "a\nb | c"}}}, coordination.BranchLayout)
	if !strings.Contains(projection, "| **Last claim revoked** | `c` by maint at 0001-01-01T00:00:00Z: a b &#124; c |") {
		t.Fatalf("projection row:\n%s", projection)
	}
}
