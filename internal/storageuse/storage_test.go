package storageuse

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func canonicalTemp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func fixture(t *testing.T) (*Manager, Resource) {
	t.Helper()
	dir := canonicalTemp(t)
	m, err := Open(filepath.Join(dir, "control"), filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m, Resource{Repository: "common-dir", Class: "agent-build", Identity: "go", Location: filepath.Join(dir, "data")}
}

func TestConcurrentUseAndExclusiveMaintenance(t *testing.T) {
	m, r := fixture(t)
	ctx := context.Background()
	a, err := m.AcquireUse(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.AcquireUse(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	blocked := func() {
		t.Helper()
		if g, err := m.AcquireMaintenance(ctx, r); !errors.Is(err, ErrUnsafe) {
			if g != nil {
				g.Close()
			}
			t.Fatalf("maintenance admitted: %v", err)
		}
	}
	blocked()
	if err = a.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	blocked()
	if err = b.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	g, err := m.AcquireMaintenance(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	// Holding maintenance is a deterministic barrier, not an inactivity timer.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = m.AcquireUse(cancelled, r); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	g.Close()
	v, err := m.read(a.key, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Uses) != 2 {
		t.Fatalf("cancelled admission wrote a use: %#v", v)
	}
	if !v.CompleteCoverage || v.LastUse.Before(v.Created) {
		t.Fatalf("bad metadata: %#v", v)
	}
}

func TestLegacyAndLostRecordsRemainUnknown(t *testing.T) {
	for _, mode := range []string{"legacy", "lost-ledger", "unknown-version", "corrupt", "pending"} {
		t.Run(mode, func(t *testing.T) {
			m, r := fixture(t)
			ctx := context.Background()
			if mode == "legacy" {
				if err := os.Mkdir(r.Location, 0700); err != nil {
					t.Fatal(err)
				}
			}
			u, err := m.AcquireUse(ctx, r)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "pending" {
				if err = u.Finish(ctx); err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "lost-ledger":
				if err = m.root.Remove(u.key + ".json"); err != nil {
					t.Fatal(err)
				}
			case "unknown-version":
				v, e := m.read(u.key, r)
				if e != nil {
					t.Fatal(e)
				}
				v.Version = 9
				if err = m.write(u.key, v); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err = m.root.WriteFileDurable(u.key+".json", []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if g, err := m.AcquireMaintenance(ctx, r); !errors.Is(err, ErrUnsafe) {
				if g != nil {
					g.Close()
				}
				t.Fatalf("maintenance admitted %s: %v", mode, err)
			}
			if mode == "lost-ledger" {
				v, e := m.AcquireUse(ctx, r)
				if e != nil {
					t.Fatal(e)
				}
				if e = v.Finish(ctx); e != nil {
					t.Fatal(e)
				}
				if g, e := m.AcquireMaintenance(ctx, r); e == nil {
					g.Close()
					t.Fatal("missing ledger recertified")
				}
			}
		})
	}
}

func TestWritableContentsCannotSupplyAuthority(t *testing.T) {
	m, r := fixture(t)
	ctx := context.Background()
	u, err := m.AcquireUse(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(r.Location, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".prefetched-v3-go-forged", ".warm-forged", u.key + ".json"} {
		if err = os.WriteFile(filepath.Join(r.Location, name), []byte(`{"CompleteCoverage":true}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if g, err := m.AcquireMaintenance(ctx, r); err == nil {
		g.Close()
		t.Fatal("cache contents completed pending use")
	}
	if err = u.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	g, err := m.AcquireMaintenance(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	g.Close()
}

func TestIdentitySeparationAndOverlap(t *testing.T) {
	m, r := fixture(t)
	a, err := m.key(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"repository", "class", "identity", "location"} {
		s := r
		switch field {
		case "repository":
			s.Repository = "other"
		case "class":
			s.Class = "validate-build"
		case "identity":
			s.Identity = "rust"
		case "location":
			s.Location += "-other"
		}
		b, err := m.key(s)
		if err != nil || a == b {
			t.Fatalf("%s collides: %v", field, err)
		}
	}
	if _, err := Open(filepath.Join(r.Location, "control"), r.Location); err == nil {
		t.Fatal("overlap accepted")
	}
	s := r
	s.Location = filepath.Dir(m.path)
	if _, err = m.AcquireUse(context.Background(), s); err == nil {
		t.Fatal("control exposed through resource")
	}
}

func TestControlRejectsAliases(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			m, r := fixture(t)
			key, err := m.key(r)
			if err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(t.TempDir(), "file")
			if err = os.WriteFile(source, nil, 0600); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(m.path, key+".lock")
			if kind == "symlink" {
				err = os.Symlink(source, dest)
			} else {
				err = os.Link(source, dest)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = m.AcquireUse(context.Background(), r); err == nil {
				t.Fatal("aliased control lock accepted")
			}
		})
	}
}

func TestOldPendingUseHasNoAutomaticRecovery(t *testing.T) {
	m, r := fixture(t)
	u, err := m.AcquireUse(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	v, err := m.read(u.key, r)
	if err != nil {
		t.Fatal(err)
	}
	v.Created = time.Unix(1, 0).UTC()
	v.LastUse = v.Created
	if err = m.write(u.key, v); err != nil {
		t.Fatal(err)
	}
	// There is deliberately no PID, heartbeat or lease-expiry field to recycle.
	// Reopening the manager cannot turn old unresolved evidence into inactivity.
	other, err := Open(m.path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if g, err := other.AcquireMaintenance(context.Background(), r); !errors.Is(err, ErrUnsafe) {
		if g != nil {
			g.Close()
		}
		t.Fatalf("old pending use recovered: %v", err)
	}
}

func TestConcurrentAdmissionsPreserveEveryToken(t *testing.T) {
	m, r := fixture(t)
	const count = 8
	start := make(chan struct{})
	uses := make(chan *Use, count)
	errorsFound := make(chan error, count)
	for range count {
		go func() { <-start; u, err := m.AcquireUse(context.Background(), r); uses <- u; errorsFound <- err }()
	}
	close(start)
	var all []*Use
	for range count {
		if err := <-errorsFound; err != nil {
			t.Fatal(err)
		}
		all = append(all, <-uses)
	}
	v, err := m.read(all[0].key, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Uses) != count {
		t.Fatalf("lost admission: %d", len(v.Uses))
	}
	for _, u := range all {
		if err := u.Finish(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	g, err := m.AcquireMaintenance(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	g.Close()
}
