package testutil

import (
	"os"
	"path/filepath"
	"strings"
)

// hostAgentNames are the coding-agent programs SwarmOS can discover on PATH.
var hostAgentNames = []string{"claude", "codex", "opencode", "omp", "grok", "dot"}

// HideHostAgents rewrites PATH for the test process so agent programs
// installed on the machine are not found. Tests then take the same time and
// give the same result on a developer workstation as on a clean CI runner,
// instead of starting whatever agent CLIs happen to be installed.
//
// Each PATH directory that holds an agent program is replaced by a shadow
// directory under root that links to everything else in it. Tests that need a
// fake agent still prepend their own directory to PATH.
func HideHostAgents(root string) error {
	agents := map[string]bool{}
	for _, name := range hostAgentNames {
		agents[name] = true
		agents[name+".exe"] = true
	}
	dirs := filepath.SplitList(os.Getenv("PATH"))
	for i, dir := range dirs {
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		found := false
		for _, entry := range entries {
			if agents[strings.ToLower(entry.Name())] {
				found = true
				break
			}
		}
		if !found {
			continue
		}
		shadow, err := os.MkdirTemp(root, "path-*")
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if agents[strings.ToLower(entry.Name())] {
				continue
			}
			if err := os.Symlink(filepath.Join(dir, entry.Name()), filepath.Join(shadow, entry.Name())); err != nil {
				return err
			}
		}
		dirs[i] = shadow
	}
	return os.Setenv("PATH", strings.Join(dirs, string(os.PathListSeparator)))
}
