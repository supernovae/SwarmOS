package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// This probe records why automated removal stays disabled. Inspection alone
// cannot prevent ignored artifacts appearing before Git recursively removes
// the worktree, even without --force.
func TestCleanupArtifactNonForcedGitRemovalProbe(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = root
		if out, e := c.CombinedOutput(); e != nil {
			t.Fatalf("git %v: %s %v", args, out, e)
		}
	}
	git("init")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.invalid")
	if e := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored\n"), 0600); e != nil {
		t.Fatal(e)
	}
	git("add", ".")
	git("commit", "-m", "fixture")
	path := filepath.Join(root, "task")
	git("worktree", "add", "-b", "task", path)
	// Deterministically insert an ignored file after the clean inspection.
	git("-C", path, "status", "--porcelain")
	if e := os.WriteFile(filepath.Join(path, "ignored"), []byte("artifact"), 0600); e != nil {
		t.Fatal(e)
	}
	git("worktree", "remove", path)
	if _, e := os.Stat(filepath.Join(path, "ignored")); !os.IsNotExist(e) {
		t.Fatalf("Git behavior changed; review removal safety: %v", e)
	}
}
