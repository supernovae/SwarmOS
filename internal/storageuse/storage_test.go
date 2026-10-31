package storageuse

import (
	"context"
	"crypto/rand"
	"errors"
	"github.com/supernovae/swarmos/internal/safefs"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestObserveNoMutationAndUnresolvedUse(t *testing.T) {
	m, r := fixture(t)
	defer m.Close()
	use, err := m.AcquireUse(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	key, err := m.key(r)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(m.path, key+".json"))
	if err != nil {
		t.Fatal(err)
	}
	observed, err := Observe(context.Background(), m.path, r, 1<<16, func(int64) error { return nil })
	if err != nil || observed.Active == nil || !*observed.Active {
		t.Fatalf("%+v %v", observed, err)
	}
	after, err := os.ReadFile(filepath.Join(m.path, key+".json"))
	if err != nil || string(before) != string(after) {
		t.Fatalf("recency changed: %v", err)
	}
	if err := use.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	observed, err = Observe(context.Background(), m.path, r, 1<<16, func(int64) error { return nil })
	if err != nil || observed.Active == nil || *observed.Active || !observed.CompleteCoverage {
		t.Fatalf("%+v %v", observed, err)
	}
}
func TestObserveDoesNotCreateAndRespectsBudget(t *testing.T) {
	path := filepath.Join(canonicalTemp(t), "missing")
	_, err := Observe(context.Background(), path, Resource{}, 4096, func(int64) error { return nil })
	if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("created control root: %v", err)
	}
	m, r := fixture(t)
	defer m.Close()
	sentinel := errors.New("refused")
	_, err = Observe(context.Background(), m.path, r, 4096, func(int64) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
}

func TestScopeCompletionAndRetention(t *testing.T) {
	for _, mode := range []string{"success", "command-error", "cancel-verified", "unverified", "panic", "finish-write-failure"} {
		t.Run(mode, func(t *testing.T) {
			m, r := fixture(t)
			s := &Scope{repository: r.Repository, manager: m}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := s.Acquire(ctx, r); err != nil {
				t.Fatal(err)
			}
			if _, err := m.AcquireMaintenance(ctx, r); !errors.Is(err, ErrUnsafe) {
				t.Fatalf("pending admission: %v", err)
			}
			var result error
			if mode == "command-error" {
				result = errors.New("command failed")
			}
			if mode == "cancel-verified" {
				cancel()
			}
			if mode == "unverified" {
				s.Retain()
			}
			if mode == "panic" {
				func() {
					defer func() {
						if recover() == nil {
							t.Error("panic swallowed")
						}
					}()
					defer s.Complete(&result)
					panic("interrupted")
				}()
			} else {
				if mode == "finish-write-failure" {
					m.Close()
				}
				s.Complete(&result)
			}
			// Reopen independently, as a later coordinator would. No process or lease
			// inference may turn a retained or failed completion into permission.
			reopened, err := Open(m.path, r.Location)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			key, _ := reopened.key(r)
			record, err := reopened.read(key, r)
			if err != nil {
				t.Fatal(err)
			}
			pending := 0
			for _, done := range record.Uses {
				if !done {
					pending++
				}
			}
			retained := mode == "unverified" || mode == "panic" || mode == "finish-write-failure"
			if (pending > 0) != retained {
				t.Fatalf("pending=%d retained=%v", pending, retained)
			}
			if mode == "finish-write-failure" && result == nil {
				t.Fatal("completion write failure hidden")
			}
			if mode == "command-error" && (result == nil || result.Error() != "command failed") {
				t.Fatalf("primary error lost: %v", result)
			}
			guard, err := reopened.AcquireMaintenance(context.Background(), r)
			if retained {
				if !errors.Is(err, ErrUnsafe) {
					t.Fatalf("retained maintenance: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				guard.Close()
			}
		})
	}
}

func TestScopeTempAdmissionAndInheritedOwner(t *testing.T) {
	m, _ := fixture(t)
	s := &Scope{repository: "host-unknown", manager: m}
	ctx := context.WithValue(context.Background(), scopeKey{}, s)
	inherited, owner, err := Ensure(ctx, "ignored")
	if err != nil || owner != nil || FromContext(inherited) != s {
		t.Fatalf("inherited owner = %v,%v", owner, err)
	}
	parent := canonicalTemp(t)
	path, err := s.TempDir(ctx, parent, "swarm-native-*", "native-scratch")
	if err != nil {
		t.Fatal(err)
	}
	resource := ResourceFor(s.Repository(), "native-scratch", path)
	if _, err := m.AcquireMaintenance(ctx, resource); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("allocation was not admitted: %v", err)
	}
	if err := s.Finish(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(m.path, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	guard, err := reopened.AcquireMaintenance(ctx, resource)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
}

func TestScopeAdmissionFailureCannotCreateScratch(t *testing.T) {
	m, _ := fixture(t)
	s := &Scope{repository: "host-unknown", manager: m}
	parent := canonicalTemp(t)
	m.Close()
	if _, err := s.TempDir(context.Background(), parent, "swarm-native-*", "native-scratch"); err == nil {
		t.Fatal("closed evidence store admitted scratch")
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("admission failure touched storage: %v,%v", entries, err)
	}
}

func TestManagedCleanupRejectsReplacementAndRetainsUncertain(t *testing.T) {
	for _, mode := range []string{"normal", "replacement", "symlink", "unverified", "panic"} {
		t.Run(mode, func(t *testing.T) {
			m, _ := fixture(t)
			s := &Scope{repository: "host-unknown", manager: m}
			parent := canonicalTemp(t)
			path, err := s.TempDir(context.Background(), parent, "swarm-native-*", "native-scratch")
			if err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(path, "keep")
			if err := os.WriteFile(sentinel, []byte("recovery"), 0600); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "replacement", "symlink":
				if err := os.Rename(path, path+"-original"); err != nil {
					t.Fatal(err)
				}
				if mode == "replacement" {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Symlink(path+"-original", path); err != nil {
						t.Fatal(err)
					}
				}
			case "unverified":
				s.Retain()
			}
			var cleanupErr error
			if mode == "panic" {
				func() {
					defer func() {
						if recover() == nil {
							t.Error("panic swallowed")
						}
					}()
					defer s.CleanupTemp(path, &cleanupErr)
					panic("lost completion")
				}()
			} else {
				s.CleanupTemp(path, &cleanupErr)
			}
			if mode == "replacement" || mode == "symlink" {
				if cleanupErr == nil {
					t.Fatal("replacement removed")
				}
				if _, err := os.Stat(filepath.Join(path+"-original", "keep")); err != nil {
					t.Fatal(err)
				}
			} else if mode == "normal" {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("normal cleanup: %v", err)
				}
			} else {
				if _, err := os.Stat(sentinel); err != nil {
					t.Fatalf("uncertain recovery deleted: %v", err)
				}
			}
			if err := s.Finish(); err != nil {
				t.Fatal(err)
			}
			if mode != "normal" {
				reopened, err := Open(m.path, path)
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				resource := ResourceFor(s.Repository(), "native-scratch", path)
				if guard, err := reopened.AcquireMaintenance(context.Background(), resource); !errors.Is(err, ErrUnsafe) {
					if guard != nil {
						guard.Close()
					}
					t.Fatalf("uncertain cleanup permitted maintenance: %v", err)
				}
			}
		})
	}
}

func TestScopeControlCannotOverlapOriginalWorkspace(t *testing.T) {
	repo := canonicalTemp(t)
	t.Setenv("XDG_CACHE_HOME", repo)
	if runtime.GOOS == "darwin" {
		t.Setenv("HOME", repo)
	}
	ctx, s, err := Ensure(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(canonicalTemp(t), "cache")
	if err := s.Acquire(ctx, ResourceFor(s.Repository(), "agent-build", outside)); err == nil {
		t.Fatal("control evidence could be archived into workspace")
	}
	control, err := ControlRoot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(control); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("control store created before overlap rejection: %v", err)
	}
}

func TestScopeDefaultTempAliasAndExplicitAliasRejection(t *testing.T) {
	parent := canonicalTemp(t)
	alias := filepath.Join(canonicalTemp(t), "temp-alias")
	if err := os.Symlink(parent, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", alias)
	m, _ := fixture(t)
	s := &Scope{repository: "host-unknown", manager: m}
	path, err := s.TempDir(context.Background(), "", "swarm-native-*", "native-scratch")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != parent {
		t.Fatalf("temporary alias not canonical: %s", path)
	}
	var cleanupErr error
	s.CleanupTemp(path, &cleanupErr)
	if cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
	if _, err := s.TempDir(context.Background(), alias, "swarm-native-*", "native-scratch"); err == nil {
		t.Fatal("explicit alias accepted")
	}
	if err := s.Finish(); err != nil {
		t.Fatal(err)
	}
}

type collisionRandom struct {
	source   io.Reader
	path     string
	collided bool
}

func (r *collisionRandom) Read(p []byte) (int, error) {
	if len(p) == 16 {
		for i := range p {
			p[i] = 0
		}
		return len(p), nil
	}
	if !r.collided {
		r.collided = true
		if err := os.Mkdir(r.path, 0700); err != nil {
			return 0, err
		}
	}
	return r.source.Read(p)
}

func TestTempCollisionNeverPromotesCoverage(t *testing.T) {
	m, _ := fixture(t)
	s := &Scope{repository: "host-unknown", manager: m}
	parent := canonicalTemp(t)
	path := filepath.Join(parent, "swarm-native-"+strings.Repeat("0", 32))
	original := rand.Reader
	injected := &collisionRandom{source: original, path: path}
	rand.Reader = injected
	defer func() { rand.Reader = original }()
	if _, err := s.TempDir(context.Background(), parent, "swarm-native-*", "native-scratch"); err == nil {
		t.Fatal("colliding allocation succeeded")
	}
	if !injected.collided {
		t.Fatal("collision fixture did not run")
	}
	if err := s.Finish(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(m.path, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	resource := ResourceFor(s.Repository(), "native-scratch", path)
	if guard, err := reopened.AcquireMaintenance(context.Background(), resource); !errors.Is(err, ErrUnsafe) {
		if guard != nil {
			guard.Close()
		}
		t.Fatalf("collision promoted unrelated storage: %v", err)
	}
}

func TestCleanupExhaustionPreservesPrimaryAndEvidence(t *testing.T) {
	for _, primary := range []error{nil, errors.New("prefetch failed")} {
		m, _ := fixture(t)
		s := &Scope{repository: "host-unknown", manager: m}
		path, err := s.TempDir(context.Background(), canonicalTemp(t), "swarm-prefetch-*", "prefetch-scratch")
		if err != nil {
			t.Fatal(err)
		}
		result := primary
		s.cleanupTemp(path, &result, func(temporary) error { return errors.Join(safefs.ErrCleanupIncomplete, os.ErrPermission) })
		if result != primary || !s.Retained() {
			t.Fatalf("primary changed: %v; retained=%v", result, s.Retained())
		}
		if err := s.Finish(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
		reopened, err := Open(m.path, path)
		if err != nil {
			t.Fatal(err)
		}
		resource := ResourceFor(s.Repository(), "prefetch-scratch", path)
		guard, err := reopened.AcquireMaintenance(context.Background(), resource)
		if guard != nil {
			guard.Close()
		}
		if !errors.Is(err, ErrUnsafe) {
			t.Fatalf("retained evidence permitted maintenance: %v", err)
		}
		reopened.Close()
	}
}

func TestCleanupMissingOwnershipRetainsEvidence(t *testing.T) {
	m, _ := fixture(t)
	s := &Scope{repository: "host-unknown", manager: m}
	path, err := s.TempDir(context.Background(), canonicalTemp(t), "swarm-prefetch-*", "prefetch-scratch")
	if err != nil {
		t.Fatal(err)
	}
	var result error
	s.CleanupTemp(path+"-unowned", &result)
	if !errors.Is(result, ErrUnsafe) || !s.Retained() {
		t.Fatalf("unowned cleanup: %v retained=%v", result, s.Retained())
	}
	if err := s.Finish(); err != nil {
		t.Fatal(err)
	}
	resource := ResourceFor(s.Repository(), "prefetch-scratch", path)
	reopened, err := Open(m.path, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	guard, err := reopened.AcquireMaintenance(context.Background(), resource)
	if guard != nil {
		guard.Close()
	}
	if !errors.Is(err, ErrUnsafe) {
		t.Fatalf("missing ownership cleared evidence: %v", err)
	}
}
