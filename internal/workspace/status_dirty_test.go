package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestStatusChangesIgnorePointerOnly(t *testing.T) {
	t.Parallel()
	guide := "# Team guide\n\nRun make check.\n"
	layouts := []struct {
		name    string
		entries map[string]string
		targets []string // files that receive the pointer
		link    string   // a tracked instruction link, if any
	}{
		{"separate files", map[string]string{"AGENTS.md": guide, "CLAUDE.md": "# Claude\n", "main.go": "package main\n"}, []string{"AGENTS.md", "CLAUDE.md"}, ""},
		{"generated claude", map[string]string{"AGENTS.md": guide, "main.go": "package main\n"}, []string{"AGENTS.md", "CLAUDE.md"}, ""},
		{"agents to claude", map[string]string{"CLAUDE.md": guide, "AGENTS.md": "->CLAUDE.md", "main.go": "package main\n"}, []string{"CLAUDE.md"}, "AGENTS.md"},
		{"claude to agents", map[string]string{"AGENTS.md": guide, "CLAUDE.md": "->AGENTS.md", "main.go": "package main\n"}, []string{"AGENTS.md"}, "CLAUDE.md"},
		{"shared third file", map[string]string{"docs/AI.md": guide, "AGENTS.md": "->docs/AI.md", "CLAUDE.md": "->docs/AI.md", "main.go": "package main\n"}, []string{"docs/AI.md"}, "AGENTS.md"},
		{"tracked empty target", map[string]string{"CLAUDE.md": "", "AGENTS.md": "->CLAUDE.md", "main.go": "package main\n"}, []string{"CLAUDE.md"}, "AGENTS.md"},
	}
	type mutation struct {
		name  string
		dirty bool
		apply func(t *testing.T, wt, target, link string)
	}
	write := func(t *testing.T, wt, rel, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(wt, filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(t *testing.T, wt, rel string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(wt, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	mutations := []mutation{
		{"pointer only", false, func(*testing.T, string, string, string) {}},
		{"pointer staged", false, func(t *testing.T, wt, target, _ string) { runWorkspaceGit(t, wt, "add", "-A") }},
		{"edit after block", true, func(t *testing.T, wt, target, _ string) { write(t, wt, target, read(t, wt, target)+"Team edit.\n") }},
		{"edit before block", true, func(t *testing.T, wt, target, _ string) { write(t, wt, target, "Team edit.\n"+read(t, wt, target)) }},
		{"whitespace outside block", true, func(t *testing.T, wt, target, _ string) { write(t, wt, target, read(t, wt, target)+"\n") }},
		{"unclosed marker", true, func(t *testing.T, wt, target, _ string) {
			write(t, wt, target, read(t, wt, target)+ActiveMarkerStart+"\n")
		}},
		{"other tracked file", true, func(t *testing.T, wt, _, _ string) { write(t, wt, "main.go", "package other\n") }},
		{"untracked file", true, func(t *testing.T, wt, _, _ string) { write(t, wt, "notes with space.txt", "x") }},
		{"marker text in other file", true, func(t *testing.T, wt, _, _ string) {
			write(t, wt, "README.md", ActiveMarkerStart+"\nx\n"+ActiveMarkerEnd+"\n\n")
		}},
		{"staged edit hidden by pointer", true, func(t *testing.T, wt, target, _ string) {
			pointer := read(t, wt, target)
			write(t, wt, target, pointer+"Staged edit.\n")
			runWorkspaceGit(t, wt, "add", "-A")
			write(t, wt, target, pointer)
		}},
		{"mode change", true, func(t *testing.T, wt, target, _ string) {
			if err := os.Chmod(filepath.Join(wt, filepath.FromSlash(target)), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"deleted file", true, func(t *testing.T, wt, _, _ string) {
			if err := os.Remove(filepath.Join(wt, "main.go")); err != nil {
				t.Fatal(err)
			}
		}},
		{"renamed file", true, func(t *testing.T, wt, _, _ string) { runWorkspaceGit(t, wt, "mv", "main.go", "app.go") }},
		{"retargeted link", true, func(t *testing.T, wt, _, link string) {
			if link == "" {
				t.Skip("layout has no instruction link")
			}
			write(t, wt, "OTHER.md", guide)
			runWorkspaceGit(t, wt, "add", "OTHER.md")
			runWorkspaceGit(t, wt, "commit", "-m", "other")
			path := filepath.Join(wt, link)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("OTHER.md", path); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, layout := range layouts {
		for _, m := range mutations {
			t.Run(layout.name+"/"+m.name, func(t *testing.T) {
				t.Parallel()
				repo, wt, branch, base := instructionLinkRepo(t, 159, layout.entries)
				ctx := context.Background()
				if err := injectActivePointer(wt, ActiveMarkerStart+"\nGenerated pointer.\n"+ActiveMarkerEnd); err != nil {
					t.Fatal(err)
				}
				for _, target := range layout.targets {
					if only, err := OnlyActivePointer(wt, target); err != nil || !only {
						t.Fatalf("%s not pointer-only after injection: %v %v", target, only, err)
					}
				}
				m.apply(t, wt, layout.targets[0], layout.link)
				before := workspaceGitOutput(t, wt, "status", "--porcelain", "-z", "--untracked-files=all")
				if before == "" {
					t.Fatal("fixture has no raw changes")
				}

				got, err := hasStatusChanges(ctx, wt)
				if err != nil || got != m.dirty {
					t.Fatalf("hasStatusChanges = %v, %v; want %v\n%q", got, err, m.dirty, before)
				}
				head := workspaceGitOutput(t, wt, "rev-parse", "HEAD")
				if s := InspectCheckpoint(ctx, repo, "origin", 159, branch, base); s.Error != "" || s.Dirty != m.dirty {
					t.Fatalf("InspectCheckpoint dirty=%v error=%q; want %v", s.Dirty, s.Error, m.dirty)
				}
				if gotHead, dirty, _, err := StatusPRHead(ctx, wt); err != nil || dirty != m.dirty || gotHead != head {
					t.Fatalf("StatusPRHead = %s %v %v; want %v", gotHead, dirty, err, m.dirty)
				}
				// Mutation preflights keep raw Git semantics.
				if _, dirty, _, err := InspectPRHead(ctx, wt); err != nil || !dirty {
					t.Fatalf("InspectPRHead dirty=%v %v; want raw dirty", dirty, err)
				}
				if after := workspaceGitOutput(t, wt, "status", "--porcelain", "-z", "--untracked-files=all"); after != before {
					t.Fatalf("status observation changed the worktree:\n%q\n%q", before, after)
				}
			})
		}
	}
}

func TestStatusChangesReportsGitFailure(t *testing.T) {
	t.Parallel()
	if _, err := hasStatusChanges(context.Background(), t.TempDir()); err == nil {
		t.Fatal("non-repository reported as clean")
	}
}
