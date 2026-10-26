package coordination

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// A store on a named upstream reads, fences and publishes there while a decoy
// origin is never touched.
func TestGitStoreWithNamedUpstreamRemote(t *testing.T) {
	f := newRetentionFixture(t)
	ctx := context.Background()
	decoy := filepath.Join(f.root, "decoy.git")
	f.git(f.root, "clone", "--bare", f.remote, decoy)
	f.git(f.clone, "remote", "rename", "origin", "upstream")
	f.git(f.clone, "remote", "add", "origin", decoy)
	decoyRefs := f.git(decoy, "for-each-ref")
	store := NewGitStoreWithRemote(f.clone, "upstream")
	if store.Remote() != "upstream" || NewGitStore(f.clone).Remote() != "origin" {
		t.Fatal("unexpected remotes")
	}
	rev, err := store.CompareAndSwap(ctx, 9, "", []byte(`{"n":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompareAndSwap(ctx, 9, "", []byte(`{"n":2}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale create = %v, want conflict", err)
	}
	branch := "swarm/issue-9-task"
	f.git(f.clone, "checkout", "-b", branch)
	f.git(f.clone, "commit", "--allow-empty", "-m", "work")
	head := f.git(f.clone, "rev-parse", "HEAD")
	if _, err := store.CompareAndSwapAndPublish(ctx, 9, rev, []byte(`{"n":3}`), branch, head); err != nil {
		t.Fatal(err)
	}
	if got := f.git(f.remote, "rev-parse", "refs/heads/"+branch); got != head {
		t.Fatalf("upstream branch = %s", got)
	}
	if got := f.git(f.remote, "rev-parse", BranchLayout.CheckpointRef(9, head)); got != head {
		t.Fatalf("upstream retention = %s", got)
	}
	if body, _, err := store.Load(ctx, 9); err != nil || string(body) != `{"n":3}` {
		t.Fatalf("load = %s %v", body, err)
	}
	if got := f.git(decoy, "for-each-ref"); got != decoyRefs {
		t.Fatalf("decoy origin changed:\n%s", got)
	}
	if _, _, err := NewGitStore(f.clone).Load(ctx, 9); !errors.Is(err, ErrNotFound) {
		t.Fatalf("origin store found upstream state: %v", err)
	}
}
