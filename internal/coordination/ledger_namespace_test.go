package coordination

import (
	"context"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type ledgerFixture struct {
	t             *testing.T
	root, remote  string
	clone, second string
	base          string
	layout        RefLayout
}

func newLedgerFixture(t *testing.T, layout RefLayout) *ledgerFixture {
	t.Helper()
	f := &ledgerFixture{t: t, root: t.TempDir(), layout: layout}
	f.remote = filepath.Join(f.root, "remote.git")
	f.clone = filepath.Join(f.root, "clone")
	f.second = filepath.Join(f.root, "second")
	f.git(f.root, "init", "--bare", "--initial-branch=main", f.remote)
	f.git(f.root, "clone", f.remote, f.clone)
	f.git(f.clone, "config", "user.name", "Ledger Test")
	f.git(f.clone, "config", "user.email", "test@example.invalid")
	f.git(f.clone, "commit", "--allow-empty", "-m", "baseline")
	f.git(f.clone, "push", "origin", "main")
	f.git(f.root, "clone", f.remote, f.second)
	f.base = f.git(f.clone, "rev-parse", "HEAD")
	return f
}

func (f *ledgerFixture) git(dir string, args ...string) string {
	f.t.Helper()
	out, err := gitfixture.Command(f.t, "", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *ledgerFixture) store(dir string) *GitStore { return NewGitStoreWithLayout(dir, "", f.layout) }

// commit makes a new code commit on the clone and returns its SHA.
func (f *ledgerFixture) commit(msg string) string {
	f.git(f.clone, "commit", "--allow-empty", "-m", msg)
	return f.git(f.clone, "rev-parse", "HEAD")
}

// snapshot lists every remote ref and target.
func (f *ledgerFixture) snapshot() string {
	return f.git(f.remote, "for-each-ref", "--format=%(refname) %(objectname)")
}

// assertNamespace checks every ledger ref lives in the fixture's namespace and
// hidden mode adds no branch other than main and task branches.
func (f *ledgerFixture) assertNamespace() {
	f.t.Helper()
	other := f.layout.Counterpart()
	for _, line := range strings.Split(f.snapshot(), "\n") {
		ref, _, _ := strings.Cut(line, " ")
		for _, prefix := range other.prefixes() {
			if strings.HasPrefix(ref, prefix) {
				f.t.Errorf("%s mode wrote %s", f.layout.Name(), ref)
			}
		}
		if f.layout == HiddenLayout && strings.HasPrefix(ref, "refs/heads/") && ref != "refs/heads/main" && !strings.HasPrefix(ref, "refs/heads/swarm/issue-") {
			f.t.Errorf("hidden mode created branch %s", ref)
		}
	}
}

func bothLayouts(t *testing.T, run func(*testing.T, RefLayout)) {
	for _, layout := range []RefLayout{BranchLayout, HiddenLayout} {
		t.Run(layout.Name(), func(t *testing.T) { run(t, layout) })
	}
}

func TestLedgerNamespacesCASCheckpointPRAndCleanup(t *testing.T) {
	bothLayouts(t, func(t *testing.T, layout RefLayout) {
		ctx := context.Background()
		f := newLedgerFixture(t, layout)
		stores := []*GitStore{f.store(f.clone), f.store(f.second)}
		initial, err := stores[0].CompareAndSwap(ctx, 1, "", []byte(`{"lease":{"issue_number":1}}`))
		if err != nil {
			t.Fatal(err)
		}
		if got := f.git(f.remote, "rev-parse", layout.StateRef(1)); got != initial {
			t.Fatalf("state ref %s = %s, want %s", layout.StateRef(1), got, initial)
		}
		for _, store := range stores {
			if _, _, err := store.Load(ctx, 1); err != nil {
				t.Fatal(err)
			}
		}
		// Two clients CAS from one revision: exactly one wins.
		var wg sync.WaitGroup
		results := make(chan error, 2)
		for i, store := range stores {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := store.CompareAndSwap(ctx, 1, initial, fmt.Appendf(nil, `{"lease":{"issue_number":1},"claim":%d}`, i))
				results <- err
			}()
		}
		wg.Wait()
		close(results)
		var wins, conflicts int
		for err := range results {
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrConflict):
				conflicts++
			default:
				t.Fatal(err)
			}
		}
		if wins != 1 || conflicts != 1 {
			t.Fatalf("CAS admitted %d winners and %d conflicts", wins, conflicts)
		}
		_, rev, err := stores[0].Load(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}

		// Atomic checkpoint: state, task branch, and retention move together.
		head := f.commit("checkpoint")
		branch := "swarm/issue-1-work"
		rev, err = stores[0].CompareAndSwapAndPublish(ctx, 1, rev, []byte(`{"lease":{"issue_number":1},"head_sha":"`+head+`"}`), branch, head)
		if err != nil {
			t.Fatal(err)
		}
		if f.git(f.remote, "rev-parse", layout.CheckpointRef(1, head)) != head || f.git(f.remote, "rev-parse", "refs/heads/"+branch) != head {
			t.Fatal("checkpoint publication missed retention or task branch")
		}
		// A stale checkpoint publication changes nothing.
		before := f.snapshot()
		if _, err := stores[0].CompareAndSwapAndPublish(ctx, 1, initial, []byte(`{"stale":true}`), branch, f.commit("stale")); err == nil {
			t.Fatal("stale state lease published a checkpoint")
		}
		if f.snapshot() != before {
			t.Fatal("rejected checkpoint changed remote refs")
		}

		// Atomic PR publication from the captured head.
		next := f.commit("pr revision")
		rev, err = stores[0].CompareAndSwapAndPublishPR(ctx, 1, rev, []byte(`{"lease":{"issue_number":1},"head_sha":"`+next+`"}`), branch, head, next)
		if err != nil {
			t.Fatal(err)
		}
		if f.git(f.remote, "rev-parse", layout.CheckpointRef(1, next)) != next || f.git(f.remote, "rev-parse", "refs/heads/"+branch) != next {
			t.Fatal("PR publication missed retention or task branch")
		}
		before = f.snapshot()
		if _, err := stores[0].CompareAndSwapAndPublishPR(ctx, 1, rev, []byte(`{}`), branch, head, f.commit("stale pr")); err == nil {
			t.Fatal("stale PR head published")
		}
		if f.snapshot() != before {
			t.Fatal("rejected PR publication changed remote refs")
		}

		// Cleanup retires only the task branch; retention survives.
		if _, err := stores[0].CompareAndSwapAndDeleteBranch(ctx, 1, rev, []byte(`{"lease":{"issue_number":1},"cleaned":true}`), branch, next); err != nil {
			t.Fatal(err)
		}
		if out := f.git(f.remote, "for-each-ref", "refs/heads/"+branch); out != "" {
			t.Fatalf("cleanup left task branch: %s", out)
		}
		for _, sha := range []string{head, next} {
			if f.git(f.remote, "rev-parse", layout.CheckpointRef(1, sha)) != sha {
				t.Fatalf("cleanup dropped retention of %s", sha)
			}
		}
		f.assertNamespace()
	})
}

func TestLedgerNamespacesForkContributorIndex(t *testing.T) {
	bothLayouts(t, func(t *testing.T, layout RefLayout) {
		ctx := context.Background()
		f := newLedgerFixture(t, layout)
		base := f.store(f.clone)
		rev1, err := base.CompareAndSwap(ctx, 1, "", []byte(`{"lease":{"issue_number":1,"status":"draft"}}`))
		if err != nil {
			t.Fatal(err)
		}
		rev2, err := base.CompareAndSwap(ctx, 2, "", []byte(`{"lease":{"issue_number":2,"status":"draft"}}`))
		if err != nil {
			t.Fatal(err)
		}
		now := time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC)
		exp := now.Add(time.Hour).Format(time.RFC3339)
		forkStore := func(id int64) *ForkIndexedStore {
			s := NewForkIndexedStoreWithLayout(f.clone, "", layout, "Alice", 1, id)
			s.SetNowFunc(func() time.Time { return now })
			return s
		}
		active := func(n int) []byte {
			return fmt.Appendf(nil, `{"lease":{"issue_number":%d,"claim_id":"c%d","owner":"alice","status":"active","expires_at":%q}}`, n, n, exp)
		}
		after, err := forkStore(101).CompareAndSwap(ctx, 1, rev1, active(1))
		if err != nil {
			t.Fatal(err)
		}
		if f.git(f.remote, "rev-parse", layout.ContributorRef("alice")) == "" {
			t.Fatal("contributor index missing")
		}
		if _, err := forkStore(101).CompareAndSwap(ctx, 1, after, active(1)); !errors.Is(err, ErrReplay) {
			t.Fatalf("replay: %v", err)
		}
		before := f.snapshot()
		if _, err := forkStore(102).CompareAndSwap(ctx, 2, rev2, active(2)); !errors.Is(err, ErrForkClaimLimit) {
			t.Fatalf("cap: %v", err)
		}
		if f.snapshot() != before {
			t.Fatal("rejected fork claim changed remote refs")
		}
		f.assertNamespace()
	})
}

// TestLedgerNamespaceAmbiguityFailsClosed seeds the counterpart of a ledger ref
// and checks every read and write refuses with both names and changes nothing.
func TestLedgerNamespaceAmbiguityFailsClosed(t *testing.T) {
	bothLayouts(t, func(t *testing.T, layout RefLayout) {
		for _, scenario := range []string{"both-equal", "both-different", "opposite-only"} {
			t.Run(scenario, func(t *testing.T) {
				ctx := context.Background()
				f := newLedgerFixture(t, layout)
				other := layout.Counterpart()
				store := f.store(f.clone)
				// Seed the issue in the other namespace, then optionally in ours.
				seeded, err := NewGitStoreWithLayout(f.clone, "", other).CompareAndSwap(ctx, 1, "", []byte(`{"lease":{"issue_number":1}}`))
				if err != nil {
					t.Fatal(err)
				}
				rev := seeded
				switch scenario {
				case "both-equal":
					f.git(f.clone, "push", "origin", seeded+":"+layout.StateRef(1))
				case "both-different":
					f.git(f.clone, "push", "origin", f.base+":"+layout.StateRef(1))
					rev = f.base
				}
				branch := "swarm/issue-1-work"
				f.git(f.clone, "push", "origin", f.base+":refs/heads/"+branch)
				head := f.commit("candidate")
				before := f.snapshot()
				names := []string{layout.StateRef(1), other.StateRef(1)}
				checks := map[string]func() error{
					"load": func() error { _, _, err := store.Load(ctx, 1); return err },
					"cas":  func() error { _, err := store.CompareAndSwap(ctx, 1, rev, []byte(`{}`)); return err },
					"create": func() error {
						_, err := store.CompareAndSwap(ctx, 1, "", []byte(`{}`))
						return err
					},
					"checkpoint": func() error {
						_, err := store.CompareAndSwapAndPublish(ctx, 1, rev, []byte(`{}`), branch, head)
						return err
					},
					"pr": func() error {
						_, err := store.CompareAndSwapAndPublishPR(ctx, 1, rev, []byte(`{}`), branch, f.base, head)
						return err
					},
					"cleanup": func() error {
						_, err := store.CompareAndSwapAndDeleteBranch(ctx, 1, rev, []byte(`{}`), branch, f.base)
						return err
					},
					"fork": func() error {
						s := NewForkIndexedStoreWithLayout(f.clone, "", layout, "alice", 1, 5)
						_, err := s.CompareAndSwap(ctx, 1, rev, []byte(`{"lease":{"issue_number":1,"status":"draft"}}`))
						return err
					},
				}
				for _, name := range slices.Sorted(func(yield func(string) bool) {
					for k := range checks {
						if !yield(k) {
							return
						}
					}
				}) {
					err := checks[name]()
					if !errors.Is(err, ErrLedgerNamespace) {
						t.Errorf("%s: %v, want ErrLedgerNamespace", name, err)
						continue
					}
					for _, ref := range names {
						if !strings.Contains(err.Error(), ref) {
							t.Errorf("%s error does not name %s: %v", name, ref, err)
						}
					}
				}
				if f.snapshot() != before {
					t.Fatalf("ambiguous ledger changed remote refs:\n%s\nwas\n%s", f.snapshot(), before)
				}
			})
		}
	})
}

func TestLedgerNamespaceRetentionAndContributorDuplicates(t *testing.T) {
	bothLayouts(t, func(t *testing.T, layout RefLayout) {
		ctx := context.Background()
		f := newLedgerFixture(t, layout)
		other := layout.Counterpart()
		store := f.store(f.clone)
		rev, err := store.CompareAndSwap(ctx, 1, "", []byte(`{"lease":{"issue_number":1,"status":"draft"}}`))
		if err != nil {
			t.Fatal(err)
		}
		head := f.commit("candidate")
		// Retention for head already exists in the other namespace.
		f.git(f.clone, "push", "origin", head+":"+other.CheckpointRef(1, head))
		before := f.snapshot()
		_, err = store.CompareAndSwapAndPublish(ctx, 1, rev, []byte(`{}`), "swarm/issue-1-work", head)
		if !errors.Is(err, ErrLedgerNamespace) || !strings.Contains(err.Error(), other.CheckpointRef(1, head)) || !strings.Contains(err.Error(), layout.CheckpointRef(1, head)) {
			t.Fatalf("retention duplicate: %v", err)
		}
		if f.snapshot() != before {
			t.Fatal("retention duplicate changed remote refs")
		}
		// A contributor index in the other namespace blocks fork claims.
		f.git(f.clone, "push", "origin", f.base+":"+other.ContributorRef("alice"))
		before = f.snapshot()
		s := NewForkIndexedStoreWithLayout(f.clone, "", layout, "alice", 1, 7)
		now := time.Now()
		s.SetNowFunc(func() time.Time { return now })
		claim := fmt.Appendf(nil, `{"lease":{"issue_number":1,"claim_id":"c1","owner":"alice","status":"active","expires_at":%q}}`, now.Add(time.Hour).Format(time.RFC3339))
		_, err = s.CompareAndSwap(ctx, 1, rev, claim)
		if !errors.Is(err, ErrLedgerNamespace) || !strings.Contains(err.Error(), other.ContributorRef("alice")) || !strings.Contains(err.Error(), layout.ContributorRef("alice")) {
			t.Fatalf("contributor duplicate: %v", err)
		}
		if f.snapshot() != before {
			t.Fatal("contributor duplicate changed remote refs")
		}
	})
}
