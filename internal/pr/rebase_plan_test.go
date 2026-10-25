package pr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRebasePlanningOutputIsBoundedAndReadOnly(t *testing.T) {
	for _, kind := range []string{"valid", "omitted path", "missing side", "source edit", "context edit", "source deletion", "symlink", "oversize"} {
		t.Run(kind, func(t *testing.T) {
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
			put("file.txt", "conflicted\n")
			put(".swarm/active/PLAN.md", "approved behavior")
			before, err := snapshotPlanTree(dir)
			if err != nil {
				t.Fatal(err)
			}
			body := "## file.txt\nOurs: upstream\nTheirs: PR\nResolution: combine\nApproved behavior: retained\n"
			switch kind {
			case "omitted path":
				body = strings.ReplaceAll(body, "file.txt", "another.txt")
			case "missing side":
				body = strings.ReplaceAll(body, "Theirs: PR", "Theirs:")
			case "source edit":
				put("file.txt", "edited")
			case "context edit":
				put(".swarm/active/PLAN.md", "changed")
			case "source deletion":
				if err := os.Remove(filepath.Join(dir, "file.txt")); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink("file.txt", filepath.Join(dir, "link")); err != nil {
					t.Skip(err)
				}
			case "oversize":
				body = strings.Repeat("x", (64<<10)+1)
			}
			put(".swarm/active/REBASE_PLAN.md", body)
			_, err = verifyRebasePlan(dir, before, []string{"file.txt"})
			if (err == nil) != (kind == "valid") {
				t.Fatalf("%s: %v", kind, err)
			}
		})
	}
}
