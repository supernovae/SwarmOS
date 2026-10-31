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
