package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
)

// instructionLinkRepo commits files and links (values starting with "->") and
// provisions a task worktree from that commit.
func instructionLinkRepo(t *testing.T, issue int, entries map[string]string) (repo, wt, branch, base string) {
	t.Helper()
	repo = t.TempDir()
	runWorkspaceGit(t, repo, "init", "-b", "main")
	runWorkspaceGit(t, repo, "config", "user.name", "Test")
	runWorkspaceGit(t, repo, "config", "user.email", "test@example.test")
	runWorkspaceGit(t, repo, "config", "core.autocrlf", "false")
	for rel, value := range entries {
		path := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if target, ok := strings.CutPrefix(value, "->"); ok {
			if err := os.Symlink(filepath.FromSlash(target), path); err != nil {
				t.Skipf("symlink creation unavailable: %v", err)
			}
		} else if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runWorkspaceGit(t, repo, "add", "-A")
	runWorkspaceGit(t, repo, "commit", "-m", "base")
	base = workspaceGitOutput(t, repo, "rev-parse", "HEAD")
	wt, branch, err := ProvisionAt(repo, issue, "instruction links", base, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return repo, wt, branch, base
}

func TestInstructionSymlinkLifecycle(t *testing.T) {
	t.Parallel()
	guide := "# Team guide\n\nRun make check.\n"
	for _, tc := range []struct {
		name    string
		entries map[string]string
		target  string
	}{
		{"agents to claude", map[string]string{"CLAUDE.md": guide, "AGENTS.md": "->CLAUDE.md"}, "CLAUDE.md"},
		{"claude to agents", map[string]string{"AGENTS.md": guide, "CLAUDE.md": "->AGENTS.md"}, "AGENTS.md"},
		{"shared third file", map[string]string{"docs/AI.md": guide, "AGENTS.md": "->docs/AI.md", "CLAUDE.md": "->docs/AI.md"}, "docs/AI.md"},
		{"tracked empty target", map[string]string{"CLAUDE.md": "", "AGENTS.md": "->CLAUDE.md"}, "CLAUDE.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo, wt, branch, base := instructionLinkRepo(t, 61, tc.entries)
			ctx := context.Background()
			read := func(rel string) string {
				t.Helper()
				data, err := os.ReadFile(filepath.Join(wt, filepath.FromSlash(rel)))
				if err != nil {
					t.Fatal(err)
				}
				return string(data)
			}
			original := read(tc.target)
			baseTree := workspaceGitOutput(t, wt, "rev-parse", base+"^{tree}")
			block := ActiveMarkerStart + "\nGenerated pointer.\n" + ActiveMarkerEnd
			for range 2 {
				if err := injectActivePointer(wt, block); err != nil {
					t.Fatal(err)
				}
			}
			if got := read(tc.target); strings.Count(got, ActiveMarkerStart) != 1 || !strings.HasSuffix(got, original) {
				t.Fatalf("%s pointer blocks:\n%s", tc.target, got)
			}
			// Linked names only resolve to the target; no file appears beside them.
			for rel, value := range tc.entries {
				if want, ok := strings.CutPrefix(value, "->"); ok {
					if got, err := os.Readlink(filepath.Join(wt, rel)); err != nil || filepath.ToSlash(got) != want {
						t.Fatalf("link %s = %q, %v", rel, got, err)
					}
				}
			}
			if only, err := OnlyActivePointer(wt, tc.target); err != nil || !only {
				t.Fatalf("pointer-only target not recognised: %v %v", only, err)
			}
			if changed, err := HasCheckpointableChanges(ctx, wt); err != nil || changed {
				t.Fatalf("pointer reported as a change: %v %v", changed, err)
			}
			// A fresh claim of the uncheckpointed worktree accepts the pointer.
			if _, _, err := ProvisionAt(repo, 61, "instruction links", base, branch, ""); err != nil {
				t.Fatal(err)
			}
			tree, err := CheckpointTree(ctx, wt)
			if err != nil {
				t.Fatal(err)
			}
			if tree != baseTree {
				t.Fatalf("pointer-only checkpoint tree %s, want %s", tree, baseTree)
			}

			// A real edit is kept; the pointer and link blobs are not changed.
			if err := os.WriteFile(filepath.Join(wt, filepath.FromSlash(tc.target)), []byte(read(tc.target)+"Team edit.\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			tree, err = CheckpointTree(ctx, wt)
			if err != nil {
				t.Fatal(err)
			}
			if got := workspaceGitOutput(t, wt, "cat-file", "blob", tree+":"+tc.target); strings.Contains(got, ActiveMarkerStart) || !strings.HasSuffix(got, "Team edit.") {
				t.Fatalf("private index target:\n%s", got)
			}
			wu := &contract.WorkUnit{IssueNumber: 61, Title: "Edit linked guide"}
			head, err := Checkpoint(ctx, wt, branch, CheckpointOptions{WorkUnit: wu, Purpose: "submit"})
			if err != nil {
				t.Fatal(err)
			}
			if got := workspaceGitOutput(t, wt, "rev-parse", head+"^{tree}"); got != tree {
				t.Fatalf("checkpoint tree %s differs from private index tree %s", got, tree)
			}
			for rel, value := range tc.entries {
				if strings.HasPrefix(value, "->") {
					if got, want := workspaceGitOutput(t, wt, "ls-tree", head, "--", rel), workspaceGitOutput(t, wt, "ls-tree", base, "--", rel); got != want || !strings.HasPrefix(got, "120000 ") {
						t.Fatalf("link %s entry %q, want %q", rel, got, want)
					}
				}
			}
			if got := read(tc.target); got != original+"Team edit.\n" {
				t.Fatalf("cleanup left %q", got)
			}
		})
	}
}

func TestInstructionSymlinkRetargetCannotEscapeWorktree(t *testing.T) {
	t.Parallel()
	outside := filepath.Join(t.TempDir(), "AI.md")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, wt, _, _ := instructionLinkRepo(t, 62, map[string]string{"AGENTS.md": "guide\n", "CLAUDE.md": "->AGENTS.md"})
	block := ActiveMarkerStart + "\nGenerated pointer.\n" + ActiveMarkerEnd
	if err := injectActivePointer(wt, block); err != nil {
		t.Fatal(err)
	}
	injected, err := os.ReadFile(filepath.Join(wt, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	// A contributor retargets the second name after the pointer was injected.
	for _, target := range []string{outside, "../../" + filepath.Base(filepath.Dir(outside)) + "/AI.md", ".git", "missing.md"} {
		if err := os.Remove(filepath.Join(wt, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(wt, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		if err := injectActivePointer(wt, block); err == nil {
			t.Fatalf("injection accepted link to %s", target)
		}
		if err := CleanEphemeralHarnessMarkers(wt); err == nil {
			t.Fatalf("cleanup accepted link to %s", target)
		}
		if _, err := CheckpointTree(context.Background(), wt); err == nil {
			t.Fatalf("checkpoint tree accepted link to %s", target)
		}
		if only, _ := OnlyActivePointer(wt, "CLAUDE.md"); only {
			t.Fatalf("retargeted link to %s treated as a pointer", target)
		}
		if got, err := os.ReadFile(outside); err != nil || string(got) != "outside" {
			t.Fatalf("outside file changed: %q, %v", got, err)
		}
		if got, err := os.ReadFile(filepath.Join(wt, "AGENTS.md")); err != nil || string(got) != string(injected) {
			t.Fatalf("AGENTS.md changed before refusal: %q, %v", got, err)
		}
	}
}

func TestPRWorktreeInstructionSymlinkPair(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	runWorkspaceGit(t, repo, "init", "-b", "main")
	runWorkspaceGit(t, repo, "config", "user.name", "Test")
	runWorkspaceGit(t, repo, "config", "user.email", "test@example.test")
	runWorkspaceGit(t, repo, "config", "core.autocrlf", "false")
	if err := os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte("Team guide.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("CLAUDE.md", filepath.Join(repo, "AGENTS.md")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: repo}); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, repo, "add", ".")
	runWorkspaceGit(t, repo, "commit", "-m", "base")
	base := workspaceGitOutput(t, repo, "rev-parse", "HEAD")
	runWorkspaceGit(t, repo, "branch", "feature")
	opts := PRWorkspaceOptions{Number: 32, HeadBranch: "feature", HeadSHA: base, BaseSHA: base}
	path, _, err := PreparePR(context.Background(), repo, opts)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := os.ReadFile(filepath.Join(path, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	wf, err := harness.LoadWorkflow(repo, "feature")
	if err != nil {
		t.Fatal(err)
	}
	wu := &contract.WorkUnit{ID: "ISSUE-8", IssueNumber: 8, Title: "Repair", Objective: "Fix PR"}
	for _, role := range []string{"planner", "developer"} {
		if err := CompilePRHarnessBundleAt(path, wu, wf, nil, role, base, 32); err != nil {
			t.Fatal(err)
		}
	}
	text, err := os.ReadFile(filepath.Join(path, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(text), ActiveMarkerStart) != 1 || !strings.Contains(string(text), "swarm pr work 32") {
		t.Fatalf("PR pointer blocks in linked CLAUDE.md:\n%s", text)
	}
	// Reopening the PR worktree accepts the pointer; cleanup restores the bytes.
	opts.AllowHarnessMarkers = true
	if _, _, err := PreparePR(context.Background(), repo, opts); err != nil {
		t.Fatal(err)
	}
	if err := CleanEphemeralHarnessMarkers(path); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(path, "CLAUDE.md")); err != nil || string(got) != string(committed) {
		t.Fatalf("cleanup left %q, %v", got, err)
	}
	if status := workspaceGitOutput(t, path, "status", "--porcelain", "--", "AGENTS.md", "CLAUDE.md"); status != "" {
		t.Fatalf("instruction pair changed: %q", status)
	}
}

func TestRecoveryIndexAcceptsOnlyPointerOnLinkedTarget(t *testing.T) {
	t.Parallel()
	_, wt, _, base := instructionLinkRepo(t, 63, map[string]string{"docs/AI.md": "Guide.\n", "AGENTS.md": "->docs/AI.md"})
	block := ActiveMarkerStart + "\nGenerated pointer.\n" + ActiveMarkerEnd
	if err := injectActivePointer(wt, block); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, wt, "add", "docs/AI.md")
	if err := verifyRecoveryIndex(wt, base); err != nil {
		t.Fatalf("staged pointer on linked target refused: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, "docs", "AI.md"), []byte(block+"\n\nGuide.\nUser edit.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, wt, "add", "docs/AI.md")
	if err := verifyRecoveryIndex(wt, base); err == nil {
		t.Fatal("staged user edit on linked target accepted")
	}
}
