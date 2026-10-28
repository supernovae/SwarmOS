package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalCheckpointEvidence(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"clean", "pointer", "pointer-real", "tracked", "untracked", "ignored", "recovery"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			wt, _, base := checkpointFixture(t)
			switch kind {
			case "pointer", "pointer-real":
				content := "User instructions\n"
				if kind == "pointer-real" {
					content += "Real instruction\n"
				}
				checkpointWrite(t, wt, "AGENTS.md", ActiveMarkerStart+"\nTask pointer\n"+ActiveMarkerEnd+"\n\n"+content)
			case "tracked":
				checkpointWrite(t, wt, "staged.txt", "changed")
			case "untracked":
				checkpointWrite(t, wt, "new.txt", "new")
			case "ignored":
				runWorkspaceGit(t, wt, "config", "core.excludesFile", filepath.Join(t.TempDir(), "ignore"))
				// Use the repository's local exclude, keeping candidate files unchanged.
				path := workspaceGitOutput(t, wt, "rev-parse", "--git-path", "info/exclude")
				if !filepath.IsAbs(path) {
					path = filepath.Join(wt, path)
				}
				if err := os.WriteFile(path, []byte("ignored.txt\n"), 0644); err != nil {
					t.Fatal(err)
				}
				checkpointWrite(t, wt, "ignored.txt", "ignored")
			case "recovery":
				dir := filepath.Join(wt, ".swarm/active-output")
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				checkpointWrite(t, dir, "timeout.patch", "patch")
			}
			before := workspaceGitOutput(t, wt, "status", "--porcelain")
			dirty, err := HasCheckpointableChanges(context.Background(), wt)
			if err != nil {
				t.Fatal(err)
			}
			want := kind == "tracked" || kind == "untracked" || kind == "pointer-real"
			if dirty != want {
				t.Fatalf("checkpointable=%t want %t", dirty, want)
			}
			if after := workspaceGitOutput(t, wt, "status", "--porcelain"); before != after {
				t.Fatal("observation mutated worktree")
			}
			count, err := CommitsBeyond(context.Background(), wt, base)
			if err != nil || count != 0 {
				t.Fatalf("count=%d err=%v", count, err)
			}
			if _, err := CommitsBeyond(context.Background(), wt, "0000000000000000000000000000000000000000"); err == nil {
				t.Fatal("missing base accepted")
			}
		})
	}
}
