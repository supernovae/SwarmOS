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
