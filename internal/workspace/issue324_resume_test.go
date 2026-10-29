package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIssue324UnpublishedProvision(t *testing.T) {
	for _, scenario := range []string{"base equality", "recorded implementation", "tracked", "staged", "untracked", "instructions", "detached", "wrong branch", "wrong head", "missing worktree", "symlink", "unrelated base", "operation"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAdvanceFixture(t)
			base, head := f.previous, f.previous
			switch scenario {
			case "recorded implementation":
				advanceWrite(t, f.path, "feature", "implemented")
				f.git(t, f.path, "add", "feature")
				f.git(t, f.path, "commit", "-m", "implementation")
				head = f.git(t, f.path, "rev-parse", "HEAD")
			case "tracked", "staged":
				advanceWrite(t, f.path, "source", "preserve")
				if scenario == "staged" {
					f.git(t, f.path, "add", "source")
				}
			case "untracked":
				advanceWrite(t, f.path, "untracked", "preserve")
			case "instructions":
				advanceWrite(t, f.path, "AGENTS.md", "real instruction edit")
			case "detached":
				f.git(t, f.path, "checkout", "--detach")
			case "wrong branch":
				f.git(t, f.path, "checkout", "-b", "other")
			case "wrong head":
				head = f.base
			case "missing worktree":
				f.git(t, f.root, "worktree", "remove", f.path)
			case "symlink":
				f.git(t, f.root, "worktree", "remove", f.path)
				if err := os.Symlink(t.TempDir(), f.path); err != nil {
					t.Fatal(err)
				}
			case "unrelated base":
				base = f.base
			case "operation":
				marker := f.git(t, f.path, "rev-parse", "--path-format=absolute", "--git-path", "MERGE_HEAD")
				if err := os.WriteFile(marker, []byte(head), 0600); err != nil {
					t.Fatal(err)
				}
			}
			refs := f.git(t, f.root, "show-ref")
			got, err := ValidateUnpublishedCheckpoint(f.root, 7, f.branch, base, head)
			positive := scenario == "base equality" || scenario == "recorded implementation"
			if positive && (err != nil || got != f.path) {
				t.Fatalf("positive: %s %v", got, err)
			}
			if !positive && err == nil {
				t.Fatal("unsafe local checkpoint admitted")
			}
			if after := f.git(t, f.root, "show-ref"); after != refs {
				t.Fatal("inspection changed refs")
			}
			if scenario == "untracked" {
				if b, err := os.ReadFile(filepath.Join(f.path, "untracked")); err != nil || string(b) != "preserve" {
					t.Fatal("source lost")
				}
			}
		})
	}
}
