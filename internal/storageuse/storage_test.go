package storageuse

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/safefs"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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

func TestMaintenanceLockedObservation(t *testing.T) {
	m, r := fixture(t)
	ctx := context.Background()
	u, e := m.AcquireUse(ctx, r)
	if e != nil {
		t.Fatal(e)
	}
	if e = u.Finish(ctx); e != nil {
		t.Fatal(e)
	}
	before, e := Observe(ctx, m.path, r, 16<<10, func(int64) error { return nil })
	if e != nil {
		t.Fatal(e)
	}
	g, e := m.AcquireMaintenance(ctx, r)
	if e != nil {
		t.Fatal(e)
	}
	defer g.Close()
	got := g.Observation()
	if !got.CompleteCoverage || got.Active == nil || *got.Active || !got.LastUse.Equal(before.LastUse) {
		t.Fatal(got, before)
	}
	*got.Active = true
	if *g.Observation().Active {
		t.Fatal("observation changed locked evidence")
	}
	blocked, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, e = m.AcquireUse(blocked, r); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
}

func TestRecoveryEligibility(t *testing.T) {
	p := ProcessIdentity{Boot: "11111111-1111-1111-1111-111111111111", PID: 42, Start: 100}
	boundary := p
	boundary.PID++
	for _, mode := range []string{"gone", "outside", "legacy", "incomplete", "live-coordinator", "live-boundary", "unknown-identity", "multiple-boundaries", "one-unknown", "cancelled", "write-failure", "incomplete-coverage"} {
		t.Run(mode, func(t *testing.T) {
			m, r := fixture(t)
			ctx := context.Background()
			u, err := m.AcquireUse(ctx, r)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := m.read(u.key, r)
			e := &useEvidence{Coordinator: p, Consumers: map[string]ProcessIdentity{strings.Repeat("a", 64): boundary}}
			if err := u.evidence(ctx, func(v *record) error {
				v.Evidence = map[string]*useEvidence{u.token: e}
				switch mode {
				case "outside":
					e.Outside = true
				case "legacy":
					v.Evidence = nil
				case "incomplete":
					e.Consumers[strings.Repeat("b", 64)] = ProcessIdentity{}
				case "unknown-identity":
					e.Coordinator.Boot = "unreadable"
				case "multiple-boundaries":
					other := boundary
					other.PID++
					e.Consumers[strings.Repeat("b", 64)] = other
				case "one-unknown":
					e.Consumers[strings.Repeat("b", 64)] = ProcessIdentity{}
				case "incomplete-coverage":
					v.CompleteCoverage = false
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			now := before.LastUse.Add(time.Hour)
			if mode == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if mode == "write-failure" {
				now = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			}

			gone := func(q ProcessIdentity) bool {
				return !(mode == "live-coordinator" && q == p || mode == "live-boundary" && q == boundary)
			}
			changed, err := m.recover(ctx, r, now, gone)
			eligible := mode == "gone" || mode == "multiple-boundaries" || mode == "incomplete-coverage"
			if mode == "cancelled" || mode == "write-failure" {
				if mode == "write-failure" {
					preserved, readErr := m.read(u.key, r)
					if readErr != nil || preserved.Uses[u.token] || !preserved.LastUse.Equal(before.LastUse) {
						t.Fatalf("failed durable write changed record: %+v %v", preserved, readErr)
					}
				}
				if err == nil || changed {
					t.Fatalf("changed=%v err=%v", changed, err)
				}
				return
			}
			if err != nil || changed != eligible {
				t.Fatalf("changed=%v expected=%v err=%v", changed, eligible, err)
			}
			after, err := m.read(u.key, r)
			if err != nil {
				t.Fatal(err)
			}
			if after.Uses[u.token] != eligible {
				t.Fatal("wrong completion")
			}
			if after.CompleteCoverage != before.CompleteCoverage && mode != "incomplete-coverage" {
				t.Fatal("coverage changed")
			}
			if mode == "incomplete-coverage" && after.CompleteCoverage {
				t.Fatal("coverage upgraded")
			}
			if eligible && !after.LastUse.Equal(now) {
				t.Fatal("recency not advanced")
			}
			again, err := m.recover(ctx, r, now.Add(time.Hour), gone)
			if err != nil || again {
				t.Fatalf("non-idempotent: %v %v", again, err)
			}
			final, _ := m.read(u.key, r)
			if !final.LastUse.Equal(after.LastUse) {
				t.Fatal("repeated pass changed recency")
			}
		})
	}
}

func TestRecoveryProcEvidence(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux proc evidence")
	}
	p, err := CaptureProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if processGone(p) {
		t.Fatal("live process declared gone")
	}
	other := p
	other.Start++
	if !processGone(other) {
		t.Fatal("different start ticks did not prove absence")
	}
	other = p
	other.Boot = "22222222-2222-2222-2222-222222222222"
	if !processGone(other) {
		t.Fatal("different boot did not prove absence")
	}
	for _, name := range []string{"simple", "name with spaces", "a (nested) name)"} {
		stat := strconv.Itoa(p.PID) + " (" + name + ") S " + strings.Repeat("0 ", 18) + "123 0"
		start, err := processStart([]byte(stat), p.PID)
		if err != nil || start != 123 {
			t.Fatalf("%q: %d %v", name, start, err)
		}
	}
	for _, stat := range []string{"", "42 no parentheses", "42 (name) S 0", "42 (name) " + strings.Repeat("x ", 20)} {
		if _, err := processStart([]byte(stat), 42); err == nil {
			t.Fatalf("accepted %q", stat)
		}
	}
}

func TestRecoveryMultipleUsesAndReadOnly(t *testing.T) {
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
	p := ProcessIdentity{Boot: "11111111-1111-1111-1111-111111111111", PID: 42, Start: 100}
	if err := a.evidence(ctx, func(v *record) error {
		v.Evidence = map[string]*useEvidence{a.token: {Coordinator: p, Consumers: map[string]ProcessIdentity{strings.Repeat("a", 64): p}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.path, a.key+".json")
	before, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	if _, err := Observe(ctx, m.path, r, 16384, func(int64) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if g, err := m.AcquireMaintenance(ctx, r); err == nil {
		g.Close()
		t.Fatal("pending admitted")
	}
	after, _ := os.ReadFile(path)
	later, _ := os.Stat(path)
	if string(before) != string(after) || !info.ModTime().Equal(later.ModTime()) {
		t.Fatal("read-only call wrote evidence")
	}
	if changed, err := m.recover(ctx, r, time.Now(), func(ProcessIdentity) bool { return true }); err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	if g, err := m.AcquireMaintenance(ctx, r); err == nil {
		g.Close()
		t.Fatal("other pending token ignored")
	}
	if err := b.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	if g, err := m.AcquireMaintenance(ctx, r); err != nil {
		t.Fatal(err)
	} else {
		g.Close()
	}
}

func TestRecoveryScopeReservationAndContamination(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux identities")
	}
	for _, mode := range []string{"sealed", "before-start", "outside-before", "outside-after", "concurrent", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			m, r := fixture(t)
			ctx := context.Background()
			s := &Scope{manager: m, repository: r.Repository}
			if mode == "outside-before" {
				if err := s.OutsideConsumer(ctx); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			if mode == "legacy" {
				err = s.Acquire(ctx, r)
			} else {
				err = s.AcquirePrepared(ctx, r)
			}
			if err != nil {
				t.Fatal(err)
			}
			b, err := s.ReserveBoundary(ctx, []string{r.Location})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "concurrent" {
				if _, err := s.ReserveBoundary(ctx, []string{r.Location}); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "before-start" {
				p, err := CaptureProcess(os.Getpid())
				if err != nil {
					t.Fatal(err)
				}
				if err := b.Seal(ctx, p); err != nil {
					t.Fatal(err)
				}
				if err := b.Seal(ctx, p); mode != "legacy" && mode != "outside-before" && err == nil {
					t.Fatal("reservation could be resealed")
				}
			}
			if mode == "outside-after" {
				if err := s.OutsideConsumer(ctx); err != nil {
					t.Fatal(err)
				}
			}
			changed, err := m.recover(ctx, r, time.Now(), func(ProcessIdentity) bool { return true })
			if err != nil || changed != (mode == "sealed") {
				t.Fatalf("%v %v", changed, err)
			}
		})
	}
}

func TestRecoveryRejectsDuplicateEvidence(t *testing.T) {
	for _, data := range []string{`{"a":1,"a":2}`, `{"Evidence":{"token":{"Consumers":{"id":{},"id":{}}}}}`, `{} {}`} {
		if err := uniqueJSON([]byte(data)); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

func TestRecoveryFencesConcurrentAdmissionAndFinish(t *testing.T) {
	m, r := fixture(t)
	ctx := context.Background()
	u, err := m.AcquireUse(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	p := ProcessIdentity{Boot: "11111111-1111-1111-1111-111111111111", PID: 42, Start: 100}
	if err := u.evidence(ctx, func(v *record) error {
		v.Evidence = map[string]*useEvidence{u.token: {Coordinator: p, Consumers: map[string]ProcessIdentity{strings.Repeat("a", 64): p}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		first := true
		_, err := m.recover(ctx, r, time.Now().Add(time.Hour), func(ProcessIdentity) bool {
			if first {
				first = false
				close(entered)
				<-release
			}
			return true
		})
		finished <- err
	}()
	<-entered
	admitted := make(chan *Use, 1)
	errs := make(chan error, 1)
	go func() { v, err := m.AcquireUse(ctx, r); admitted <- v; errs <- err }()
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	next := <-admitted
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if err := u.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	v, err := m.read(u.key, r)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Uses[u.token] || v.Uses[next.token] || len(v.Uses) != 2 {
		t.Fatal("recovery lost concurrent admission")
	}
}

func TestRecoveryCancellationBeforePersistence(t *testing.T) {
	m, r := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u, err := m.AcquireUse(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	p := ProcessIdentity{Boot: "11111111-1111-1111-1111-111111111111", PID: 42, Start: 100}
	if err := u.evidence(ctx, func(v *record) error {
		v.Evidence = map[string]*useEvidence{u.token: {Coordinator: p, Consumers: map[string]ProcessIdentity{strings.Repeat("a", 64): p}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	changed, err := m.recover(ctx, r, time.Now(), func(ProcessIdentity) bool { cancel(); return true })
	if changed || !errors.Is(err, context.Canceled) {
		t.Fatalf("%v %v", changed, err)
	}
	v, err := m.read(u.key, r)
	if err != nil || v.Uses[u.token] {
		t.Fatal("cancelled recovery persisted", err)
	}
}

func TestRecoveryUnknownProcessEvidence(t *testing.T) {
	p := ProcessIdentity{Boot: "11111111-1111-1111-1111-111111111111", PID: 42, Start: 100}
	for _, mode := range []string{"same", "different-start", "different-boot", "missing-pid", "missing-stat-only", "boot-permission", "proc-permission", "pid-permission", "malformed-boot", "malformed-stat", "io-error"} {
		t.Run(mode, func(t *testing.T) {
			read := func(path string) ([]byte, error) {
				if strings.HasSuffix(path, "boot_id") {
					switch mode {
					case "boot-permission":
						return nil, os.ErrPermission
					case "malformed-boot":
						return []byte("unknown"), nil
					case "different-boot":
						return []byte("22222222-2222-2222-2222-222222222222"), nil
					}
					return []byte(p.Boot), nil
				}
				switch mode {
				case "missing-pid", "missing-stat-only", "pid-permission":
					return nil, os.ErrNotExist
				case "proc-permission":
					return nil, os.ErrPermission
				case "io-error":
					return nil, errors.New("read failure")
				case "malformed-stat":
					return []byte("42 (broken)"), nil
				}
				ticks := "100"
				if mode == "different-start" {
					ticks = "101"
				}
				return []byte("42 (name with (parentheses)) S " + strings.Repeat("0 ", 18) + ticks), nil
			}
			stat := func(string) (os.FileInfo, error) {
				if mode == "missing-pid" {
					return nil, os.ErrNotExist
				}
				if mode == "pid-permission" {
					return nil, os.ErrPermission
				}
				return nil, nil
			}
			gone := processGoneRead(p, read, stat)
			want := mode == "different-start" || mode == "different-boot" || mode == "missing-pid"
			if gone != want {
				t.Fatalf("gone=%v want=%v", gone, want)
			}
		})
	}
}

func TestRecoveryOverlappingUnclassifiedAdmission(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux identities")
	}
	m, r := fixture(t)
	ctx := context.Background()
	s := &Scope{manager: m, repository: r.Repository}
	if err := s.AcquirePrepared(ctx, r); err != nil {
		t.Fatal(err)
	}
	b, err := s.ReserveBoundary(ctx, []string{r.Location})
	if err != nil {
		t.Fatal(err)
	}
	p, err := CaptureProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Seal(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.Acquire(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.AcquirePrepared(ctx, r); err != nil {
		t.Fatal(err)
	}
	later, err := s.ReserveBoundary(ctx, []string{r.Location})
	if err != nil {
		t.Fatal(err)
	}
	if err := later.Seal(ctx, p); err != nil {
		t.Fatal(err)
	}
	if changed, err := m.recover(ctx, r, time.Now(), func(ProcessIdentity) bool { return true }); changed || err != nil {
		t.Fatalf("unclassified consumer erased: %v %v", changed, err)
	}
}

func TestRecoveryEvidenceCapacityRetainsWithoutBlockingTools(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux identities")
	}
	m, r := fixture(t)
	ctx := context.Background()
	s := &Scope{manager: m, repository: r.Repository}
	if err := s.AcquirePrepared(ctx, r); err != nil {
		t.Fatal(err)
	}
	u := s.uses[0]
	p, err := CaptureProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := u.evidence(ctx, func(v *record) error {
		e := v.Evidence[u.token]
		for n := 0; n < 256; n++ {
			e.Consumers[fmt.Sprintf("%064x", n)] = p
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	b, err := s.ReserveBoundary(ctx, []string{r.Location})
	if err != nil {
		t.Fatal("evidence cap blocked admission", err)
	}
	if err := b.Seal(ctx, p); err != nil {
		t.Fatal("outside evidence blocked execution", err)
	}
	v, err := m.read(u.key, r)
	if err != nil {
		t.Fatal(err)
	}
	e := v.Evidence[u.token]
	if !e.Outside || len(e.Consumers) != 256 {
		t.Fatal("capacity did not permanently disqualify bounded evidence")
	}
}

func TestFinishedHistoryBounded(t *testing.T) {
	m, r := fixture(t)
	ctx := context.Background()
	for i := 0; i < 3000; i++ {
		u, err := m.AcquireUse(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		if err = u.Finish(ctx); err != nil {
			t.Fatal(err)
		}
		if err = u.Finish(ctx); err != nil {
			t.Fatal(err)
		}
	}
	key, _ := m.key(r)
	info, err := m.root.Lstat(key + ".json")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() >= 16384 {
		t.Fatalf("history grew to %d", info.Size())
	}
	v, err := m.read(key, r)
	if err != nil {
		t.Fatal(err)
	}
	if v.Uses == nil || !v.CompleteCoverage {
		t.Fatal("lost coverage")
	}
	g, err := m.AcquireMaintenance(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	g.Close()
}

func TestCompactPreservesPendingAndValidEvidence(t *testing.T) {
	m, r := fixture(t)
	ctx := context.Background()
	u, err := m.AcquireUse(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := m.key(r)
	v, err := m.read(key, r)
	if err != nil {
		t.Fatal(err)
	}
	v.Evidence = map[string]*useEvidence{u.token: {Consumers: map[string]ProcessIdentity{"pending": {}}}}
	for i := 0; i < 400; i++ {
		token := fmt.Sprintf("%064x", i)
		v.Uses[token] = true
		v.Evidence[token] = &useEvidence{Consumers: map[string]ProcessIdentity{"finished": {}}}
	}
	v.CompleteCoverage = false
	if err = m.write(key, v); err != nil {
		t.Fatal(err)
	}
	before, _ := m.root.Lstat(key + ".json")
	if before.Size() <= 16384 {
		t.Fatal("fixture not oversized")
	}
	changed, err := m.Compact(ctx, r)
	if err != nil || !changed {
		t.Fatalf("compact: %v %v", changed, err)
	}
	after, err := m.read(key, r)
	if err != nil {
		t.Fatalf("#418 validation failed: %v", err)
	}
	if len(after.Uses) != 1 || after.Uses[u.token] || len(after.Evidence) != 1 || after.Evidence[u.token] == nil {
		t.Fatal("pending evidence changed")
	}
	if after.CompleteCoverage || !after.Created.Equal(v.Created) || !after.LastUse.Equal(v.LastUse) {
		t.Fatal("metadata changed")
	}
	if changed, err = m.Compact(ctx, r); err != nil || changed {
		t.Fatalf("idempotence: %v %v", changed, err)
	}
}

func TestCompactAdmissionExclusionAndCancellation(t *testing.T) {
	m, r := fixture(t)
	ctx := context.Background()
	u, err := m.AcquireUse(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if err = u.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	guard, err := m.AcquireMaintenance(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	blocked, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	if _, err = m.Compact(blocked, r); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("maintenance exclusion: %v", err)
	}
	cancel()
	guard.Close()
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 100; i++ {
			if _, e := m.Compact(ctx, r); e != nil {
				done <- e
				return
			}
		}
		done <- nil
	}()
	for i := 0; i < 100; i++ {
		use, e := m.AcquireUse(ctx, r)
		if e != nil {
			t.Fatal(e)
		}
		if e = use.Finish(ctx); e != nil {
			t.Fatal(e)
		}
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	guard, err = m.AcquireMaintenance(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	guard.Close()
}

func TestCompactFinishedEvidencePreservesEmptyCoverage(t *testing.T) {
	m, r := fixture(t)
	ctx := context.Background()
	u, err := m.AcquireUse(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if err = u.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	v, err := m.read(u.key, r)
	if err != nil {
		t.Fatal(err)
	}
	v.Evidence = map[string]*useEvidence{u.token: {Consumers: map[string]ProcessIdentity{"finished": {}}}}
	if err = m.write(u.key, v); err != nil {
		t.Fatal(err)
	}
	if changed, err := m.Compact(ctx, r); err != nil || !changed {
		t.Fatalf("compact: %v %v", changed, err)
	}
	after, err := m.read(u.key, r)
	if err != nil {
		t.Fatalf("#418 validation rejected compacted empty coverage: %v", err)
	}
	if after.Uses == nil || len(after.Uses) != 0 || len(after.Evidence) != 0 || !after.CompleteCoverage || !after.Created.Equal(v.Created) || !after.LastUse.Equal(v.LastUse) {
		t.Fatalf("compaction changed coverage or metadata: %+v", after)
	}
	guard, err := m.AcquireMaintenance(ctx, r)
	if err != nil {
		t.Fatalf("empty completed coverage lost maintenance eligibility: %v", err)
	}
	guard.Close()
}

// Native observations use the same parser and identity checks as teardown.
// Kernel files are injected here; this test does not claim real containment.
func TestNativeBoundaryIdentityAndEmpty(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux stat identity")
	}
	root := canonicalTemp(t)
	group := filepath.Join(root, "group")
	if err := os.Mkdir(group, 0700); err != nil {
		t.Fatal(err)
	}
	const unit = "swarm-native-0123456789abcdef0123456789abcdef.scope"
	const boot = "11111111-1111-1111-1111-111111111111"
	baseRead := func(path string) ([]byte, error) {
		switch path {
		case "/proc/self/cgroup":
			return []byte("0::/user.slice/" + unit + "\n"), nil
		case "/proc/sys/kernel/random/boot_id":
			return []byte(boot), nil
		case "/sys/fs/cgroup/cgroup.controllers":
			return []byte("memory pids"), nil
		default:
			return []byte("populated 0\nfrozen 0\n"), nil
		}
	}
	baseStat := func(path string) (os.FileInfo, error) {
		if path == "/sys/fs/cgroup" {
			return os.Stat(root)
		}
		return os.Stat(group)
	}
	n, err := CaptureNativeIdentity(unit, baseRead, baseStat)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, events, boot                          string
		missing, denied, changedMount, changedGroup bool
		deniedRead, deniedStat                      string
		want                                        bool
	}{
		{name: "empty", events: "populated 0\nfrozen 0\n", want: true},
		{name: "descendant", events: "populated 1\nfrozen 0\n"},
		{name: "duplicate", events: "populated 0\npopulated 1\n"},
		{name: "missing field", events: "frozen 0\n"},
		{name: "loose match", events: "populated 01\n"},
		{name: "permission", denied: true},
		{name: "controllers unreadable", deniedRead: "/sys/fs/cgroup/cgroup.controllers"},
		{name: "events unreadable", deniedRead: "/sys/fs/cgroup/user.slice/" + unit + "/cgroup.events"},
		{name: "root unreadable", deniedStat: "/sys/fs/cgroup"},
		{name: "group unreadable", deniedStat: "/sys/fs/cgroup/user.slice/" + unit},

		{name: "removed after creation", missing: true, want: true},
		{name: "unmounted", changedMount: true},
		{name: "reused inode", changedGroup: true},
		{name: "reboot", boot: "22222222-2222-2222-2222-222222222222", want: true},
		{name: "invalid boot", boot: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := func(path string) ([]byte, error) {
				if tc.denied || path == tc.deniedRead {
					return nil, os.ErrPermission
				}
				if path == "/proc/sys/kernel/random/boot_id" && tc.boot != "" {
					return []byte(tc.boot), nil
				}
				if strings.HasSuffix(path, "cgroup.events") {
					return []byte(tc.events), nil
				}
				return baseRead(path)
			}
			stat := func(path string) (os.FileInfo, error) {
				if path == tc.deniedStat {
					return nil, os.ErrPermission
				}
				if path != "/sys/fs/cgroup" && tc.missing {
					return nil, os.ErrNotExist
				}
				if path == "/sys/fs/cgroup" && tc.changedMount {
					return os.Stat(group)
				}
				if path != "/sys/fs/cgroup" && tc.changedGroup {
					return os.Stat(root)
				}
				return baseStat(path)
			}
			reason := map[string]string{
				"descendant": "populated=\"1\"", "duplicate": "malformed cgroup.events",
				"missing field": "populated=\"\"", "loose match": "populated=\"01\"",
				"permission": "/proc/sys/kernel/random/boot_id", "unmounted": "mount identity mismatch",
				"reused inode": "cgroup identity mismatch", "invalid boot": "invalid boot identity",
			}
			if tc.deniedRead != "" {
				reason[tc.name] = "read " + tc.deniedRead
			}
			if tc.deniedStat != "" {
				reason[tc.name] = "stat " + tc.deniedStat
			}
			if err := CheckNativeBoundaryEmpty(n, read, stat); !tc.want {
				if !errors.Is(err, ErrUnsafe) || !strings.Contains(err.Error(), reason[tc.name]) {
					t.Fatalf("diagnostic: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if err := CheckNativeBoundaryEmpty(NativeIdentity{}, read, stat); !errors.Is(err, ErrUnsafe) || !strings.Contains(err.Error(), "invalid native identity") {
				t.Fatal(err)
			}
			if got := NativeBoundaryEmpty(n, read, stat); got != tc.want {
				t.Fatalf("empty=%v, want %v", got, tc.want)
			}
			if NativeBoundaryEmpty(NativeIdentity{}, read, stat) {
				t.Fatal("unacknowledged creation recovered")
			}
		})
	}
	for _, membership := range []string{"0::/wrong.scope", "0::/user.slice/" + unit + "\n0::/user.slice/" + unit, "0::/a/../" + unit, "1:memory:/" + unit} {
		read := func(path string) ([]byte, error) {
			if path == "/proc/self/cgroup" {
				return []byte(membership), nil
			}
			return baseRead(path)
		}
		if _, err := CaptureNativeIdentity(unit, read, baseStat); err == nil {
			t.Fatalf("accepted membership %q", membership)
		}
	}
}

func TestNativeRecoveryReservations(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux identities")
	}
	for _, mode := range []string{"reserved", "sealed live", "sealed empty", "outside before", "outside after", "mixed live", "mixed empty", "coordinator live", "later admission", "bad identity", "duplicate identity", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			m, r := fixture(t)
			ctx := context.Background()
			s := &Scope{manager: m, repository: r.Repository}
			if mode == "outside before" {
				if err := s.OutsideConsumer(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.AcquirePrepared(ctx, r); err != nil {
				t.Fatal(err)
			}
			b, err := s.ReserveNative(ctx)
			if err != nil {
				t.Fatal(err)
			}
			n := NativeIdentity{Boot: "11111111-1111-1111-1111-111111111111", Unit: "swarm-native-0123456789abcdef0123456789abcdef.scope", Group: "/user.slice/swarm-native-0123456789abcdef0123456789abcdef.scope", MountDevice: 1, MountInode: 2, Inode: 3}
			if mode != "reserved" {
				if err = b.Seal(ctx, n); err != nil {
					t.Fatal(err)
				}
				if err = b.Seal(ctx, n); err == nil {
					t.Fatal("resealed native reservation")
				}
			}
			if mode == "outside after" {
				if err = s.OutsideConsumer(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(mode, "mixed") {
				ns, err := s.ReserveBoundary(ctx, []string{r.Location})
				if err != nil {
					t.Fatal(err)
				}
				if err = ns.Seal(ctx, ProcessIdentity{Boot: n.Boot, PID: 42, Start: 99}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "later admission" {
				if err = s.AcquirePrepared(ctx, r); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "bad identity" || mode == "duplicate identity" {
				u := s.uses[0]
				if err = u.evidence(ctx, func(v *record) error {
					e := v.Evidence[u.token]
					if mode == "bad identity" {
						e.Native[b.id] = NativeEvidence{Identity: NativeIdentity{}, Empty: true}
					} else {
						e.Native[strings.Repeat("a", 64)] = e.Native[b.id]
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			changed, err := m.recoverNative(ctx, r, time.Now(), func(p ProcessIdentity) bool {
				return mode != "coordinator live" && (mode != "mixed live" || p.PID != 42)
			}, func(NativeIdentity) bool { return mode != "sealed live" })
			if mode == "canceled" {
				if !errors.Is(err, context.Canceled) || changed {
					t.Fatalf("canceled recovery: %v %v", changed, err)
				}
				return
			}
			want := mode == "sealed empty" || mode == "mixed empty" || mode == "later admission"
			if err != nil || changed != want {
				t.Fatalf("recovery %v %v, want %v", changed, err, want)
			}
			if !want {
				if maintenance, err := m.AcquireMaintenance(ctx, r); err != nil && !errors.Is(err, ErrUnsafe) {
					t.Fatal(err)
				} else if maintenance != nil {
					maintenance.Close()
					t.Fatal("pending native use released")
				}
			}
		})
	}
}

func TestNativeReservationPartialPersistence(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux identities")
	}
	m, r := fixture(t)
	ctx := context.Background()
	s := &Scope{manager: m, repository: r.Repository}
	if err := s.AcquirePrepared(ctx, r); err != nil {
		t.Fatal(err)
	}
	other := r
	other.Identity += "-other"
	other.Location += "-other"
	if err := s.AcquirePrepared(ctx, other); err != nil {
		t.Fatal(err)
	}
	reservation, err := s.ReserveNative(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A failed second durable write must not advance in-memory acknowledgement.
	key, err := m.key(other)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.root.WriteFileDurable(key+".json", []byte("invalid ledger"), 0600); err != nil {
		t.Fatal(err)
	}
	n := NativeIdentity{Boot: "11111111-1111-1111-1111-111111111111", Unit: "swarm-native-0123456789abcdef0123456789abcdef.scope", Group: "/user.slice/swarm-native-0123456789abcdef0123456789abcdef.scope", MountDevice: 1, MountInode: 2, Inode: 3}
	if err = reservation.Seal(ctx, n); err == nil {
		t.Fatal("partial identity write was acknowledged")
	}
	if s.native[reservation.id] != (NativeEvidence{}) {
		t.Fatal("failed persistence advanced native identity")
	}
	if err = reservation.MarkEmpty(ctx); err == nil {
		t.Fatal("partial registration acquired empty evidence")
	}
}

func TestNativeEvidenceLimitIsPermanentOutside(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux identities")
	}
	m, r := fixture(t)
	ctx := context.Background()
	s := &Scope{manager: m, repository: r.Repository, native: map[string]NativeEvidence{}}
	// Exhaust memory-side registration without repeatedly writing a large ledger.
	for i := 0; i < 256; i++ {
		s.native[fmt.Sprintf("%064x", i)] = NativeEvidence{}
	}
	if err := s.AcquirePrepared(ctx, r); err != nil {
		t.Fatal(err)
	}
	if reservation, err := s.ReserveNative(ctx); err != nil || reservation != nil {
		t.Fatalf("exhaustion: %v %v", reservation, err)
	}
	if !s.outside {
		t.Fatal("exhaustion did not contaminate future uses")
	}
	if err := s.AcquirePrepared(ctx, r); err != nil {
		t.Fatal(err)
	}
	key, _ := m.key(r)
	record, err := m.read(key, r)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range record.Evidence {
		if !e.Outside {
			t.Fatal("evidence exhaustion was not permanent")
		}
	}
}

func TestNativeReservationConcurrentAdmissionAndDiagnostics(t *testing.T) {
	for _, mode := range []string{"concurrent admission", "changed evidence", "missing use", "verification", "ledger read", "lock timeout"} {
		t.Run(mode, func(t *testing.T) {
			m, r := fixture(t)
			ctx := context.Background()
			s := &Scope{manager: m, repository: r.Repository}
			if err := s.AcquirePrepared(ctx, r); err != nil {
				t.Fatal(err)
			}
			b, err := s.ReserveNative(ctx)
			if err != nil {
				t.Fatal(err)
			}
			n := NativeIdentity{Boot: "11111111-1111-1111-1111-111111111111", Unit: "swarm-native-0123456789abcdef0123456789abcdef.scope", Group: "/user.slice/swarm-native-0123456789abcdef0123456789abcdef.scope", MountDevice: 1, MountInode: 2, Inode: 3}
			if err := b.Seal(ctx, n); err != nil {
				t.Fatal(err)
			}
			u := s.uses[0]
			want := ""
			switch mode {
			case "changed evidence":
				want = "native reservation evidence missing or changed"
				if err := u.evidence(ctx, func(v *record) error { delete(v.Evidence[u.token].Native, b.id); return nil }); err != nil {
					t.Fatal(err)
				}
			case "missing use":
				want = "storage use no longer registered"
				if err := u.evidence(ctx, func(v *record) error { delete(v.Uses, u.token); delete(v.Evidence, u.token); return nil }); err != nil {
					t.Fatal(err)
				}
			case "verification":
				want = "reservation MarkEmpty verification"
			case "ledger read":
				want = "storage evidence read"
				if err := m.root.WriteFileDurable(u.key+".json", []byte("invalid ledger"), 0600); err != nil {
					t.Fatal(err)
				}
			case "lock timeout":
				want = "storage evidence lock"
				f, _, err := m.lock(ctx, u.key)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock(f)
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 25*time.Millisecond)
				defer cancel()

			}
			done := make(chan error, 1)
			if mode == "concurrent admission" {
				go func() {
					for i := 0; i < 20; i++ {
						if err := s.AcquirePrepared(ctx, r); err != nil {
							done <- err
							return
						}
					}
					done <- nil
				}()
			}
			err = b.markEmptyWith(ctx, func(got NativeIdentity) error {
				if got != n {
					t.Fatalf("identity changed: %+v", got)
				}
				if mode == "verification" {
					return fmt.Errorf("%w: unreadable cgroup.events", ErrUnsafe)
				}
				return nil
			})
			if want != "" {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("err=%v want=%s", err, want)
				}
				if mode == "lock timeout" {
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatal(err)
					}
				} else if mode != "ledger read" && !errors.Is(err, ErrUnsafe) {
					t.Fatal(err)
				}
				if s.native[b.id].Empty {
					t.Fatal("failed persistence marked empty")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				for _, u := range s.uses {
					v, err := m.read(u.key, u.resource)
					if err != nil {
						t.Fatal(err)
					}
					if !v.Evidence[u.token].Native[b.id].Empty {
						t.Fatal("admission lost empty evidence")
					}
				}
			}
		})
	}
}
