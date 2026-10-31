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
	"github.com/supernovae/swarmos/internal/storage"
)

func TestStorageMaintenanceDiagnostics(t *testing.T) {
	cmd := &cobra.Command{}
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	renderMaintenanceDiagnostics(cmd, storage.MaintenanceSummary{ObservedBytes: 100, ExcessBytes: 80, ProtectedBytes: 100, FinishedTasks: 2, Stale: true})
	if out.Len() != 0 {
		t.Fatal("diagnostics changed stdout")
	}
	for _, want := range []string{"excess=80", "protected/retained=100", "finished task candidates=2", "incomplete=true", "stale=true", "swarm storage cleanup --all-repos", "swarm storage cleanup --all-repos --apply"} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatal(want, errOut.String())
		}
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
		renderMaintenanceSpace(cmd, "fixture-filesystem", tc.free, tc.total, cfg)
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
					for _, want := range []string{"age=2m0s", "60700000000 (60.7 GB)", "17800000000 (17.8 GB)", "stale=true"} {
						if !strings.Contains(stderr.String(), want) {
							t.Fatal(want, stderr.String())
						}
					}
				} else if !strings.Contains(stderr.String(), "no storage inventory exists yet; one will run at the next eligible work, stage, validate or node run") || strings.Contains(stderr.String(), "observed cache bytes=0") {
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
