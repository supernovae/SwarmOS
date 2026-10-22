package harness

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScaffoldRejectsSymlinkDestinations(t *testing.T) {
	for _, name := range []string{".swarm", ".github", ".claude", ".gitignore"} {
		for _, force := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "-keep", true: "-force"}[force], func(t *testing.T) {
				root, outside := t.TempDir(), t.TempDir()
				target := outside
				if name == ".gitignore" {
					target = filepath.Join(outside, "ignore")
					if err := os.WriteFile(target, []byte("original"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
					t.Skip(err)
				}
				if _, err := InitRepository(InitOptions{RepoRoot: root, Force: force}); err == nil {
					t.Fatal("symlink accepted")
				}
				entries, err := os.ReadDir(outside)
				if err != nil {
					t.Fatal(err)
				}
				if name == ".gitignore" {
					b, err := os.ReadFile(target)
					if err != nil || string(b) != "original" {
						t.Fatal("outside ignore changed")
					}
				} else if len(entries) != 0 {
					t.Fatalf("outside writes: %v", entries)
				}
			})
		}
	}
}
