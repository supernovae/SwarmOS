package coordination

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestInventoryPreviewBoundedAndNonCreating(t *testing.T) {
	root := t.TempDir()
	store := NewFileStore(root)
	reserve := func(int64) error { return nil }
	if _, err := store.LoadInventoryPreview(context.Background(), 1, 1024, reserve); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".swarm")); !os.IsNotExist(err) {
		t.Fatal("created storage")
	}
	dir := filepath.Join(root, ".swarm", "coordination")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "issue-1.json")
	if err := os.WriteFile(path, []byte(`{"lease":{"issue_number":1}}`), 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	bytes, err := store.LoadInventoryPreview(context.Background(), 1, 1024, reserve)
	if err != nil || len(bytes) == 0 {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("changed state")
	}
	names, _ := os.ReadDir(dir)
	if len(names) != 1 {
		t.Fatal("created lock")
	}
	sentinel := errors.New("reserve refused")
	if _, err = store.LoadInventoryPreview(context.Background(), 1, 1024, func(int64) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if _, err = store.LoadInventoryPreview(context.Background(), 1, 1, reserve); err == nil {
		t.Fatal("accepted oversized input")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = store.LoadInventoryPreview(ctx, 1, 1024, reserve); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestInventorySnapshotCoherenceAndNoLocks(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".swarm", "coordination")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "issue-1.json")
	if err := os.WriteFile(path, []byte(`{"lease":{"version":1,"issue_number":1}}`), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewFileStore(root)
	states, err := store.ReadInventorySnapshot(context.Background(), 10, func(int64) error { return nil })
	if err != nil || len(states) != 1 {
		t.Fatalf("%v %v", states, err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatal("created lock", files, err)
	}
	mutated := false
	_, err = store.ReadInventorySnapshot(context.Background(), 10, func(int64) error {
		if !mutated {
			mutated = true
			tmp := filepath.Join(dir, "replacement")
			if e := os.WriteFile(tmp, []byte("replacement"), 0600); e != nil {
				return e
			}
			return os.Rename(tmp, path)
		}
		return nil
	})
	if err == nil {
		t.Fatal("mixed snapshot accepted")
	}
	if _, err := store.ReadInventorySnapshot(context.Background(), 0, func(int64) error { return nil }); err == nil {
		t.Fatal("ignored entry bound")
	}
}
