package safefs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// instructionFixture builds files, directories (trailing slash), and links
// (values starting with "->") under a fresh root.
func instructionFixture(t *testing.T, entries map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for _, pass := range []string{"dirs", "files", "links"} {
		for name, value := range entries {
			path := filepath.Join(root, filepath.FromSlash(strings.TrimSuffix(name, "/")))
			var err error
			switch {
			case strings.HasSuffix(name, "/") && pass == "dirs":
				err = os.MkdirAll(path, 0o755)
			case strings.HasPrefix(value, "->") && pass == "links":
				if err = os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
					if err = os.Symlink(filepath.FromSlash(strings.TrimPrefix(value, "->")), path); err != nil {
						t.Skipf("symlink creation unavailable: %v", err)
					}
				}
			case !strings.HasSuffix(name, "/") && !strings.HasPrefix(value, "->") && pass == "files":
				if err = os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
					err = os.WriteFile(path, []byte(value), 0o644)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func TestResolveInstructionFollowsInternalLinks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries map[string]string
		want    map[string]Instruction
	}{
		{"regular and missing", map[string]string{"AGENTS.md": "a"}, map[string]Instruction{
			"AGENTS.md": {Name: "AGENTS.md", Target: "AGENTS.md", Exists: true},
			"CLAUDE.md": {Name: "CLAUDE.md", Target: "CLAUDE.md"},
		}},
		{"agents to claude", map[string]string{"CLAUDE.md": "c", "AGENTS.md": "->CLAUDE.md"}, map[string]Instruction{
			"AGENTS.md": {Name: "AGENTS.md", Target: "CLAUDE.md", Link: true, Exists: true},
			"CLAUDE.md": {Name: "CLAUDE.md", Target: "CLAUDE.md", Exists: true},
		}},
		{"claude to agents", map[string]string{"AGENTS.md": "a", "CLAUDE.md": "->./AGENTS.md"}, map[string]Instruction{
			"AGENTS.md": {Name: "AGENTS.md", Target: "AGENTS.md", Exists: true},
			"CLAUDE.md": {Name: "CLAUDE.md", Target: "AGENTS.md", Link: true, Exists: true},
		}},
		{"shared third file through links", map[string]string{
			"docs/guide/AI.md": "g", "docs/current": "->guide", "AGENTS.md": "->docs/current/AI.md", "CLAUDE.md": "->docs/guide/../guide/AI.md",
		}, map[string]Instruction{
			"AGENTS.md": {Name: "AGENTS.md", Target: filepath.Join("docs", "guide", "AI.md"), Link: true, Exists: true},
			"CLAUDE.md": {Name: "CLAUDE.md", Target: filepath.Join("docs", "guide", "AI.md"), Link: true, Exists: true},
		}},
		{"multi hop", map[string]string{"docs/AI.md": "g", "docs/link.md": "->AI.md", "CLAUDE.md": "->AGENTS.md", "AGENTS.md": "->docs/link.md"}, map[string]Instruction{
			"AGENTS.md": {Name: "AGENTS.md", Target: filepath.Join("docs", "AI.md"), Link: true, Exists: true},
			"CLAUDE.md": {Name: "CLAUDE.md", Target: filepath.Join("docs", "AI.md"), Link: true, Exists: true},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := Open(instructionFixture(t, tc.entries))
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			got, err := root.ResolveInstructions()
			if err != nil {
				t.Fatal(err)
			}
			for _, in := range got {
				if in != tc.want[in.Name] {
					t.Errorf("%s = %+v, want %+v", in.Name, in, tc.want[in.Name])
				}
			}
		})
	}
}

func TestResolveInstructionRefusesUnsafeLinks(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "AI.md"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		entries map[string]string
		want    string
	}{
		{"absolute outside", map[string]string{"AGENTS.md": "->" + filepath.ToSlash(filepath.Join(outside, "AI.md"))}, "not a relative path"},
		{"relative outside", map[string]string{"AGENTS.md": "->../AI.md"}, "leaves the repository"},
		{"leaves and returns", map[string]string{"CLAUDE.md": "c", "AGENTS.md": "->../" + "x/../CLAUDE.md"}, "leaves the repository"},
		{"outside directory chain", map[string]string{"out": "->" + filepath.ToSlash(outside), "AGENTS.md": "->out/AI.md"}, "not a relative path"},
		{"directory", map[string]string{"docs/": "", "AGENTS.md": "->docs"}, "directory"},
		{"root", map[string]string{"AGENTS.md": "->."}, "repository root"},
		{"dangling", map[string]string{"AGENTS.md": "->missing.md"}, "does not exist"},
		{"cycle", map[string]string{"AGENTS.md": "->CLAUDE.md", "CLAUDE.md": "->AGENTS.md"}, "cycle"},
		{"through file", map[string]string{"file": "x", "AGENTS.md": "->file/AI.md"}, "passes through the file"},
		{"git metadata", map[string]string{".git/config": "x", "AGENTS.md": "->.git/config"}, ".git"},
		{"swarm control", map[string]string{".swarm/project.md": "x", "CLAUDE.md": "->.swarm/project.md"}, ".swarm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := Open(instructionFixture(t, tc.entries))
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			_, err = root.ResolveInstructions()
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "inside the repository") {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestResolveInstructionOnlyForInstructionNames(t *testing.T) {
	root, err := Open(instructionFixture(t, map[string]string{"real.md": "x", "README.md": "->real.md"}))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, name := range []string{"README.md", "docs/AGENTS.md", "../AGENTS.md"} {
		if _, err := root.ResolveInstruction(name); err == nil {
			t.Fatalf("%s resolved", name)
		}
	}
	// Generic reads keep refusing links, including resolved instruction links.
	if _, err := root.ReadFile("README.md", 10); err == nil {
		t.Fatal("generic read followed a link")
	}
}
