package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

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
