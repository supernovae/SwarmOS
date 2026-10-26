package coordination

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type retentionFixture struct {
	root, remote, clone string
	store               *GitStore
	t                   *testing.T
}

func newRetentionFixture(t *testing.T) *retentionFixture {
	t.Helper()
	f := &retentionFixture{root: t.TempDir(), t: t}
	f.remote = filepath.Join(f.root, "remote.git")
	f.clone = filepath.Join(f.root, "clone")
	f.git(f.root, "init", "--bare", "--initial-branch=main", f.remote)
	f.git(f.root, "clone", f.remote, f.clone)
	f.git(f.clone, "config", "user.name", "Retention Test")
	f.git(f.clone, "config", "user.email", "test@example.invalid")
	f.git(f.clone, "commit", "--allow-empty", "-m", "base")
	f.git(f.clone, "push", "origin", "main")
	f.store = NewGitStore(f.clone)
	return f
}
func (f *retentionFixture) git(dir string, args ...string) string {
	f.t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
func TestCheckpointRetentionSurvivesRewriteDeletionAndGC(t *testing.T) {
	for _, mode := range []string{"issue", "PR", "fork"} {
		t.Run(mode, func(t *testing.T) {
			f := newRetentionFixture(t)
			ctx := context.Background()
			branch := "swarm/issue-1-retention"
			base := f.git(f.clone, "rev-parse", "HEAD")
			f.git(f.clone, "checkout", "-b", branch)
			f.git(f.clone, "commit", "--allow-empty", "-m", "A")
			a := f.git(f.clone, "rev-parse", "HEAD")
			rev, err := f.store.CompareAndSwapAndPublish(ctx, 1, "", []byte(fmt.Sprintf(`{"head_sha":%q}`, a)), branch, a)
			if err != nil {
				t.Fatal(err)
			}
			heads := []string{a}
			trees := []string{f.git(f.clone, "rev-parse", a+"^{tree}")}
			for _, name := range []string{"B", "C"} {
				predecessor := heads[len(heads)-1]
				f.git(f.clone, "reset", "--hard", base)
				f.git(f.clone, "commit", "--allow-empty", "-m", name)
				head := f.git(f.clone, "rev-parse", "HEAD")
				raw := []byte(fmt.Sprintf(`{"head_sha":%q}`, head))
				switch mode {
				case "issue":
					rev, err = f.store.CompareAndSwapAndReconcileCheckpoint(ctx, 1, rev, raw, branch, head, predecessor)
				case "PR":
					rev, err = f.store.CompareAndSwapAndPublishPR(ctx, 1, rev, raw, branch, predecessor, head)
				case "fork":
					// Verified coordinator has imported the object before indexed recording.
					request := int64(len(heads) + 100)
					forkStore := NewForkIndexedStore(f.clone, "alice", 1, request)
					rev, err = forkStore.CompareAndSwap(ctx, 1, rev, raw)
				}
				if err != nil {
					t.Fatal(err)
				}
				heads = append(heads, head)
				trees = append(trees, f.git(f.clone, "rev-parse", head+"^{tree}"))
			}
			f.git(f.clone, "push", "origin", ":refs/heads/"+branch)
			f.git(f.remote, "reflog", "expire", "--expire=now", "--all")
			f.git(f.remote, "gc", "--prune=now")
			fresh := filepath.Join(f.root, "fresh")
			f.git(f.root, "clone", "--single-branch", f.remote, fresh)
			for i, head := range heads {
				ref := BranchLayout.CheckpointRef(1, head)
				f.git(fresh, "fetch", "origin", ref)
				if got := f.git(fresh, "rev-parse", "FETCH_HEAD"); got != head {
					t.Fatalf("retained %s, want %s", got, head)
				}
				if got := f.git(fresh, "rev-parse", head+"^{tree}"); got != trees[i] {
					t.Fatal("retained tree changed")
				}
			}
			// Ledger ancestry remains independent of task code.
			if out, err := exec.Command("git", "-C", f.clone, "merge-base", "--is-ancestor", a, rev).CombinedOutput(); err == nil {
				t.Fatalf("code became ledger ancestor: %s", out)
			}
		})
	}
}

func TestRetentionAtomicRejectionAndLegacyHistory(t *testing.T) {
	for _, scenario := range []string{"identical", "conflict", "atomic unsupported", "unavailable predecessor", "missing legacy", "state race"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRetentionFixture(t)
			ctx := context.Background()
			branch := "swarm/issue-1-test"
			a := f.git(f.clone, "rev-parse", "HEAD")
			raw := []byte(fmt.Sprintf(`{"head_sha":%q}`, a))
			rev, err := f.store.CompareAndSwapAndPublish(ctx, 1, "", raw, branch, a)
			if err != nil {
				t.Fatal(err)
			}
			f.git(f.clone, "commit", "--allow-empty", "-m", "B")
			b := f.git(f.clone, "rev-parse", "HEAD")
			switch scenario {
			case "identical":
				f.git(f.clone, "push", "origin", b+":"+BranchLayout.CheckpointRef(1, b))
			case "conflict":
				f.git(f.clone, "push", "origin", a+":"+BranchLayout.CheckpointRef(1, b))
			case "atomic unsupported":
				f.git(f.remote, "config", "receive.advertiseAtomic", "false")
			case "unavailable predecessor":
				// Remote ref existence does not make a missing expected object available.
				if _, err := f.store.retentionArgs(ctx, 1, strings.Repeat("e", 40)); err == nil {
					t.Fatal("missing required object accepted")
				}
				return
			case "missing legacy":
				rev, err = f.store.CompareAndSwap(ctx, 1, rev, []byte(fmt.Sprintf(`{"head_sha":%q,"last_rebase":{"old_head":%q}}`, a, strings.Repeat("e", 40))))
				if err != nil {
					t.Fatal(err)
				}
			case "state race":
				if _, err = f.store.CompareAndSwap(ctx, 1, rev, raw); err != nil {
					t.Fatal(err)
				}
			}
			_, err = f.store.CompareAndSwapAndReconcileCheckpoint(ctx, 1, rev, []byte(fmt.Sprintf(`{"head_sha":%q}`, b)), branch, b, a)
			success := scenario == "identical" || scenario == "missing legacy"
			if (err == nil) != success {
				t.Fatalf("publication error=%v success=%t", err, success)
			}
			want := a
			if success {
				want = b
			}
			if got := f.git(f.remote, "rev-parse", "refs/heads/"+branch); got != want {
				t.Fatal("rejection changed branch")
			}
			if !success && scenario != "conflict" {
				if _, err := exec.Command("git", "-C", f.remote, "show-ref", "--verify", BranchLayout.CheckpointRef(1, b)).Output(); err == nil {
					t.Fatal("rejected push created retention ref")
				}
			}
		})
	}
}

func TestCheckpointRetentionWithStateWrite(t *testing.T) {
	ctx := context.Background()
	t.Run("git", func(t *testing.T) {
		f := newRetentionFixture(t)
		f.git(f.clone, "commit", "--allow-empty", "-m", "abandoned")
		head := f.git(f.clone, "rev-parse", "HEAD")
		stale, err := f.store.CompareAndSwap(ctx, 1, "", []byte(`{"v":0}`))
		if err != nil {
			t.Fatal(err)
		}
		rev, err := f.store.CompareAndSwap(ctx, 1, stale, []byte(`{"v":1}`))
		if err != nil {
			t.Fatal(err)
		}
		// A stale state lease rejects the whole atomic push, retention included.
		if _, err := f.store.CompareAndSwapRetaining(ctx, 1, stale, []byte(`{"v":2}`), head); !errors.Is(err, ErrConflict) {
			t.Fatalf("stale write: %v", err)
		}
		if out := f.git(f.remote, "for-each-ref", CheckpointRef(1, head)); out != "" {
			t.Fatalf("failed write retained %s", out)
		}
		if _, err := f.store.CompareAndSwapRetaining(ctx, 1, rev, []byte(`{"v":2}`), strings.Repeat("1", 40)); err == nil {
			t.Fatal("retained an unavailable commit")
		}
		if _, err := f.store.CompareAndSwapRetaining(ctx, 1, rev, []byte(`{"v":2}`), head); err != nil {
			t.Fatal(err)
		}
		if got := f.git(f.remote, "rev-parse", CheckpointRef(1, head)); got != head {
			t.Fatalf("retention ref is %s", got)
		}
		if body, _, err := f.store.Load(ctx, 1); err != nil || string(body) != `{"v":2}` {
			t.Fatalf("state %s %v", body, err)
		}
	})
	t.Run("file", func(t *testing.T) {
		f := newRetentionFixture(t)
		store := NewFileStore(f.clone)
		f.git(f.clone, "commit", "--allow-empty", "-m", "abandoned")
		head := f.git(f.clone, "rev-parse", "HEAD")
		if _, err := store.CompareAndSwapRetaining(ctx, 1, "", []byte(`{"v":1}`), strings.Repeat("1", 40)); err == nil {
			t.Fatal("retained an unavailable commit")
		}
		if _, _, err := store.Load(ctx, 1); !errors.Is(err, ErrNotFound) {
			t.Fatal("failed retention wrote state")
		}
		rev, err := store.CompareAndSwapRetaining(ctx, 1, "", []byte(`{"v":1}`), head)
		if err != nil {
			t.Fatal(err)
		}
		// An existing correct ref is accepted; a wrong one is never rewritten.
		if _, err := store.CompareAndSwapRetaining(ctx, 1, rev, []byte(`{"v":2}`), head); err != nil {
			t.Fatal(err)
		}
		f.git(f.clone, "commit", "--allow-empty", "-m", "other")
		other := f.git(f.clone, "rev-parse", "HEAD")
		f.git(f.clone, "update-ref", CheckpointRef(1, other), head)
		if _, err := store.CompareAndSwapRetaining(ctx, 1, rev, []byte(`{"v":3}`), other); !errors.Is(err, ErrConflict) {
			t.Fatalf("corrupt ref: %v", err)
		}
		if got := f.git(f.clone, "rev-parse", CheckpointRef(1, other)); got != head {
			t.Fatal("rewrote a retention ref")
		}
	})
}
