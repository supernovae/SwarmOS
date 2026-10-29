package pr

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
)

func TestRepairRejectsEntireOutputOutsideFixedScope(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"valid", "outside edit", "outside addition", "outside deletion", "outside mode", "control", "protected", "symlink", "noop"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			put := func(p, body string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, p), []byte(body), 0644); err != nil {
					t.Fatal(err)
				}
			}
			put("allowed.txt", "original")
			put("other.txt", "original")
			put("protected.txt", "original")
			put(".swarm/active/PLAN.md", "original")
			before, err := snapshotPlanTree(dir)
			if err != nil {
				t.Fatal(err)
			}
			rv := &resolver{unit: &contract.WorkUnit{}, protected: []string{"protected.txt"}}
			if kind != "noop" {
				put("allowed.txt", "repair")
			}
			switch kind {
			case "outside edit":
				put("other.txt", "changed")
			case "outside addition":
				put("extra.txt", "changed")
			case "outside deletion":
				if err := os.Remove(filepath.Join(dir, "other.txt")); err != nil {
					t.Fatal(err)
				}
			case "outside mode":
				if err := os.Chmod(filepath.Join(dir, "other.txt"), 0755); err != nil {
					t.Fatal(err)
				}
			case "control":
				put(".swarm/active/PLAN.md", "changed")
			case "protected":
				put("protected.txt", "changed")
			case "symlink":
				if err := os.Symlink("allowed.txt", filepath.Join(dir, "link")); err != nil {
					t.Skip(err)
				}
			}
			after, err := verifyRepairTree(dir, before, []string{"allowed.txt", "protected.txt", ".swarm/active/PLAN.md"}, rv)
			if (err == nil) != (kind == "valid") {
				t.Fatalf("%s: %v", kind, err)
			}
			if err != nil && after != nil {
				t.Fatal("rejected partial output returned for import")
			}
		})
	}
}

func TestRepairProtectedPathsNeverReceiveHarnessException(t *testing.T) {
	for _, allowHarness := range []bool{false, true} {
		rv := &resolver{unit: &contract.WorkUnit{AllowHarnessChanges: allowHarness}, protected: []string{"protected/**"}}
		for _, p := range []string{"protected/x.go", ".swarm/active/PLAN.md", "AGENTS.md", "CLAUDE.md", ".git/config"} {
			if repairAllowed(p, []string{p}, rv) {
				t.Fatalf("harness=%t path %s accepted", allowHarness, p)
			}
		}
		dir := t.TempDir()
		before := map[string]planFile{"allowed.go": {data: []byte("before"), mode: 0644}, "protected/x.go": {data: []byte("before"), mode: 0644}}
		after := map[string]planFile{"allowed.go": {data: []byte("after"), mode: 0644}, "protected/x.go": {data: []byte("after"), mode: 0644}}
		if importRepair(dir, before, after, []string{"allowed.go", "protected/x.go"}, rv) == nil {
			t.Fatal("import accepted protected edit")
		}
		if _, err := os.Stat(filepath.Join(dir, "allowed.go")); !os.IsNotExist(err) {
			t.Fatal("partial import")
		}
	}
}
