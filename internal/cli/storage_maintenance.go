package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/node"
	"github.com/supernovae/swarmos/internal/registry"
	"github.com/supernovae/swarmos/internal/storage"
	"github.com/supernovae/swarmos/internal/storageuse"
	"github.com/supernovae/swarmos/internal/tracker"
)

var maintenanceCacheDir = os.UserCacheDir

// installStorageMaintenance wraps outer command returns, after workflow-owned
// use scopes finish even on failure. It does not run inside individual roles.
func installStorageMaintenance(cmd *cobra.Command, o *RootOptions) {
	for _, child := range cmd.Commands() {
		installStorageMaintenance(child, o)
	}
	if cmd.RunE == nil || cmd.Parent() == nil {
		return
	}
	parent, name := cmd.Parent().Name(), cmd.Name()
	readOnly := parent == "node" && name == "status"
	enabled := readOnly || parent == "node" && name == "run" || (parent == "issue" || parent == "pr") && (name == "work" || name == "stage" || name == "validate") || parent == "repo" && name == "validate"
	if !enabled {
		return
	}
	run := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		o.storageMaintenance(cmd, readOnly)
		if !readOnly {
			defer o.storageMaintenance(cmd, false)
		}
		return run(cmd, args)
	}
}
func (o *RootOptions) storageMaintenance(cmd *cobra.Command, readOnly bool) {
	if o.Fork {
		return
	}
	if o.maintenanceHook != nil {
		o.maintenanceHook(cmd, readOnly)
		return
	}
	cfg, err := node.LoadStorageSettings(o.nodeConfigPath())
	if err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "Storage maintenance disabled: invalid operator storage settings:", err)
		return
	}
	cache, err := maintenanceCacheDir()
	if err != nil {
		return
	}
	control, err := storageuse.ControlRoot()
	if err != nil {
		return
	}
	options := storage.Options{RegistryPath: o.registryPath(), CacheRoot: filepath.Join(cache, "swarm"), UseRoot: control, SelectedRoot: o.RepoRoot, Remote: o.effectiveUpstreamRemote()}
	if options.SelectedRoot != "" {
		options.SelectedRoot, err = filepath.Abs(options.SelectedRoot)
		if err != nil {
			return
		}
	}
	retention := storage.BackendRetention{Apply: true, Remote: o.effectiveUpstreamRemote(), Tracker: func(sc storage.RepositoryScope) (tracker.PullRequestVisitor, error) {
		if sc.Backend == "local" {
			return tracker.NewLocalTracker(sc.Root), nil
		}
		slug := registry.DetectGitHubSlugFrom(sc.Root, o.effectiveUpstreamRemote())
		var tr tracker.Tracker
		var err error
		if o.trackerOverride != nil {
			tr = o.trackerOverride
		} else if o.githubTrackerFunc != nil {
			tr, err = o.githubTrackerFunc(slug)
		} else {
			tr, err = tracker.NewGitHubTracker(slug)
		}
		if err != nil {
			return nil, err
		}
		v, ok := tr.(tracker.PullRequestVisitor)
		if !ok {
			return nil, fmt.Errorf("bounded tracker unavailable")
		}
		return v, nil
	}}
	runStorageMaintenance(cmd, storage.MaintenanceOptions{Options: options, StateRoot: filepath.Join(cache, "swarm-maintenance"), Now: o.now(), ReadOnly: readOnly, Policy: storage.MaintenancePolicy{TargetBytes: cfg.CacheTargetBytes, IdleAge: cfg.InactiveRetention, Interval: cfg.MaintenanceInterval, WarningInterval: cfg.WarningInterval}, Retention: retention, FreeSpace: func() {
		// A statfs per managed root also handles roots on different filesystems.
		for _, path := range []string{cache, control, os.TempDir()} {
			free, total, e := storage.FilesystemSpace(path)
			if os.IsNotExist(e) {
				continue
			}
			if e != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "Storage: free space unavailable")
				continue
			}
			renderMaintenanceSpace(cmd, path, free, total, cfg, storageCleanupCommand(cmd, o.registryPath()))
		}
	}})
}

func runStorageMaintenance(cmd *cobra.Command, options storage.MaintenanceOptions) {
	summary, warn, err := storage.Maintain(cmd.Context(), options)
	if err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "Storage: the cache check did not finish; it runs again after the maintenance interval:", err)
	}
	if !warn {
		return
	}
	target, cleanup := options.Policy.TargetBytes, storageCleanupCommand(cmd, options.RegistryPath)
	if !options.ReadOnly {
		renderMaintenanceDiagnostics(cmd, summary, target, cleanup)
		return
	}
	if summary.InventoryAt.IsZero() {
		fmt.Fprintln(cmd.ErrOrStderr(), "Storage: caches have not been measured yet; the next swarm work, stage, validate or node run measures them.")
		return
	}
	age := options.Now.Sub(summary.InventoryAt)
	if age < 0 {
		age = 0
	}
	fmt.Fprintln(cmd.ErrOrStderr(), maintenanceNotice(summary, target, cleanup, fmt.Sprintf(" (measured %s ago)", age.Round(time.Second))))
}

// renderMaintenanceDiagnostics speaks up during work only when the operator
// can act: caches are over the target or finished tasks are waiting.
func renderMaintenanceDiagnostics(cmd *cobra.Command, s storage.MaintenanceSummary, target int64, cleanup string) {
	if s.ExcessBytes > 0 || s.FinishedTasks > 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), maintenanceNotice(s, target, cleanup, ""))
	}
}

// storageCleanupCommand names a preview that works on this host: --all-repos
// needs a registered repository, and the default covers the current checkout.
func storageCleanupCommand(cmd *cobra.Command, registryPath string) string {
	if registryPath != "" {
		// ReadSnapshot never creates the registry or its lock file.
		entries, err := registry.New(registryPath).ReadSnapshot(cmd.Context(), 4096, func(int64) error { return nil })
		if err != nil || len(entries) > 0 {
			return "swarm storage cleanup --all-repos"
		}
	}
	return "swarm storage cleanup"
}

func maintenanceNotice(s storage.MaintenanceSummary, target int64, cleanup, measured string) string {
	var b strings.Builder
	if s.ExcessBytes > 0 {
		fmt.Fprintf(&b, "Storage: Swarm caches use %s, %s over the %s target%s.", humanBytes(s.ObservedBytes), humanBytes(s.ExcessBytes), humanBytes(target), measured)
	} else {
		fmt.Fprintf(&b, "Storage: Swarm caches use %s of the %s target%s.", humanBytes(s.ObservedBytes), humanBytes(target), measured)
	}
	if s.FinishedTasks == 1 {
		b.WriteString(" 1 finished task can be cleaned up.")
	} else if s.FinishedTasks > 1 {
		fmt.Fprintf(&b, " %d finished tasks can be cleaned up.", s.FinishedTasks)
	}
	if !s.Complete {
		b.WriteString(" Some storage could not be measured, so usage may be higher.")
	}
	if s.ExcessBytes > 0 || s.FinishedTasks > 0 {
		fmt.Fprintf(&b, "\n  See what can be removed: %s", cleanup)
	}
	return b.String()
}

// humanBytes renders binary units, matching du -h and the 40 GiB default target.
func humanBytes(n int64) string {
	const kib, mib, gib = 1 << 10, 1 << 20, 1 << 30
	switch v := float64(n); {
	case v >= gib:
		return fmt.Sprintf("%.1f GiB", v/gib)
	case v >= mib:
		return fmt.Sprintf("%.1f MiB", v/mib)
	case v >= kib:
		return fmt.Sprintf("%.1f KiB", v/kib)
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

func renderMaintenanceSpace(cmd *cobra.Command, path string, free, total uint64, cfg node.StorageSettings, cleanup string) {
	threshold := float64(cfg.LowFreeBytes)
	if fraction := float64(total) * cfg.LowFreePercent / 100; fraction > threshold {
		threshold = fraction
	}
	if float64(free) < threshold {
		fmt.Fprintf(cmd.ErrOrStderr(), "Storage: low free space at %q (%s free of %s).\n  See what can be removed: %s\n", path, humanBytes(int64(free)), humanBytes(int64(total)), cleanup)
	}
}
