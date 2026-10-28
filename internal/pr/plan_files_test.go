package pr

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
)

func TestRestoreApprovedPlan(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"fresh", "matching", "edited", "symlink file", "symlink directory"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			active := filepath.Join(root, ".swarm", "active")
			if err := os.MkdirAll(active, 0700); err != nil {
				t.Fatal(err)
			}
			st := &lease.IssueState{Artifacts: []contract.Artifact{
				{Type: contract.ArtifactDesign, Body: "# Approved plan\n"},
				{Type: contract.ArtifactTestPlan, Body: "# Approved tests\n"},
			}}
			outside := t.TempDir()
			victim := filepath.Join(outside, "PLAN.md")
			if err := os.WriteFile(victim, []byte("keep outside\n"), 0600); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "matching", "edited":
				body := st.Artifacts[0].Body
				if mode == "edited" {
					body = "local edits\n"
				}
				if err := os.WriteFile(filepath.Join(active, "PLAN.md"), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink file":
				if err := os.Symlink(victim, filepath.Join(active, "PLAN.md")); err != nil {
					t.Fatal(err)
				}
			case "symlink directory":
				if err := os.Remove(active); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, active); err != nil {
					t.Fatal(err)
				}
			}
			err := restoreApprovedPlan(root, st)
			wantError := mode == "edited" || mode == "symlink file" || mode == "symlink directory"
			if (err != nil) != wantError {
				t.Fatalf("restore error = %v, want error %t", err, wantError)
			}
			if mode == "edited" {
				body, err := os.ReadFile(filepath.Join(active, "PLAN.md"))
				if err != nil || string(body) != "local edits\n" {
					t.Fatalf("local plan overwritten: %q %v", body, err)
				}
			}
			if !wantError {
				for i, name := range []string{"PLAN.md", "TEST_PLAN.md"} {
					body, err := os.ReadFile(filepath.Join(active, name))
					if err != nil || string(body) != st.Artifacts[i].Body {
						t.Fatalf("restored %s = %q, %v", name, body, err)
					}
				}
				if err := restoreApprovedPlan(root, st); err != nil {
					t.Fatalf("retry: %v", err)
				}
			}
			body, err := os.ReadFile(victim)
			if err != nil || string(body) != "keep outside\n" {
				t.Fatalf("outside plan overwritten: %q %v", body, err)
			}
		})
	}
}
