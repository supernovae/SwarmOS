package coordination

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

func TestFileStoreRejectsSymlinkStateAndLock(t *testing.T) {
	for _, name := range []string{"issue-1.json", "issue-1.json.lock"} {
		t.Run(name, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			dir := filepath.Join(root, ".swarm", "coordination")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			canary := filepath.Join(outside, "target")
			if err := os.WriteFile(canary, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(canary, filepath.Join(dir, name)); err != nil {
				t.Skip(err)
			}
			if _, err := NewFileStore(root).CompareAndSwap(context.Background(), 1, "", []byte("new")); err == nil {
				t.Fatal("symlink accepted")
			}
			b, err := os.ReadFile(canary)
			if err != nil || string(b) != "original" {
				t.Fatalf("canary modified: %q %v", b, err)
			}
		})
	}
}

func TestFileStoreDoesNotUsePredictableTemporaryFile(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	dir := filepath.Join(root, ".swarm", "coordination")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(outside, "target")
	if err := os.WriteFile(canary, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(canary, filepath.Join(dir, "issue-1.json.tmp")); err != nil {
		t.Skip(err)
	}
	s := NewFileStore(root)
	if _, err := s.CompareAndSwap(context.Background(), 1, "", []byte("new")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(canary)
	if err != nil || string(b) != "original" {
		t.Fatalf("canary modified: %q %v", b, err)
	}
}

func TestFileStoreConcurrentAtomicReadAndCAS(t *testing.T) {
	s := NewFileStore(t.TempDir())
	ctx := context.Background()
	if _, err := s.CompareAndSwap(ctx, 1, "", []byte("0")); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	failures := make(chan error, 8)
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 20 {
				for {
					data, rev, err := s.Load(ctx, 1)
					if err != nil {
						failures <- err
						return
					}
					n, err := strconv.Atoi(string(data))
					if err != nil {
						failures <- err
						return
					}
					_, err = s.CompareAndSwap(ctx, 1, rev, []byte(strconv.Itoa(n+1)))
					if errors.Is(err, ErrConflict) {
						continue
					}
					if err != nil {
						failures <- err
						return
					}
					break
				}
			}
		}()
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	data, _, err := s.Load(ctx, 1)
	if err != nil || string(data) != "160" {
		t.Fatalf("unexpected final state %q: %v", data, err)
	}
}
