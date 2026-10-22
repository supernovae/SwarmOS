package tracker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalTrackerRejectsSymlinkStateAndLock(t *testing.T) {
	for _, name := range []string{"state.json", "state.json.lock"} {
		t.Run(name, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			dir := filepath.Join(root, ".swarm", "local-tracker")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			canary := filepath.Join(outside, "canary")
			if err := os.WriteFile(canary, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(canary, filepath.Join(dir, name)); err != nil {
				t.Skip(err)
			}
			if _, err := NewLocalTracker(root).CreateIssue(context.Background(), "title", "body", nil); err == nil {
				t.Fatal("symlink accepted")
			}
			b, err := os.ReadFile(canary)
			if err != nil || string(b) != "original" {
				t.Fatalf("canary modified: %q %v", b, err)
			}
		})
	}
}

func TestLocalTrackerReportsUnreadableState(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".swarm", "local-tracker", "state.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocalTracker(root).CreateIssue(context.Background(), "title", "body", nil); err == nil {
		t.Fatal("invalid state silently replaced")
	}
}
