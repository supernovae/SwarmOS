package contract

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExternalWorkUnitRejectsSymlinkEscape(t *testing.T) {
	for _, parent := range []bool{false, true} {
		root, outside := t.TempDir(), t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "unit.yaml"), []byte("objective: Outside contract\nworkflow: feature\nacceptance:\n  - Preserve boundary\n"), 0600); err != nil {
			t.Fatal(err)
		}
		target, name := filepath.Join(outside, "unit.yaml"), "link"
		if parent {
			target = outside
			name = "link/unit.yaml"
		}
		if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
			t.Skip(err)
		}
		if _, err := ParseWorkUnitFromIssueWithRoot(1, "title", "swarm-workunit-path: "+name, root); err == nil {
			t.Fatal("external symlink accepted")
		}
	}
}
