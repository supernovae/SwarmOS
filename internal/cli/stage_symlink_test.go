package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/workspace"
)

// Staging publishes PLAN.md and TEST_PLAN.md as artifacts. A report that is a
// symlink must be refused instead of publishing the file it points at.
func TestIssueStagePlanRefusesSymlinkedReports(t *testing.T) {
	f := newCLIFixture(t)
	issue := f.create(t, "human")
	n := strconv.Itoa(issue)
	f.must(t, "issue", "approve", n)
	f.must(t, "issue", "claim", n, "--agent", "mock")
	active := filepath.Join(workspace.WorktreePath(f.root, issue), ".swarm", "active")
	secret := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(secret, []byte("OUTSIDE-WORKTREE-CONTENT\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"PLAN.md", "TEST_PLAN.md"} {
		t.Run(name, func(t *testing.T) {
			for _, report := range []string{"PLAN.md", "TEST_PLAN.md"} {
				path := filepath.Join(active, report)
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				if report == name {
					if err := os.Symlink(secret, path); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(path, []byte("# Report\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			out, err := f.run("issue", "stage", n, "--phase", "plan")
			if err == nil || !strings.Contains(err.Error(), "missing or empty .swarm/active/"+name) {
				t.Fatalf("symlinked %s was staged: %v\n%s", name, err, out)
			}
			status, _ := f.run("issue", "status", n, "--json")
			if strings.Contains(out+status, "OUTSIDE-WORKTREE-CONTENT") {
				t.Fatal("content from outside the worktree was published")
			}
		})
	}
}
