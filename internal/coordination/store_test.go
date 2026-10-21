package coordination

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGitCASAcrossClonesAndAtomicCheckpointFence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	remote := filepath.Join(root, "remote.git")
	git(root, "init", "--bare", "--initial-branch=main", remote)
	first := filepath.Join(root, "first")
	git(root, "clone", remote, first)
	git(first, "config", "user.name", "CAS Test")
	git(first, "config", "user.email", "test@example.invalid")
	git(first, "commit", "--allow-empty", "-m", "baseline")
	git(first, "push", "origin", "main")
	second := filepath.Join(root, "second")
	git(root, "clone", remote, second)
	stores := []*GitStore{NewGitStore(first), NewGitStore(second)}
	initial, err := stores[0].CompareAndSwap(ctx, 1, "", []byte(`{"status":"ready"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, store := range stores {
		if _, _, err := store.Load(ctx, 1); err != nil {
			t.Fatal(err)
		}
	}
	var workers sync.WaitGroup
	results := make(chan error, 2)
	for index, store := range stores {
		workers.Add(1)
		go func(index int, store *GitStore) {
			defer workers.Done()
			_, err := store.CompareAndSwap(ctx, 1, initial, []byte(fmt.Sprintf(`{"claim":"worker-%d"}`, index)))
			results <- err
		}(index, store)
	}
	workers.Wait()
	close(results)
	winners, conflicts := 0, 0
	for err := range results {
		if err == nil {
			winners++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("CAS admitted %d winners and %d conflicts", winners, conflicts)
	}
	_, current, err := stores[0].Load(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	head := git(first, "rev-parse", "HEAD")
	branch := "swarm/issue-1-checkpoint"
	published, err := stores[0].CompareAndSwapAndPublish(ctx, 1, current, []byte(`{"status":"checkpointed"}`), branch, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, "candidate.txt"), []byte("stale change"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(first, "add", ".")
	git(first, "commit", "-m", "stale candidate")
	staleHead := git(first, "rev-parse", "HEAD")
	if _, err := stores[0].CompareAndSwap(ctx, 1, published, []byte(`{"claim":"successor"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := stores[0].CompareAndSwapAndPublish(ctx, 1, published, []byte(`{"claim":"stale"}`), branch, staleHead); err == nil {
		t.Fatal("stale worker published checkpoint")
	}
	if remoteHead := git(remote, "rev-parse", "refs/heads/"+branch); remoteHead != head {
		t.Fatalf("rejected atomic push changed checkpoint to %s", remoteHead)
	}
}

func TestConcurrentIssueReadsStayPinnedAndTransportErrorsSurface(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	clone := filepath.Join(root, "clone")
	for _, args := range [][]string{{"init", "--bare", remote}, {"clone", remote, clone}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	store := NewGitStore(clone)
	ctx := context.Background()
	for issue := 1; issue <= 4; issue++ {
		if _, err := store.CompareAndSwap(ctx, issue, "", []byte(fmt.Sprint(issue))); err != nil {
			t.Fatal(err)
		}
	}
	var workers sync.WaitGroup
	for issue := 1; issue <= 4; issue++ {
		workers.Add(1)
		go func(issue int) {
			defer workers.Done()
			body, _, err := store.Load(ctx, issue)
			if err != nil || string(body) != fmt.Sprint(issue) {
				t.Errorf("issue %d read %q: %v", issue, body, err)
			}
		}(issue)
	}
	workers.Wait()
	if out, err := exec.Command("git", "-C", clone, "remote", "set-url", "origin", filepath.Join(root, "missing")).CombinedOutput(); err != nil {
		t.Fatalf("set-url: %v: %s", err, out)
	}
	if _, _, err := store.Load(ctx, 1); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("transport error hidden as absent state: %v", err)
	}
}

func TestForkIndexedStoreAtomicCapAndReplay(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	remote := filepath.Join(root, "origin.git")
	git(root, "init", "--bare", "--initial-branch=main", remote)
	clone := filepath.Join(root, "clone")
	git(root, "clone", remote, clone)
	git(clone, "config", "user.name", "Fork Index Test")
	git(clone, "config", "user.email", "test@example.invalid")
	git(clone, "commit", "--allow-empty", "-m", "initial")
	git(clone, "push", "origin", "main")

	baseStore := NewGitStore(clone)
	var anyStore Store = NewForkIndexedStore(clone, "alice", 1, 100)
	if _, ok := anyStore.(PublishingStore); ok {
		t.Fatal("ForkIndexedStore must never implement PublishingStore")
	}

	rev1, err := baseStore.CompareAndSwap(ctx, 1, "", []byte(`{"lease":{"issue_number":1,"status":"draft"}}`))
	if err != nil {
		t.Fatal(err)
	}
	rev2, err := baseStore.CompareAndSwap(ctx, 2, "", []byte(`{"lease":{"issue_number":2,"status":"draft"}}`))
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC)
	exp := now.Add(time.Hour).Format(time.RFC3339)

	// Acquire issue #1 as alice with request ID 101
	s1 := NewForkIndexedStore(clone, "alice", 1, 101)
	s1.SetNowFunc(func() time.Time { return now })
	active1 := fmt.Sprintf(`{"lease":{"issue_number":1,"claim_id":"c1","owner":"alice","status":"active","expires_at":%q}}`, exp)
	rev1After, err := s1.CompareAndSwap(ctx, 1, rev1, []byte(active1))
	if err != nil {
		t.Fatalf("acquire issue 1 failed: %v", err)
	}

	// Replaying request ID 101 (or older ID 100) must fail with ErrReplay
	sReplay := NewForkIndexedStore(clone, "alice", 1, 101)
	sReplay.SetNowFunc(func() time.Time { return now })
	if _, err := sReplay.CompareAndSwap(ctx, 1, rev1After, []byte(active1)); !errors.Is(err, ErrReplay) {
		t.Fatalf("expected ErrReplay for duplicate request ID, got %v", err)
	}

	// Acquiring issue #2 while issue #1 is active exceeds maxActive=1
	s2 := NewForkIndexedStore(clone, "alice", 1, 102)
	s2.SetNowFunc(func() time.Time { return now })
	active2 := fmt.Sprintf(`{"lease":{"issue_number":2,"claim_id":"c2","owner":"alice","status":"active","expires_at":%q}}`, exp)
	if _, err := s2.CompareAndSwap(ctx, 2, rev2, []byte(active2)); !errors.Is(err, ErrForkClaimLimit) {
		t.Fatalf("expected ErrForkClaimLimit on second active claim, got %v", err)
	}

	// Advance clock past issue #1 expiry: revalidation prunes issue #1 and admits issue #2
	later := now.Add(2 * time.Hour)
	exp2 := later.Add(time.Hour).Format(time.RFC3339)
	s2Later := NewForkIndexedStore(clone, "alice", 1, 103)
	s2Later.SetNowFunc(func() time.Time { return later })
	active2Later := fmt.Sprintf(`{"lease":{"issue_number":2,"claim_id":"c2","owner":"alice","status":"active","expires_at":%q}}`, exp2)
	if _, err := s2Later.CompareAndSwap(ctx, 2, rev2, []byte(active2Later)); err != nil {
		t.Fatalf("acquire issue 2 after issue 1 expiry failed: %v", err)
	}
}
