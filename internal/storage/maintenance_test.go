package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/registry"
	"github.com/supernovae/swarmos/internal/storageuse"
)

func maintenanceFixture(t *testing.T) (MaintenanceOptions, *storageuse.Manager, storageuse.Resource) {
	t.Helper()
	ro, m, r := reclaimFixture(t, "agent-build")
	root := filepath.Dir(ro.CacheRoot)
	registryPath := filepath.Join(root, "registry.yaml")
	if _, e := registry.New(registryPath).Add("fixture/repo", ro.Scopes[0].Root, "local"); e != nil {
		t.Fatal(e)
	}
	o := MaintenanceOptions{Options: Options{RegistryPath: registryPath, CacheRoot: ro.CacheRoot, UseRoot: ro.ControlRoot, Backend: "local"}, StateRoot: filepath.Join(root, "maintenance"), Now: time.Now().Add(30 * 24 * time.Hour).UTC(), Retention: completeRetention(), Policy: MaintenancePolicy{TargetBytes: 1, IdleAge: 14 * 24 * time.Hour, Interval: 30 * time.Minute, WarningInterval: 10 * time.Minute}}
	return o, m, r
}
func setLastUse(t *testing.T, root string, r storageuse.Resource, last time.Time) {
	t.Helper()
	data, _ := json.Marshal(r)
	h := sha256.Sum256(data)
	path := filepath.Join(root, hex.EncodeToString(h[:])+".json")
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var v map[string]any
	if e = json.Unmarshal(raw, &v); e != nil {
		t.Fatal(e)
	}
	v["Created"] = last.Add(-time.Hour)
	v["LastUse"] = last
	raw, e = json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
}
func TestMaintenanceAggregateBudgetAndOrder(t *testing.T) {
	o, m, first := maintenanceFixture(t)
	// Individually small repositories collectively exceed a target. The order is
	// the reverse of directory mtime, across every cache owner/ecosystem.
	resources := []storageuse.Resource{first}
	for repo := 0; repo < 4; repo++ {
		root := filepath.Join(filepath.Dir(o.CacheRoot), fmt.Sprintf("repo-%d", repo))
		if data, e := exec.Command("git", "init", "-q", root).CombinedOutput(); e != nil {
			t.Fatal(string(data), e)
		}
		if _, e := registry.New(o.RegistryPath).Add(fmt.Sprintf("fixture/repo%d", repo), root, "local"); e != nil {
			t.Fatal(e)
		}
		h := sha256.Sum256([]byte(filepath.Join(root, ".git")))
		key := hex.EncodeToString(h[:16])
		for _, class := range []string{"agent-build", "validate-build", "agent-deps", "validate-deps"} {
			for _, eco := range []string{"go", "rust", "c-cpp"} {
				r := storageuse.ResourceFor(key, class, filepath.Join(o.CacheRoot, class, key, eco))
				u, e := m.AcquireUse(context.Background(), r)
				if e != nil {
					t.Fatal(e)
				}
				put(t, filepath.Join(r.Location, "data"), "cache")
				if e = u.Finish(context.Background()); e != nil {
					t.Fatal(e)
				}
				resources = append(resources, r)
			}
		}
	}
	o.allocation = func(_ os.FileInfo, blocks int64) int64 { return blocks * (1 << 19) }
	var total int64
	for i, r := range resources {
		setLastUse(t, o.UseRoot, r, o.Now.Add(-time.Duration(60-i)*24*time.Hour))
		if e := os.Chtimes(r.Location, o.Now.Add(time.Duration(i)*time.Hour), o.Now.Add(time.Duration(i)*time.Hour)); e != nil {
			t.Fatal(e)
		}
		s := reclaimScanner(context.Background(), ReclaimOptions{Limits: DefaultLimits()})
		s.opts.allocation = o.allocation
		u := usage(s, r.Location)
		if u.AllocatedBytes == nil {
			t.Skip("allocated bytes unavailable")
		}
		total += *u.AllocatedBytes
	}
	if total <= 45_000_000_000 {
		t.Fatal("aggregate fixture too small", total)
	}
	o.Policy.TargetBytes = 40 << 30
	calls := 0
	o.Retention = retentionFixture(func(_ context.Context, _ RepositoryScope, v func(RetentionEvidence) error) error {
		calls++
		return v(RetentionEvidence{Complete: true, Stable: true})
	})
	summary, _, e := Maintain(context.Background(), o)
	if e != nil {
		t.Fatal(e)
	}
	if summary.RemovedNamespaceBytes <= 0 || summary.ExcessBytes != 0 || calls != 5 {
		t.Fatal(summary, calls)
	}
	if _, e = os.Stat(first.Location); !os.IsNotExist(e) {
		t.Fatal("oldest retained", e)
	}
	retained := false
	removed := 0
	for _, r := range resources {
		_, e := os.Stat(r.Location)
		if os.IsNotExist(e) {
			if retained {
				t.Fatal("eviction did not follow oldest-use order", r)
			}
			removed++
		} else if e != nil {
			t.Fatal(e)
		} else {
			retained = true
		}
	}
	if removed < 2 || !retained {
		t.Fatal("aggregate target did not evict a prefix", removed, summary)
	}

}
func TestMaintenanceProtectsConsumers(t *testing.T) {
	for _, mode := range []string{"pending", "overlapping", "legacy", "corrupt", "reuse", "recent", "offline"} {
		t.Run(mode, func(t *testing.T) {
			o, m, r := maintenanceFixture(t)
			switch mode {
			case "pending", "overlapping":
				u, e := m.AcquireUse(context.Background(), r)
				if e != nil {
					t.Fatal(e)
				}
				if mode == "overlapping" {
					second, e := m.AcquireUse(context.Background(), r)
					if e != nil {
						t.Fatal(e)
					}
					if e = second.Finish(context.Background()); e != nil {
						t.Fatal(e)
					}
				}
				_ = u
			case "legacy":
				data, _ := json.Marshal(r)
				h := sha256.Sum256(data)
				if e := os.Remove(filepath.Join(o.UseRoot, hex.EncodeToString(h[:])+".json")); e != nil {
					t.Fatal(e)
				}
			case "corrupt":
				data, _ := json.Marshal(r)
				h := sha256.Sum256(data)
				put(t, filepath.Join(o.UseRoot, hex.EncodeToString(h[:])+".json"), "invalid")
			case "reuse":
				o.Retention = retentionFixture(func(_ context.Context, _ RepositoryScope, v func(RetentionEvidence) error) error {
					u, e := m.AcquireUse(context.Background(), r)
					if e != nil {
						return e
					}
					if e = u.Finish(context.Background()); e != nil {
						return e
					}
					return v(RetentionEvidence{Complete: true, Stable: true})
				})
			case "recent":
				o.Now = time.Now().UTC()
			case "offline":
				o.Retention = retentionFixture(func(context.Context, RepositoryScope, func(RetentionEvidence) error) error {
					return fmt.Errorf("offline")
				})
			}
			summary, _, e := Maintain(context.Background(), o)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = os.Stat(r.Location); e != nil {
				t.Fatal("protected consumer removed", e)
			}
			if summary.RemovedNamespaceBytes != 0 || summary.ExcessBytes <= 0 {
				t.Fatal(summary)
			}
		})
	}
}
func TestMaintenanceThrottleCounters(t *testing.T) {
	o, _, _ := maintenanceFixture(t)
	o.Policy.TargetBytes = 1 << 40
	dirs, git, remote, free := 0, 0, 0, 0
	o.ReadDirectory = func(ctx context.Context, path string, visit func(os.DirEntry) error) error {
		dirs++
		es, e := os.ReadDir(path)
		if os.IsNotExist(e) {
			return nil
		}
		if e != nil {
			return e
		}
		for _, entry := range es {
			if e = visit(entry); e != nil {
				return e
			}
		}
		return nil
	}
	o.GitOutput = func(ctx context.Context, root string, args ...string) (io.ReadCloser, error) {
		git++
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
		data, e := cmd.Output()
		return io.NopCloser(bytes.NewReader(data)), e
	}
	o.Retention = retentionFixture(func(_ context.Context, _ RepositoryScope, v func(RetentionEvidence) error) error {
		remote++
		return v(RetentionEvidence{Complete: true, Stable: true})
	})
	o.FreeSpace = func() { free++ }
	if _, _, e := Maintain(context.Background(), o); e != nil {
		t.Fatal(e)
	}
	baseline := [3]int{dirs, git, remote}
	if dirs == 0 || remote != 1 {
		t.Fatal(baseline)
	}
	for i := 0; i < 10; i++ {
		if _, warn, e := Maintain(context.Background(), o); e != nil || warn {
			t.Fatal(warn, e)
		}
	}
	if baseline != [3]int{dirs, git, remote} || free != 1 {
		t.Fatal("throttled call did expensive work", dirs, git, remote, free)
	}
	o.Now = o.Now.Add(10 * time.Minute)
	s, w, e := Maintain(context.Background(), o)
	if e != nil || !w || !s.Stale || free != 2 || baseline != [3]int{dirs, git, remote} {
		t.Fatal(s, w, e, free)
	}
	o.Now = o.Now.Add(20 * time.Minute)
	if _, _, e = Maintain(context.Background(), o); e != nil {
		t.Fatal(e)
	}
	if remote != 2 {
		t.Fatal(remote)
	}
}
func TestMaintenanceConcurrentAdmissionAndFailure(t *testing.T) {
	o, _, _ := maintenanceFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	o.Retention = retentionFixture(func(_ context.Context, _ RepositoryScope, v func(RetentionEvidence) error) error {
		close(entered)
		<-release
		return v(RetentionEvidence{})
	})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, _, _ = Maintain(context.Background(), o) }()
	<-entered
	if _, w, e := Maintain(context.Background(), o); e != nil || w {
		t.Fatal(w, e)
	}
	close(release)
	wg.Wait()
	o.Retention = retentionFixture(func(context.Context, RepositoryScope, func(RetentionEvidence) error) error {
		t.Error("failure was not throttled")
		return nil
	})
	if _, w, e := Maintain(context.Background(), o); e != nil || w {
		t.Fatal(w, e)
	}
	// A read-only diagnostic never inventories or removes cache data even when due.
	o.Now = o.Now.Add(time.Hour)
	o.ReadOnly = true
	if _, _, e := Maintain(context.Background(), o); e != nil {
		t.Fatal(e)
	}
}
func TestMaintenanceNeverRetiresTasks(t *testing.T) {
	o, _, _ := maintenanceFixture(t)
	repo, _, _ := DiscoverScopes(context.Background(), o.Options, true)
	task := filepath.Join(repo[0].Root, ".swarm", "worktrees", "issue-1", "data")
	put(t, task, "keep")
	state := filepath.Join(repo[0].Root, ".swarm", "coordination", "issue-1.json")
	put(t, state, "unchanged")
	if _, _, e := Maintain(context.Background(), o); e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{task, state} {
		raw, e := os.ReadFile(p)
		if e != nil || len(raw) == 0 {
			t.Fatal(p, e)
		}
	}
}

func TestMaintenanceRealShapeScaleAndEvidenceBudget(t *testing.T) {
	o, m, first := maintenanceFixture(t)
	resources := []storageuse.Resource{first}
	// 47 repositories and 940 independent producer leaves, plus the initial one.
	for n := 0; n < 47; n++ {
		root := filepath.Join(filepath.Dir(o.CacheRoot), fmt.Sprintf("scale-repo-%d", n))
		if data, e := exec.Command("git", "init", "-q", root).CombinedOutput(); e != nil {
			t.Fatal(string(data), e)
		}
		if _, e := registry.New(o.RegistryPath).Add(fmt.Sprintf("scale/repo%d", n), root, "local"); e != nil {
			t.Fatal(e)
		}
		h := sha256.Sum256([]byte(filepath.Join(root, ".git")))
		key := hex.EncodeToString(h[:16])
		for _, class := range []string{"agent-build", "validate-build", "agent-deps", "validate-deps"} {
			for _, eco := range []string{"go", "rust", "c-cpp", "node", "python"} {
				r := storageuse.ResourceFor(key, class, filepath.Join(o.CacheRoot, class, key, eco))
				u, e := m.AcquireUse(context.Background(), r)
				if e != nil {
					t.Fatal(e)
				}
				put(t, filepath.Join(r.Location, "data"), "cache")
				if e = u.Finish(context.Background()); e != nil {
					t.Fatal(e)
				}
				resources = append(resources, r)
			}
		}
	}
	// The large first leaf exhausts the pass usage allowance. Later ownership/use
	// observations must still be admitted, rather than sharing that exhausted pool.
	for n := 0; n < 54262; n++ {
		put(t, filepath.Join(first.Location, fmt.Sprintf("entry-%05d", n)), "x")
	}
	// Unrelated control entries are not enumerated for cache eligibility.
	for n := 0; n < 7000; n++ {
		put(t, filepath.Join(o.UseRoot, fmt.Sprintf("unrelated-%05d.json", n)), "unrelated")
	}
	o.allocation = func(_ os.FileInfo, blocks int64) int64 { return blocks * 16384 }
	o.Policy.TargetBytes = 1 << 60
	dirs, remotes := 0, 0
	o.ReadDirectory = func(ctx context.Context, path string, v func(os.DirEntry) error) error {
		dirs++
		es, e := os.ReadDir(path)
		if os.IsNotExist(e) {
			return nil
		}
		if e != nil {
			return e
		}
		for _, entry := range es {
			if e = v(entry); e != nil {
				return e
			}
		}
		return nil
	}
	o.Retention = retentionFixture(func(_ context.Context, _ RepositoryScope, v func(RetentionEvidence) error) error {
		remotes++
		return v(RetentionEvidence{Complete: true, Stable: true})
	})
	summary, _, e := Maintain(context.Background(), o)
	if e != nil {
		t.Fatal(e)
	}
	if summary.ObservedBytes < 45_000_000_000 || summary.DiscoveryCounters.Records != len(resources) || summary.UsageCounters.Entries > DefaultLimits().Entries || summary.UsageCounters.Bytes > DefaultLimits().Bytes || remotes != 48 {
		t.Fatal(summary, len(resources), remotes)
	}
	beforeDirs := dirs
	if _, _, e = Maintain(context.Background(), o); e != nil {
		t.Fatal(e)
	}
	if dirs != beforeDirs || remotes != 48 {
		t.Fatal("throttled inventory repeated")
	}
	// Verify all later use evidence was observed even after accounting hit a limit.
	s := reclaimScanner(context.Background(), ReclaimOptions{CacheRoot: o.CacheRoot, ControlRoot: o.UseRoot, Limits: DefaultLimits()})
	s.usageBudget = &Budget{Limits: Limits{256, 10000, 1, 100000, 4096}}
	for _, scope := range mustScopes(t, o) {
		s.repos[scope.CommonDirectory] = scope.Root
	}
	s.caches()
	if len(s.out.Resources) != len(resources) {
		t.Fatal("usage starved discovery", len(s.out.Resources))
	}
	for _, r := range s.out.Resources {
		if r.Active == nil || *r.Active || r.LastUse == nil {
			t.Fatal("usage starved use evidence", r)
		}
	}
}
func mustScopes(t *testing.T, o MaintenanceOptions) []SelectedRepository {
	t.Helper()
	sc, _, e := DiscoverScopes(context.Background(), o.Options, true)
	if e != nil {
		t.Fatal(e)
	}
	return sc
}

func TestMaintenanceHardlinksAndUnsafeEntries(t *testing.T) {
	for _, mode := range []string{"hardlink", "symlink", "fifo"} {
		t.Run(mode, func(t *testing.T) {
			o, _, r := maintenanceFixture(t)
			switch mode {
			case "hardlink":
				if e := os.Link(filepath.Join(r.Location, "data"), filepath.Join(filepath.Dir(o.CacheRoot), "keep")); e != nil {
					t.Skip(e)
				}
			case "symlink":
				if e := os.Symlink(t.TempDir(), filepath.Join(r.Location, "link")); e != nil {
					t.Skip(e)
				}
			case "fifo":
				if e := exec.Command("mkfifo", filepath.Join(r.Location, "fifo")).Run(); e != nil {
					t.Skip(e)
				}
			}
			summary, _, e := Maintain(context.Background(), o)
			if e != nil {
				t.Fatal(e)
			}
			if mode == "hardlink" {
				if summary.ExcessBytes <= 0 {
					t.Fatal("claimed shared blocks reclaimed", summary)
				}
			} else {
				if _, e = os.Stat(r.Location); e != nil {
					t.Fatal("unsafe leaf removed", e)
				}
			}
		})
	}
}

func TestMaintenanceFailureIsolation(t *testing.T) {
	for _, mode := range []string{"corrupt-throttle", "state-symlink", "cancel", "clock-rollback", "readonly", "overlap"} {
		t.Run(mode, func(t *testing.T) {
			o, _, r := maintenanceFixture(t)
			switch mode {
			case "corrupt-throttle":
				put(t, filepath.Join(o.StateRoot, "throttle.json"), "corrupt")
			case "state-symlink":
				if e := os.Symlink(t.TempDir(), o.StateRoot); e != nil {
					t.Skip(e)
				}
			case "clock-rollback":
				if _, _, e := Maintain(context.Background(), o); e != nil {
					t.Fatal(e)
				}
				o.Now = o.Now.Add(-time.Hour)
				o.Policy.TargetBytes = 1 << 40
			case "readonly":
				o.ReadOnly = true
			case "overlap":
				o.StateRoot = r.Location
			}
			ctx := context.Background()
			if mode == "cancel" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			_, _, e := Maintain(ctx, o)
			if mode == "state-symlink" || mode == "overlap" || mode == "cancel" {
				if e == nil {
					t.Fatal("failure not reported")
				}
			}
			if mode == "state-symlink" || mode == "overlap" || mode == "cancel" || mode == "readonly" {
				if _, e = os.Stat(r.Location); e != nil {
					t.Fatal("failure changed data", e)
				}
			}
			if mode == "clock-rollback" {
				if _, warn, e := Maintain(context.Background(), o); e != nil || warn {
					t.Fatal("rollback not throttled", warn, e)
				}
			}
		})
	}
}

func TestMaintenanceIdentitySwapAndBelowTarget(t *testing.T) {
	for _, mode := range []string{"swap", "below-target", "tie"} {
		t.Run(mode, func(t *testing.T) {
			o, m, r := maintenanceFixture(t)
			switch mode {
			case "swap":
				o.Retention = retentionFixture(func(_ context.Context, _ RepositoryScope, v func(RetentionEvidence) error) error {
					if e := os.Rename(r.Location, r.Location+"-old"); e != nil {
						return e
					}
					put(t, filepath.Join(r.Location, "data"), "replacement")
					return v(RetentionEvidence{Complete: true, Stable: true})
				})
			case "below-target":
				o.Policy.TargetBytes = 1 << 40
			case "tie":
				second := storageuse.ResourceFor(r.Repository, "validate-build", filepath.Join(o.CacheRoot, "validate-build", r.Repository, "go"))
				u, e := m.AcquireUse(context.Background(), second)
				if e != nil {
					t.Fatal(e)
				}
				put(t, filepath.Join(second.Location, "data"), "second")
				if e = u.Finish(context.Background()); e != nil {
					t.Fatal(e)
				}
				for _, resource := range []storageuse.Resource{r, second} {
					setLastUse(t, o.UseRoot, resource, o.Now.Add(-30*24*time.Hour))
				}
				s := reclaimScanner(context.Background(), ReclaimOptions{Limits: DefaultLimits()})
				observed := usage(s, second.Location)
				if observed.AllocatedBytes == nil {
					t.Skip("allocated bytes unavailable")
				}
				o.Policy.TargetBytes = *observed.AllocatedBytes
			}
			summary, _, e := Maintain(context.Background(), o)
			if e != nil {
				t.Fatal(e)
			}
			if mode == "tie" {
				if _, e = os.Stat(r.Location); !os.IsNotExist(e) {
					t.Fatal("identity tie-break did not remove agent first", e)
				}
				if summary.RemovedNamespaceBytes == 0 {
					t.Fatal(summary)
				}
			} else if _, e = os.Stat(r.Location); e != nil {
				t.Fatal("protected identity removed", e)
			}
		})
	}
}

func TestMaintenancePartialLeafDoesNotPoisonLaterAccounting(t *testing.T) {
	o, _, r := maintenanceFixture(t)
	unsafe := filepath.Join(o.CacheRoot, "unsafe")
	put(t, filepath.Join(unsafe, "data"), "data")
	if e := os.Symlink(t.TempDir(), filepath.Join(unsafe, "link")); e != nil {
		t.Skip(e)
	}
	s := reclaimScanner(context.Background(), ReclaimOptions{Limits: DefaultLimits()})
	first := usage(s, unsafe)
	if first.AllocatedBytes != nil {
		t.Fatal("unsafe accounting complete")
	}
	second := usage(s, r.Location)
	if second.AllocatedBytes == nil || *second.AllocatedBytes <= 0 || s.physical {
		t.Fatal("partial aggregate poisoned independent leaf", second, s.physical)
	}
}

func TestMaintenanceCacheBudgetExcludesScratch(t *testing.T) {
	o, _, r := maintenanceFixture(t)
	scratch := filepath.Join(o.CacheRoot, "agent-deps", r.Repository, ".prefetch-rust-active")
	put(t, filepath.Join(scratch, "data"), "active prefetch")
	s := reclaimScanner(context.Background(), ReclaimOptions{Limits: DefaultLimits()})
	observed := usage(s, r.Location)
	if observed.AllocatedBytes == nil {
		t.Skip("allocated bytes unavailable")
	}
	o.Policy.TargetBytes = *observed.AllocatedBytes
	summary, _, e := Maintain(context.Background(), o)
	if e != nil {
		t.Fatal(e)
	}
	if summary.RemovedNamespaceBytes != 0 || summary.ExcessBytes != 0 || summary.ObservedBytes != *observed.AllocatedBytes {
		t.Fatal("scratch affected cache budget", summary)
	}
	if _, e = os.Stat(filepath.Join(scratch, "data")); e != nil {
		t.Fatal(e)
	}
}

func TestMaintenanceAggregateAccountingOverflow(t *testing.T) {
	_, _, r := maintenanceFixture(t)
	s := reclaimScanner(context.Background(), ReclaimOptions{Limits: DefaultLimits()})
	s.out.ObservedAllocatedBytes = math.MaxInt64
	_ = usage(s, r.Location)
	if s.out.ObservedAllocatedBytes != math.MaxInt64 || s.physical {
		t.Fatal("aggregate overflow wrapped or became complete", s.out, s.physical)
	}
}
