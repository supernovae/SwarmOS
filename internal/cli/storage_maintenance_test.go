package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/node"
	"github.com/supernovae/swarmos/internal/registry"
	"github.com/supernovae/swarmos/internal/storage"
)

func TestStorageMaintenanceDiagnostics(t *testing.T) {
	const target = 40 << 30
	for _, tc := range []struct {
		name    string
		summary storage.MaintenanceSummary
		want    []string
		quiet   bool
	}{
		// Within the target with nothing to retire there is nothing to act on,
		// even when part of the inventory could not be measured.
		{name: "within target", summary: storage.MaintenanceSummary{ObservedBytes: 11 << 30, ProtectedBytes: 11 << 30, Stale: true}, quiet: true},
		{name: "over target", summary: storage.MaintenanceSummary{ObservedBytes: 45 << 30, ExcessBytes: 5 << 30, Complete: true}, want: []string{"Swarm caches use 45.0 GiB, 5.0 GiB over the 40.0 GiB target.", "\n  See what can be removed: swarm storage cleanup\n"}},
		{name: "finished tasks", summary: storage.MaintenanceSummary{ObservedBytes: 3 << 20, FinishedTasks: 2}, want: []string{"Swarm caches use 3.0 MiB of the 40.0 GiB target.", "2 finished tasks can be cleaned up.", "Some storage could not be measured", "swarm storage cleanup\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			var out, errOut bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			renderMaintenanceDiagnostics(cmd, tc.summary, target, "swarm storage cleanup")
			if out.Len() != 0 {
				t.Fatal("diagnostics changed stdout")
			}
			if tc.quiet != (errOut.Len() == 0) {
				t.Fatal(errOut.String())
			}
			for _, want := range tc.want {
				if !strings.Contains(errOut.String(), want) {
					t.Fatal(want, errOut.String())
				}
			}
			// One runnable command per line: never a "preview; apply" pair, raw
			// byte counts, or key=value fields.
			for _, banned := range []string{"; apply:", "--apply", "=", "bytes"} {
				if strings.Contains(errOut.String(), banned) {
					t.Fatal(banned, errOut.String())
				}
			}
		})
	}
}

// The suggested preview must run on this host: --all-repos fails when no
// repository is registered, and looking must not create the registry.
func TestStorageCleanupCommandMatchesRegistry(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	dir := t.TempDir()
	missing := filepath.Join(dir, "none", "repos.yaml")
	if got := storageCleanupCommand(cmd, missing); got != "swarm storage cleanup" {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Dir(missing)); !os.IsNotExist(err) {
		t.Fatal("created registry directory", err)
	}
	if got := storageCleanupCommand(cmd, ""); got != "swarm storage cleanup" {
		t.Fatal(got)
	}
	f := newCLIFixture(t)
	path := filepath.Join(dir, "repos.yaml")
	if _, err := registry.New(path).Add("local/fixture", f.root, "local"); err != nil {
		t.Fatal(err)
	}
	if got := storageCleanupCommand(cmd, path); got != "swarm storage cleanup --all-repos" {
		t.Fatal(got)
	}
}
func TestStorageMaintenanceLifecycleAndFailureIsolation(t *testing.T) {
	for _, parent := range []string{"issue", "pr", "repo", "node"} {
		for _, name := range []string{"work", "stage", "validate", "status", "run", "cleanup"} {
			t.Run(parent+"/"+name, func(t *testing.T) {
				root := &cobra.Command{Use: "swarm"}
				group := &cobra.Command{Use: parent}
				root.AddCommand(group)
				wantErr := errors.New("original workflow result")
				finished := false
				calls := 0
				cmd := &cobra.Command{Use: name, RunE: func(*cobra.Command, []string) error { defer func() { finished = true }(); return wantErr }}
				group.AddCommand(cmd)
				opts := &RootOptions{maintenanceHook: func(_ *cobra.Command, ro bool) {
					calls++
					if calls == 2 && !finished {
						t.Error("after hook before use completion")
					}
					if parent == "node" && name == "status" && !ro {
						t.Error("status requested mutation")
					}
				}}
				installStorageMaintenance(root, opts)
				if e := cmd.RunE(cmd, nil); e != wantErr {
					t.Fatal(e)
				}
				expected := 0
				if (parent == "issue" || parent == "pr") && (name == "work" || name == "stage" || name == "validate") || parent == "repo" && name == "validate" || parent == "node" && name == "run" {
					expected = 2
				}
				if parent == "node" && name == "status" {
					expected = 1
				}
				if calls != expected {
					t.Fatal(calls, expected)
				}
				opts.Fork = true
				calls = 0
				_ = cmd.RunE(cmd, nil)
				if calls != 0 {
					t.Fatal("fork maintenance enabled")
				}
			})
		}
	}
}

func TestStorageMaintenanceLowSpaceThresholds(t *testing.T) {
	cfg := node.DefaultStorageSettings()
	for _, tc := range []struct {
		free, total uint64
		low         bool
	}{{5 << 30, 10 << 30, false}, {(5 << 30) - 1, 10 << 30, true}, {10 << 30, 100 << 30, false}, {(10 << 30) - 1, 100 << 30, true}} {
		cmd := &cobra.Command{}
		var out, errOut bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&errOut)
		renderMaintenanceSpace(cmd, "fixture-filesystem", tc.free, tc.total, cfg, "swarm storage cleanup")
		if (errOut.Len() > 0) != tc.low || out.Len() != 0 {
			t.Fatal(tc, errOut.String())
		}
	}
}

// These counters sit on the actual inventory and retention adapter boundaries.
type statusRetentionCounter struct{ calls *int }

func (r statusRetentionCounter) WithRetention(context.Context, storage.RepositoryScope, func(storage.RetentionEvidence) error) error {
	*r.calls++
	return errors.New("status must not query retention")
}

func TestStorageMaintenanceRepeatedStatus(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprint(cached), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "maintenance")
			now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
			var before []byte
			if cached {
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				before, _ = json.Marshal(struct {
					Attempt, Warning time.Time
					Summary          storage.MaintenanceSummary
				}{now.Add(-2 * time.Minute), now, storage.MaintenanceSummary{
					InventoryAt: now.Add(-2 * time.Minute), ObservedBytes: 60_700_000_000,
					ExcessBytes: 17_800_000_000, Complete: true,
				}})
				if err := os.WriteFile(filepath.Join(root, "throttle.json"), before, 0600); err != nil {
					t.Fatal(err)
				}
			}
			walks, remotes, git := 0, 0, 0
			options := storage.MaintenanceOptions{
				StateRoot: root, ReadOnly: true, Now: now,
				Policy:    storage.MaintenancePolicy{TargetBytes: 40 << 30, IdleAge: time.Hour, Interval: 30 * time.Minute, WarningInterval: 10 * time.Minute},
				Retention: statusRetentionCounter{&remotes},
				Options: storage.Options{
					ReadDirectory: func(context.Context, string, func(os.DirEntry) error) error {
						walks++
						return errors.New("unexpected walk")
					},
					GitOutput: func(context.Context, string, ...string) (io.ReadCloser, error) {
						git++
						return nil, errors.New("unexpected git")
					},
				},
			}
			for i := 0; i < 2; i++ {
				cmd := &cobra.Command{}
				cmd.SetContext(context.Background())
				var stdout, stderr bytes.Buffer
				cmd.SetOut(&stdout)
				cmd.SetErr(&stderr)
				runStorageMaintenance(cmd, options)
				if stdout.Len() != 0 {
					t.Fatal("changed stdout", stdout.String())
				}
				if cached {
					for _, want := range []string{"Swarm caches use 56.5 GiB, 16.6 GiB over the 40.0 GiB target (measured 2m0s ago).", "See what can be removed: swarm storage cleanup"} {
						if !strings.Contains(stderr.String(), want) {
							t.Fatal(want, stderr.String())
						}
					}
				} else if !strings.Contains(stderr.String(), "caches have not been measured yet; the next swarm work, stage, validate or node run measures them") || strings.Contains(stderr.String(), "caches use") {
					t.Fatal(stderr.String())
				}
			}
			if walks != 0 || remotes != 0 || git != 0 {
				t.Fatal(walks, remotes, git)
			}
			after, err := os.ReadFile(filepath.Join(root, "throttle.json"))
			if cached {
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("status changed throttle", err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatal("status created throttle", err)
			}
		})
	}
}
