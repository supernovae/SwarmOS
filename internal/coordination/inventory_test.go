package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInventoryPreviewBoundedAndNonCreating(t *testing.T) {
	root := t.TempDir()
	store := NewFileStore(root)
	reserve := func(int64) error { return nil }
	if _, err := store.LoadInventoryPreview(context.Background(), 1, 1024, reserve, testInventoryDecoder); !os.IsNotExist(err) {
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
	bytes, err := store.LoadInventoryPreview(context.Background(), 1, 1024, reserve, testInventoryDecoder)
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
	if _, err = store.LoadInventoryPreview(context.Background(), 1, 1024, func(int64) error { return sentinel }, testInventoryDecoder); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if _, err = store.LoadInventoryPreview(context.Background(), 1, 1, reserve, testInventoryDecoder); err == nil {
		t.Fatal("accepted oversized input")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = store.LoadInventoryPreview(ctx, 1, 1024, reserve, testInventoryDecoder); !errors.Is(err, context.Canceled) {
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
	states, err := store.ReadInventorySnapshot(context.Background(), 10, func(int64) error { return nil }, testInventoryDecoder)
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
	}, testInventoryDecoder)
	if err == nil {
		t.Fatal("mixed snapshot accepted")
	}
	if _, err := store.ReadInventorySnapshot(context.Background(), 0, func(int64) error { return nil }, testInventoryDecoder); err == nil {
		t.Fatal("ignored entry bound")
	}
}

func testInventoryDecoder(ctx context.Context, r io.Reader, reserve func(int64) error) ([]byte, error) {
	if err := reserve(8192); err != nil {
		return nil, err
	}
	var v map[string]any
	err := json.NewDecoder(r).Decode(&v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func TestInventoryStreamingLimitsAndErrors(t *testing.T) {
	reserve := func(int64) error { return nil }
	// An exact input ceiling probes EOF; a single additional byte is refused.
	for _, extra := range []string{"", " "} {
		r := &inventoryReader{ctx: context.Background(), r: strings.NewReader(`{}` + extra), remaining: 2}
		_, err := io.ReadAll(r)
		if (err == nil) != (extra == "") {
			t.Fatal(extra, err)
		}
	}
	store := NewGitStore(t.TempDir())
	for _, sha := range []string{"HEAD", strings.Repeat("g", 40), "refs/heads/main"} {
		if _, err := store.LoadInventoryObject(context.Background(), sha, reserve, testInventoryDecoder); err == nil {
			t.Fatal(sha)
		}
	}
	realPath := os.Getenv("PATH")
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+realPath)
	script := filepath.Join(dir, "git")
	sha := strings.Repeat("a", 40)
	for _, tc := range []struct {
		script string
		valid  bool
	}{
		{"printf 'diagnostic' >&2\nprintf '{\"lease\":{\"issue_number\":1}}'\n", true},
		{"printf 'underlying Git failure' >&2\nexit 42\n", false},
		{"i=0; while [ $i -lt 10000 ]; do printf 'hostile diagnostic' >&2; i=$((i+1)); done\nexit 42\n", false},
	} {
		if err := os.WriteFile(script, []byte("#!/bin/sh\n"+tc.script), 0700); err != nil {
			t.Fatal(err)
		}
		data, err := store.LoadInventoryObject(context.Background(), sha, reserve, testInventoryDecoder)
		if (err == nil) != tc.valid {
			t.Fatalf("data=%s err=%v", data, err)
		}
		if err != nil && (len(err.Error()) > 5000 || strings.Contains(err.Error(), "signal: killed")) {
			t.Fatal(err)
		}
		if strings.Contains(tc.script, "underlying Git failure") && (err == nil || !strings.Contains(err.Error(), "underlying Git failure")) {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec /usr/bin/yes x\n"), 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("projection budget exhausted")
	refusing := func(context.Context, io.Reader, func(int64) error) ([]byte, error) { return nil, sentinel }
	if _, err := store.LoadInventoryObject(context.Background(), sha, reserve, refusing); !errors.Is(err, sentinel) || strings.Contains(err.Error(), "signal: killed") {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancelled := func(context.Context, io.Reader, func(int64) error) ([]byte, error) {
		cancel()
		return nil, context.Canceled
	}
	if _, err := store.LoadInventoryObject(ctx, sha, reserve, cancelled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// Both calls returned only after Wait reaped the child; no helper survives.
}
