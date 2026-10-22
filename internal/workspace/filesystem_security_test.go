package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
)

func TestActiveBundleRejectsSymlinkDirectory(t *testing.T) {
	for _, name := range []string{".swarm", ".swarm/active"} {
		t.Run(name, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, name)); err != nil {
				t.Skip(err)
			}
			err := compileHarnessBundle(root, &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1}, &contract.WorkflowSpec{Name: "feature"}, nil, "planner", "prompt", "", "", "", "policy")
			if err == nil {
				t.Fatal("symlink active directory accepted")
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatalf("outside writes: %v %v", entries, err)
			}
		})
	}
}

func TestReacquireRejectsUnpinnedWorktreeWithoutDiscardingChanges(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "dirty", true: "unpublished-commit"}[committed], func(t *testing.T) {
			root := t.TempDir()
			runWorkspaceGit(t, root, "init", "-b", "main")
			runWorkspaceGit(t, root, "config", "user.name", "Test")
			runWorkspaceGit(t, root, "config", "user.email", "test@example.test")
			runWorkspaceGit(t, root, "commit", "--allow-empty", "-m", "base")
			base := workspaceGitOutput(t, root, "rev-parse", "HEAD")
			wt, branch, err := ProvisionAt(root, 91, "Reacquire", base, "", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(wt, "candidate.txt"), []byte("preserve me"), 0600); err != nil {
				t.Fatal(err)
			}
			if committed {
				if _, err := Checkpoint(context.Background(), wt, branch); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := ProvisionAt(root, 91, "Reacquire", base, branch, ""); err == nil {
				t.Fatal("reacquired unpinned worktree")
			}
			b, err := os.ReadFile(filepath.Join(wt, "candidate.txt"))
			if err != nil || string(b) != "preserve me" {
				t.Fatal("existing work was discarded")
			}
		})
	}
}

func TestPinnedResumePreservesUncommittedWork(t *testing.T) {
	root := t.TempDir()
	runWorkspaceGit(t, root, "init", "-b", "main")
	runWorkspaceGit(t, root, "config", "user.name", "Test")
	runWorkspaceGit(t, root, "config", "user.email", "test@example.test")
	runWorkspaceGit(t, root, "commit", "--allow-empty", "-m", "base")
	base := workspaceGitOutput(t, root, "rev-parse", "HEAD")
	wt, branch, err := ProvisionAt(root, 92, "Resume", base, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "candidate.txt"), []byte("preserve me"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ProvisionAt(root, 92, "Resume", base, branch, base); err != nil {
		t.Fatal(err)
	}
}

func TestWorktreeExcludeRejectsSymlink(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	runWorkspaceGit(t, root, "init", "-b", "main")
	canary := filepath.Join(outside, "exclude")
	if err := os.WriteFile(canary, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, ".git", "info", "exclude")
	if err := os.Remove(destination); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(canary, destination); err != nil {
		t.Skip(err)
	}
	if err := configureWorktreeExclude(root); err == nil {
		t.Fatal("symlink exclude accepted")
	}
	b, err := os.ReadFile(canary)
	if err != nil || string(b) != "outside" {
		t.Fatal("outside exclude modified")
	}
}

func TestUncheckpointedResumeAllowsOnlyGeneratedInstructionPointers(t *testing.T) {
	root := t.TempDir()
	runWorkspaceGit(t, root, "init", "-b", "main")
	runWorkspaceGit(t, root, "config", "user.name", "Test")
	runWorkspaceGit(t, root, "config", "user.email", "test@example.test")
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("Original instructions.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, root, "add", ".")
	runWorkspaceGit(t, root, "commit", "-m", "base")
	base := workspaceGitOutput(t, root, "rev-parse", "HEAD")
	wt, branch, err := ProvisionAt(root, 93, "Resume pointers", base, "", "")
	if err != nil {
		t.Fatal(err)
	}
	block := ActiveMarkerStart + "\nGenerated pointer.\n" + ActiveMarkerEnd
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		if err := injectManagedBlock(filepath.Join(wt, name), block); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := ProvisionAt(root, 93, "Resume pointers", base, branch, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "AGENTS.md"), []byte(block+"\n\nAltered user instructions.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ProvisionAt(root, 93, "Resume pointers", base, branch, ""); err == nil {
		t.Fatal("uncheckpointed user instruction changes accepted")
	}
}
