package storageuse

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestInventoryRegistrationsReadOnlyAndBounded(t *testing.T) {
	root := filepath.Join(t.TempDir(), "control")
	m, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	resource := Resource{Repository: "local/test", Class: "agent-build", Identity: "fixture", Location: filepath.Join(t.TempDir(), "missing")}
	use, err := m.AcquireUse(context.Background(), resource)
	if err != nil {
		t.Fatal(err)
	}
	if err = use.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	contents := map[string]string{}
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(root, f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		contents[f.Name()] = string(b)
	}
	count, entries := 0, 0
	err = VisitResources(context.Background(), root, 1, func() error { entries++; return nil }, func(int64) error { return nil }, func(got Resource) error {
		count++
		if got != resource {
			t.Fatal(got)
		}
		return nil
	})
	if err != nil || count != 1 || entries < 1 {
		t.Fatal(err, count, entries)
	}
	after, _ := os.ReadDir(root)
	if len(after) != len(files) {
		t.Fatal("created files")
	}
	for _, f := range after {
		b, _ := os.ReadFile(filepath.Join(root, f.Name()))
		if string(b) != contents[f.Name()] {
			t.Fatal("mutated", f.Name())
		}
	}
	sentinel := errors.New("refused")
	err = VisitResources(context.Background(), root, 1, func() error { return nil }, func(int64) error { return sentinel }, func(Resource) error { t.Fatal("visited after refusal"); return nil })
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = VisitResources(ctx, root, 1, func() error { return nil }, func(int64) error { return nil }, func(Resource) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestInventoryDeadRecordsDoNotConsumeAllowance(t *testing.T) {
	m, live := fixture(t)
	defer m.Close()
	for n := 0; n < 25; n++ {
		r := live
		r.Location = filepath.Join(t.TempDir(), "missing")
		u, err := m.AcquireUse(t.Context(), r)
		if err != nil {
			t.Fatal(err)
		}
		if err = u.Finish(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	pending := live
	if _, err := m.AcquireUse(t.Context(), pending); err != nil {
		t.Fatal(err)
	}
	count := 0
	if err := VisitResources(t.Context(), m.path, 1, func() error { return nil }, func(int64) error { return nil }, func(r Resource) error {
		count++
		if r != pending {
			t.Errorf("visited dead record %v", r)
		}
		return nil
	}, false); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal(count)
	}
}

func TestInventoryPruneAbsentAdmissionSafety(t *testing.T) {
	m, r := fixture(t)
	defer m.Close()
	u, err := m.AcquireUse(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if pruned, err := m.PruneAbsent(t.Context(), r); pruned || !errors.Is(err, ErrUnsafe) {
		t.Fatal("pending evidence pruned", pruned, err)
	}
	if err = u.Finish(t.Context()); err != nil {
		t.Fatal(err)
	}
	key, _ := m.key(r)
	before, err := os.Stat(filepath.Join(m.path, key+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	// Open the admission inode before pruning, as a waiting producer can do.
	waiter, err := m.root.OpenLock(key + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.Close()
	if pruned, err := m.PruneAbsent(t.Context(), r); err != nil || !pruned {
		t.Fatal(pruned, err)
	}
	after, err := os.Stat(filepath.Join(m.path, key+".lock"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("admission inode changed", err)
	}
	if _, err = os.Stat(filepath.Join(m.path, key+".json")); !os.IsNotExist(err) {
		t.Fatal("registration remains", err)
	}
	// Recreating a pruned resource must recover complete coverage while retaining
	// the same lock, and must clear the marker before persisting pending evidence.
	u, err = m.AcquireUse(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	record, err := m.read(key, r)
	if err != nil || !record.CompleteCoverage {
		t.Fatal(record, err)
	}
	marker, err := os.ReadFile(filepath.Join(m.path, key+".lock"))
	if err != nil || len(marker) != 0 {
		t.Fatal("stale prune marker", err)
	}
	if pruned, err := m.PruneAbsent(t.Context(), r); pruned || !errors.Is(err, ErrUnsafe) {
		t.Fatal("new pending use pruned", pruned, err)
	}
	if err = u.Finish(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(r.Location, 0700); err != nil {
		t.Fatal(err)
	}
	if pruned, err := m.PruneAbsent(t.Context(), r); err != nil || pruned {
		t.Fatal("existing resource pruned", pruned, err)
	}
}
