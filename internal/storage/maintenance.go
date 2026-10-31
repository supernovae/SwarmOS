package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/supernovae/swarmos/internal/filelock"
	"github.com/supernovae/swarmos/internal/safefs"
	"github.com/supernovae/swarmos/internal/storageuse"
)

// MaintenancePolicy is supplied by the operator, never the candidate checkout.
type MaintenancePolicy struct {
	TargetBytes                        int64
	IdleAge, Interval, WarningInterval time.Duration
}
type MaintenanceSummary struct {
	DiscoveryCounters, UsageCounters Counters
	ObservedBytes                    int64
	RemovedNamespaceBytes            int64
	ExcessBytes                      int64
	ProtectedBytes                   int64
	FinishedTasks                    int
	Complete                         bool
	InventoryAt                      time.Time
	Stale                            bool
}
type maintenanceState struct {
	Attempt, Warning time.Time
	Summary          MaintenanceSummary
}
type MaintenanceOptions struct {
	Options
	Policy    MaintenancePolicy
	StateRoot string // private coordinator root, outside granted caches
	Now       time.Time
	ReadOnly  bool
	Retention RetentionSource
	// FreeSpace is called only when a warning is due; it must not walk trees.
	FreeSpace func()
}

func due(now, prior time.Time, interval time.Duration) bool {
	// A backwards clock permits one newly reserved attempt, then resumes throttling.
	return prior.IsZero() || now.Before(prior) || now.Sub(prior) >= interval
}

// Maintain reserves a durable throttle before any repository or cache discovery.
// The lock is nonblocking and is held through deletion and summary publication.
func Maintain(ctx context.Context, o MaintenanceOptions) (MaintenanceSummary, bool, error) {
	if o.Now.IsZero() {
		o.Now = time.Now().UTC()
	}
	if o.Policy.TargetBytes <= 0 || o.Policy.IdleAge <= 0 || o.Policy.Interval <= 0 || o.Policy.WarningInterval <= 0 {
		return MaintenanceSummary{}, false, errors.New("invalid maintenance policy")
	}
	if !filepath.IsAbs(o.StateRoot) || filepath.Clean(o.StateRoot) == string(filepath.Separator) {
		return MaintenanceSummary{}, false, errors.New("private maintenance root required")
	}
	for _, p := range []string{o.CacheRoot, o.UseRoot, o.SelectedRoot, o.ScratchRoot} {
		if p != "" && pathsOverlap(o.StateRoot, p) {
			return MaintenanceSummary{}, false, errors.New("maintenance root overlaps managed or repository storage")
		}
	}
	volume := filepath.VolumeName(o.StateRoot) + string(filepath.Separator)
	base, err := safefs.Open(volume)
	if err != nil {
		return MaintenanceSummary{}, false, err
	}
	defer base.Close()
	rel, err := filepath.Rel(volume, o.StateRoot)
	if err != nil {
		return MaintenanceSummary{}, false, err
	}
	root, err := base.Sub(rel, !o.ReadOnly, 0700)
	if o.ReadOnly && os.IsNotExist(err) {
		return MaintenanceSummary{}, true, nil
	}
	if err != nil {
		return MaintenanceSummary{}, false, err
	}
	defer root.Close()
	info, err := root.Lstat(".")
	if err != nil || info.Mode().Perm()&0077 != 0 {
		return MaintenanceSummary{}, false, errors.New("maintenance root is not private")
	}
	if !o.ReadOnly {
		lock, err := root.OpenLock("admission.lock")
		if err != nil {
			return MaintenanceSummary{}, false, err
		}
		defer lock.Close()
		if err = filelock.TryLockExclusive(lock); err != nil {
			return MaintenanceSummary{}, false, nil
		}
		defer filelock.Unlock(lock) // best effort admission
	}
	var state maintenanceState
	if data, e := root.ReadFile("throttle.json", 8192); e == nil {
		if json.Unmarshal(data, &state) != nil {
			state = maintenanceState{}
		}
	} else if !os.IsNotExist(e) {
		return state.Summary, false, e
	}
	if o.ReadOnly {
		state.Summary.Stale = state.Summary.InventoryAt.IsZero() || !state.Summary.InventoryAt.Equal(o.Now)
		if due(o.Now, state.Warning, o.Policy.WarningInterval) && o.FreeSpace != nil {
			o.FreeSpace()
		}
		return state.Summary, true, nil
	}
	warning := due(o.Now, state.Warning, o.Policy.WarningInterval)
	maintenance := !o.ReadOnly && due(o.Now, state.Attempt, o.Policy.Interval)
	if !warning && !maintenance {
		return state.Summary, false, nil
	}
	if maintenance {
		state.Attempt = o.Now
	}
	if warning {
		state.Warning = o.Now
	}
	save := func() error {
		data, e := json.Marshal(state)
		if e != nil {
			return e
		}
		return root.WriteFileDurable("throttle.json", data, 0600)
	}
	if err = save(); err != nil {
		return state.Summary, false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if maintenance {
		state.Summary, err = maintenancePass(ctx, o)
		state.Summary.InventoryAt = o.Now
	}
	state.Summary.Stale = state.Summary.InventoryAt.IsZero() || !state.Summary.InventoryAt.Equal(o.Now)
	if warning && o.FreeSpace != nil {
		o.FreeSpace()
	}
	return state.Summary, warning, errors.Join(err, save())
}
func pathsOverlap(a, b string) bool {
	// Existing aliases are rejected by root-bound opens; resolve prefixes here to
	// reject overlap before opening control storage as well.
	inside := func(a, b string) bool {
		r, e := filepath.Rel(filepath.Clean(a), filepath.Clean(b))
		return e == nil && (r == "." || filepath.IsLocal(r))
	}
	return inside(a, b) || inside(b, a)
}

type maintenanceUnit struct {
	resource   storageuse.Resource
	observed   Resource
	info       os.FileInfo
	scope      RepositoryScope
	commonInfo os.FileInfo
}

func automaticClass(class string) bool {
	return class == "agent-build" || class == "validate-build" || class == "agent-deps" || class == "validate-deps"
}

func maintenancePass(ctx context.Context, o MaintenanceOptions) (MaintenanceSummary, error) {
	out := MaintenanceSummary{Complete: true}
	// Scope discovery occurs once, after durable admission. Selected checkout is
	// included alongside registered repositories without filtering the registry.
	o.Options.includeSelected = true
	scopes, reasons, err := DiscoverScopes(ctx, o.Options, true)
	if err != nil || len(reasons) > 0 {
		out.Complete = false
	}
	limits := o.Limits
	if limits == (Limits{}) {
		limits = DefaultLimits()
	}
	s := reclaimScanner(ctx, ReclaimOptions{CacheRoot: o.CacheRoot, ControlRoot: o.UseRoot, Limits: limits})
	s.opts.cacheOnly = true
	s.opts.ReadDirectory = o.ReadDirectory
	s.opts.allocation = o.allocation
	// A pass-wide accounting allowance is independent of ownership/use evidence.
	// Exhausting a deep leaf cannot starve admission evidence for other leaves.
	s.usageBudget = &Budget{Limits: limits}
	s.recheckBudget = &Budget{Limits: limits}
	selected := map[string]RepositoryScope{}
	commons := map[string]os.FileInfo{}
	for _, sc := range scopes {
		h := sha256.Sum256([]byte(sc.CommonDirectory))
		key := hex.EncodeToString(h[:16])
		if _, ok := selected[key]; ok {
			continue
		}
		if pathsOverlap(o.StateRoot, sc.Root) || pathsOverlap(o.CacheRoot, sc.Root) || pathsOverlap(o.UseRoot, sc.Root) {
			out.Complete = false
			continue
		}
		root, e := openExisting(sc.CommonDirectory)
		if e != nil {
			out.Complete = false
			continue
		}
		info, e := root.Lstat(".")
		root.Close()
		if e != nil {
			out.Complete = false
			continue
		}
		selected[key] = sc.RepositoryScope
		commons[key] = info
		s.repos[sc.CommonDirectory] = sc.Root
	}
	// Only managed cache leaves are inventoried; task state, scratch and worktrees
	// are never passed to removal. Durable records are observed once per leaf.
	s.caches()
	out.DiscoveryCounters = s.b.Counters
	out.UsageCounters = s.usageBudget.Counters
	out.ObservedBytes = s.out.ObservedAllocatedBytes
	out.Complete = out.Complete && s.out.Complete && s.physical
	units := []maintenanceUnit{}
	ro := ReclaimOptions{CacheRoot: o.CacheRoot}
	for _, r := range s.out.Resources {
		if !automaticClass(r.Class) {
			continue
		}
		rel, e := filepath.Rel(o.CacheRoot, r.Path)
		if e != nil {
			continue
		}
		parts := splitPath(rel)
		if len(parts) != 3 {
			continue
		}
		key := parts[1]
		sc, ok := selected[key]
		use := storageuse.ResourceFor(key, r.Class, r.Path)
		if !ok || !reclaimLayout(ro, use, key) || r.Active == nil || *r.Active || r.LastUse == nil || r.LastUse.IsZero() || r.LastUse.After(o.Now) || o.Now.Sub(*r.LastUse) < o.Policy.IdleAge || r.AllocatedBytes == nil {
			continue
		}
		parent, e := openExisting(filepath.Dir(r.Path))
		if e != nil {
			continue
		}
		info, e := parent.Lstat(filepath.Base(r.Path))
		parent.Close()
		if e != nil {
			continue
		}
		units = append(units, maintenanceUnit{use, r, info, sc, commons[key]})
	}
	sort.Slice(units, func(i, j int) bool {
		a, b := units[i], units[j]
		if !a.observed.LastUse.Equal(*b.observed.LastUse) {
			return a.observed.LastUse.Before(*b.observed.LastUse)
		}
		return a.resource.Identity < b.resource.Identity
	})
	evidence := map[string]RetentionEvidence{}
	// Fetch repository evidence once, before acquiring individual resource locks.
	for _, sc := range selected {
		if o.Retention == nil {
			out.Complete = false
			continue
		}
		called := false
		e := o.Retention.WithRetention(ctx, sc, func(ev RetentionEvidence) error {
			if called {
				return errors.New("repeated retention callback")
			}
			called = true
			evidence[sc.CommonDirectory] = ev
			return nil
		})
		ev := evidence[sc.CommonDirectory]
		if e != nil || !ev.Complete || !ev.Stable {
			out.Complete = false
			delete(evidence, sc.CommonDirectory)
		} else {
			out.FinishedTasks += ev.FinishedTasks
		}
	}
	remaining := out.ObservedBytes
	var manager *storageuse.Manager
	defer func() {
		if manager != nil {
			manager.Close()
		}
	}()
	for _, u := range units {
		if remaining <= o.Policy.TargetBytes {
			break
		}
		if ctx.Err() != nil {
			out.Complete = false
			break
		}
		ev, ok := evidence[u.scope.CommonDirectory]
		if !ok || !ev.Complete || !ev.Stable {
			continue
		}
		if manager == nil {
			manager, err = storageuse.Open(o.UseRoot, o.CacheRoot)
			if err != nil {
				out.Complete = false
				break
			}
		}
		// A waiting producer wins after a short timeout; maintenance never waits for
		// a consumer while holding the host-wide lock for the entire pass deadline.
		lockCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		guard, e := manager.AcquireMaintenance(lockCtx, u.resource)
		cancel()
		if e != nil {
			continue
		}
		removed, bytes, e := removeMaintenanceUnit(ctx, s, ro, u, guard)
		guard.Close()
		if e != nil {
			out.Complete = false
		}
		if removed {
			if bytes > remaining {
				bytes = remaining
			}
			remaining -= bytes
			out.RemovedNamespaceBytes += bytes
			_, e = manager.PruneAbsent(ctx, u.resource)
			if e != nil {
				out.Complete = false
			}
		}
	}
	out.ProtectedBytes = remaining
	if remaining > o.Policy.TargetBytes {
		out.ExcessBytes = remaining - o.Policy.TargetBytes
	}
	return out, ctx.Err()
}
func splitPath(path string) []string {
	var out []string
	for path != "." && path != "" {
		dir, base := filepath.Split(path)
		if base == "" {
			path = filepath.Clean(dir)
			continue
		}
		out = append([]string{base}, out...)
		path = filepath.Clean(dir)
	}
	return out
}
func removeMaintenanceUnit(ctx context.Context, s *scanner, ro ReclaimOptions, u maintenanceUnit, g *storageuse.Maintenance) (bool, int64, error) {
	if !g.Observation().LastUse.Equal(*u.observed.LastUse) {
		return false, 0, nil
	}
	root, e := openExisting(u.scope.CommonDirectory)
	if e != nil {
		return false, 0, e
	}
	info, e := root.Lstat(".")
	root.Close()
	if e != nil || !os.SameFile(info, u.commonInfo) {
		return false, 0, e
	}
	fresh, e := canonicalScope(s, u.scope)
	if e != nil || fresh.CommonDirectory != u.scope.CommonDirectory || !reclaimLayout(ro, u.resource, u.resource.Repository) {
		return false, 0, e
	}
	parent, e := openExisting(filepath.Dir(u.resource.Location))
	if e != nil {
		return false, 0, e
	}
	defer parent.Close()
	info, e = parent.Lstat(filepath.Base(u.resource.Location))
	if e != nil || !os.SameFile(info, u.info) {
		return false, 0, e
	}
	// Recheck accounting under exclusion. An incomplete observation never permits
	// deletion, and retained units are never walked a second time.
	check := reclaimScanner(ctx, ReclaimOptions{Limits: s.b.Limits})
	check.opts.allocation = s.opts.allocation
	check.usageBudget = s.recheckBudget
	before := usage(check, u.resource.Location)
	if before.AllocatedBytes == nil || u.observed.AllocatedBytes == nil || *before.AllocatedBytes != *u.observed.AllocatedBytes {
		return false, 0, nil
	}
	e = parent.RemoveAllCheckedContext(ctx, filepath.Base(u.resource.Location), info, func() error { return ctx.Err() })
	_, after := parent.Lstat(filepath.Base(u.resource.Location))
	if e != nil || !os.IsNotExist(after) {
		return false, 0, errors.Join(e, after)
	}
	// Deduct only unique namespace bytes observed for this unit. Cross-leaf
	// hardlinks may survive elsewhere; physical savings are never asserted.
	return true, u.observed.uniqueBytes, nil
}
