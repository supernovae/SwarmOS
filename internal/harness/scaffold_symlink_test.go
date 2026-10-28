package harness

import (
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// instructionRepo creates a Git repository with the given files and links
// (values starting with "->"), skipping when the host cannot create symlinks.
func instructionRepo(t *testing.T, entries map[string]string) string {
	t.Helper()
	root := t.TempDir()
	if out, err := gitfixture.Command(t, "", "-C", root, "init", "-q", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	for rel, value := range entries {
		path := filepath.Join(root, filepath.FromSlash(rel))
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
	return root
}

func TestInitInstructionSymlinks(t *testing.T) {
	guide := "# Team guide\n\nRun make check.\n"
	for _, tc := range []struct {
		name    string
		entries map[string]string
		target  string
	}{
		{"agents to claude", map[string]string{"CLAUDE.md": guide, "AGENTS.md": "->CLAUDE.md"}, "CLAUDE.md"},
		{"claude to agents", map[string]string{"AGENTS.md": guide, "CLAUDE.md": "->AGENTS.md"}, "AGENTS.md"},
		{"shared third file", map[string]string{"docs/AI.md": guide, "docs/latest.md": "->AI.md", "AGENTS.md": "->docs/latest.md", "CLAUDE.md": "->docs/AI.md"}, "docs/AI.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := instructionRepo(t, tc.entries)
			links := map[string]string{}
			for rel, value := range tc.entries {
				if strings.HasPrefix(value, "->") {
					links[rel] = filepath.FromSlash(strings.TrimPrefix(value, "->"))
				}
			}
			dry, err := InitRepository(InitOptions{RepoRoot: root, DryRun: true})
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := dry.Diffs[tc.target]; !ok {
				t.Fatalf("dry run did not diff %s: %v", tc.target, dry.Diffs)
			}
			res, err := InitRepository(InitOptions{RepoRoot: root})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(res.SectionFiles, tc.target) {
				t.Fatalf("section not reported against %s: %+v", tc.target, res.SectionFiles)
			}
			check := func() string {
				t.Helper()
				for rel, want := range links {
					got, err := os.Readlink(filepath.Join(root, filepath.FromSlash(rel)))
					if err != nil || got != want {
						t.Fatalf("link %s = %q, %v; want %q", rel, got, err, want)
					}
				}
				data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(tc.target)))
				if err != nil {
					t.Fatal(err)
				}
				body := string(data)
				if !strings.HasPrefix(body, guide) || strings.Count(body, harnessSectionStart) != 1 || strings.Count(body, harnessSectionEnd) != 1 {
					t.Fatalf("%s does not hold exactly one section after the team's text:\n%s", tc.target, body)
				}
				return body
			}
			first := check()
			manifest, err := os.ReadFile(filepath.Join(root, ManifestPath))
			if err != nil {
				t.Fatal(err)
			}
			again, err := InitRepository(InitOptions{RepoRoot: root})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(again.CurrentFiles, tc.target) || len(again.SectionFiles) != 0 {
				t.Fatalf("rerun reported changes: %+v", again)
			}
			if check() != first {
				t.Fatal("rerun changed the instruction target")
			}
			if after, _ := os.ReadFile(filepath.Join(root, ManifestPath)); string(after) != string(manifest) {
				t.Fatal("rerun changed the manifest")
			}
			if _, err := InitRepository(InitOptions{RepoRoot: root, Force: true}); err != nil {
				t.Fatal(err)
			}
			if check() != first {
				t.Fatal("forced rerun changed the instruction target")
			}
		})
	}
}

func TestInitRefusesUnsafeInstructionLinksBeforeWriting(t *testing.T) {
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "AI.md")
	if err := os.WriteFile(sentinel, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		entries map[string]string
	}{
		{"absolute outside", map[string]string{"AGENTS.md": "->" + filepath.ToSlash(sentinel)}},
		{"relative outside", map[string]string{"AGENTS.md": "->../" + filepath.Base(outside) + "/AI.md"}},
		{"directory", map[string]string{"docs/readme.md": "x", "AGENTS.md": "->docs"}},
		{"dangling", map[string]string{"AGENTS.md": "->missing.md"}},
		// AGENTS.md is valid; the unsafe CLAUDE.md link must still stop init.
		{"second name outside", map[string]string{"AGENTS.md": "guide\n", "CLAUDE.md": "->" + filepath.ToSlash(sentinel)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := instructionRepo(t, tc.entries)
			before := map[string]string{}
			for rel, value := range tc.entries {
				if !strings.HasPrefix(value, "->") {
					before[rel] = value
				}
			}
			if _, err := InitRepository(InitOptions{RepoRoot: root}); err == nil || !strings.Contains(err.Error(), "inside the repository") {
				t.Fatalf("init error = %v", err)
			}
			if _, err := os.Lstat(filepath.Join(root, ".swarm")); !os.IsNotExist(err) {
				t.Fatalf("init wrote .swarm before refusing: %v", err)
			}
			for rel, want := range before {
				if got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))); err != nil || string(got) != want {
					t.Fatalf("%s changed: %q, %v", rel, got, err)
				}
			}
			if got, err := os.ReadFile(sentinel); err != nil || string(got) != "outside" {
				t.Fatalf("outside file changed: %q, %v", got, err)
			}
		})
	}
}
