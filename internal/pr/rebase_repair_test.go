package pr

import (
	"context"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"github.com/supernovae/swarmos/internal/workspace"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
)

func TestRepairRejectsEntireOutputOutsideFixedScope(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"valid", "outside edit", "outside addition", "outside deletion", "outside mode", "control", "protected", "protected harness", "symlink", "noop"} {
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
			case "protected harness":
				rv.unit.AllowHarnessChanges = true
				put("protected.txt", "changed")
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

func TestRepairReportedLocations(t *testing.T) {
	tracked := []string{"pkg/out.go", "pkg/out_test.go", "a/dup_test.go", "b/dup_test.go"}
	for _, tc := range []struct {
		output string
		want   []string
	}{
		{"pkg/out.go:3:4: compiler error", []string{"pkg/out.go"}},
		{"    out_test.go:8: failed", []string{"pkg/out_test.go"}},
		{"    dup_test.go:8: failed", nil},
		{"forged.go:1:1: fake", nil},
	} {
		if got := reportedRepairFiles(tc.output, tracked); !slices.Equal(got, tc.want) {
			t.Fatalf("%q: %v", tc.output, got)
		}
	}
}

func TestRepairExactFileSelection(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		c := gitfixture.Command(t, dir, args...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	git("init")
	for _, p := range []string{"ok.go", "comma,file.s", "sub/file.go"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte("data"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	ctx := context.Background()
	got, err := workspace.ExactRepairFiles(ctx, dir, []string{"ok.go", "ok.go", "comma,file.s"})
	if err != nil || len(got) != 2 {
		t.Fatalf("exact: %v %v", got, err)
	}
	for _, p := range []string{"../ok.go", "/ok.go", "./ok.go", "sub/../ok.go", "*.go", "sub", "missing.go", "C:/ok.go", "sub\\file.go"} {
		if _, err := workspace.ExactRepairFiles(ctx, dir, []string{p}); err == nil {
			t.Errorf("accepted %q", p)
		}
	}
	if err := os.Remove(filepath.Join(dir, "ok.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.ExactRepairFiles(ctx, dir, []string{"ok.go"}); err == nil {
		t.Fatal("accepted deletion")
	}
	if err := os.Symlink("comma,file.s", filepath.Join(dir, "ok.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.ExactRepairFiles(ctx, dir, []string{"ok.go"}); err == nil {
		t.Fatal("accepted symlink")
	}
	if err := os.Rename(filepath.Join(dir, "sub"), filepath.Join(dir, "real")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.ExactRepairFiles(ctx, dir, []string{"sub/file.go"}); err == nil {
		t.Fatal("accepted ancestor symlink")
	}
}
