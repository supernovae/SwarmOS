package workspace

import (
	"context"
	"os"
	"os/exec"
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

func TestValidationInstructionsLifecycle(t *testing.T) {
	for _, scenario := range []string{"exact", "edit", "unsafe replacement", "empty", "no pointer"} {
		t.Run(scenario, func(t *testing.T) {
			original := "Repository instructions\n"
			if scenario == "empty" {
				original = ""
			}
			_, wt, _, _ := instructionLinkRepo(t, 396, map[string]string{"AGENTS.md": "->guide.md", "CLAUDE.md": "->guide.md", "guide.md": original})
			path := filepath.Join(wt, "guide.md")
			if err := os.Chmod(path, 0755); err != nil {
				t.Fatal(err)
			}
			pointer := "<!-- swarm:active:start -->\nTask\n<!-- swarm:active:end -->\n\n"
			want := original
			if scenario != "no pointer" {
				want = pointer + original
				if err := os.WriteFile(path, []byte(want), 0755); err != nil {
					t.Fatal(err)
				}
			}
			restore, err := PrepareValidationInstructions(wt)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(path)
			if string(data) != original {
				t.Fatalf("cleaned %q", data)
			}
			if scenario == "edit" {
				if err := os.WriteFile(path, []byte("Check edit\n"+original), 0755); err != nil {
					t.Fatal(err)
				}
				want = pointer + "Check edit\n" + original
			}
			if scenario == "unsafe replacement" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("other.md", path); err != nil {
					t.Fatal(err)
				}
				if err := restore(); err == nil {
					t.Fatal("unsafe restore accepted")
				}
				return
			}
			if err := restore(); err != nil {
				t.Fatal(err)
			}
			data, _ = os.ReadFile(path)
			if string(data) != want {
				t.Fatalf("restored %q want %q", data, want)
			}
			mode, err := os.Stat(path)
			if err != nil || mode.Mode().Perm() != 0755 {
				t.Fatalf("mode %v %v", mode, err)
			}
			for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
				target, err := os.Readlink(filepath.Join(wt, name))
				if err != nil || target != "guide.md" {
					t.Fatalf("link %s %s %v", name, target, err)
				}
			}
		})
	}
}

func TestValidationInstructionsPartialSetup(t *testing.T) {
	_, wt, _, _ := instructionLinkRepo(t, 396, map[string]string{"AGENTS.md": "guide\n"})
	pointer := "<!-- swarm:active:start -->\nTask\n<!-- swarm:active:end -->\n\n"
	want := pointer + "guide\n"
	if err := os.WriteFile(filepath.Join(wt, "AGENTS.md"), []byte(want), 0644); err != nil {
		t.Fatal(err)
	}
	// Resolution succeeds, but reading the second target exceeds the instruction cap.
	if err := os.WriteFile(filepath.Join(wt, "CLAUDE.md"), []byte(strings.Repeat("x", 17<<20)), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareValidationInstructions(wt); err == nil {
		t.Fatal("oversized target accepted")
	}
	data, err := os.ReadFile(filepath.Join(wt, "AGENTS.md"))
	if err != nil || string(data) != want {
		t.Fatalf("partial cleanup not restored: %q %v", data, err)
	}
}

func TestValidationInstructionsTrackedEmpty(t *testing.T) {
	_, wt, _, _ := instructionLinkRepo(t, 396, map[string]string{"AGENTS.md": ""})
	pointer := "<!-- swarm:active:start -->\nTask\n<!-- swarm:active:end -->\n\n"
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		if err := os.WriteFile(filepath.Join(wt, name), []byte(pointer), 0644); err != nil {
			t.Fatal(err)
		}
	}
	restore, err := PrepareValidationInstructions(wt)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(wt, "AGENTS.md")); err != nil || len(data) != 0 {
		t.Fatalf("tracked empty removed %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(wt, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Fatalf("untracked pointer retained: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, "CLAUDE.md"), []byte("Check text\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(wt, "CLAUDE.md")); err != nil || string(data) != pointer+"Check text\n" {
		t.Fatalf("check edit lost %q %v", data, err)
	}
}

func TestValidationInstructionsGitFailure(t *testing.T) {
	_, wt, _, _ := instructionLinkRepo(t, 396, map[string]string{"AGENTS.md": ""})
	pointer := "<!-- swarm:active:start -->\nTask\n<!-- swarm:active:end -->\n\n"
	if err := os.WriteFile(filepath.Join(wt, "AGENTS.md"), []byte(pointer), 0644); err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nif test \"$1\" = ls-files; then exit 71; fi\nexec '" + realGit + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := PrepareValidationInstructions(wt); err == nil {
		t.Fatal("Git failure treated as untracked")
	}
	data, err := os.ReadFile(filepath.Join(wt, "AGENTS.md"))
	if err != nil || string(data) != pointer {
		t.Fatalf("Git failure lost tracked file: %q %v", data, err)
	}
}
