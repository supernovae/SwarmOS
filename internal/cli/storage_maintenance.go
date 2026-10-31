package cli

import (
	"fmt"
	"os"
	"path/filepath"

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
	options := storage.Options{RegistryPath: o.registryPath(), CacheRoot: filepath.Join(cache, "swarm"), UseRoot: control, SelectedRoot: o.RepoRoot, Backend: o.Backend, Remote: o.effectiveUpstreamRemote()}
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
	summary, warn, err := storage.Maintain(cmd.Context(), storage.MaintenanceOptions{Options: options, StateRoot: filepath.Join(cache, "swarm-maintenance"), Now: o.now(), ReadOnly: readOnly, Policy: storage.MaintenancePolicy{TargetBytes: cfg.CacheTargetBytes, IdleAge: cfg.InactiveRetention, Interval: cfg.MaintenanceInterval, WarningInterval: cfg.WarningInterval}, Retention: retention, FreeSpace: func() {
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
			renderMaintenanceSpace(cmd, path, free, total, cfg)

		}
	}})
	if err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "Storage maintenance incomplete; retry after the maintenance interval:", err)
	}
	if warn {
		renderMaintenanceDiagnostics(cmd, summary)
	}
}
func renderMaintenanceDiagnostics(cmd *cobra.Command, s storage.MaintenanceSummary) {
	if s.ExcessBytes > 0 || !s.Complete || s.FinishedTasks > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "Storage: observed cache bytes=%d; excess=%d; protected/retained=%d; finished task candidates=%d; incomplete=%t; stale=%t. Preview: swarm storage cleanup --all-repos; apply: swarm storage cleanup --all-repos --apply\n", s.ObservedBytes, s.ExcessBytes, s.ProtectedBytes, s.FinishedTasks, !s.Complete, s.Stale)
	}
}

func renderMaintenanceSpace(cmd *cobra.Command, path string, free, total uint64, cfg node.StorageSettings) {
	threshold := float64(cfg.LowFreeBytes)
	if fraction := float64(total) * cfg.LowFreePercent / 100; fraction > threshold {
		threshold = fraction
	}
	if float64(free) < threshold {
		fmt.Fprintf(cmd.ErrOrStderr(), "Storage: low free space at %q (%d bytes available). Preview: swarm storage cleanup --all-repos; apply: swarm storage cleanup --all-repos --apply\n", path, free)
	}
}
