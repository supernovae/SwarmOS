package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// CleanupWorktree is one entry found under .swarm/worktrees. Kind is "issue"
// or "pr" for standard names and empty otherwise; Number is then zero.
type CleanupWorktree struct {
	Path       string `json:"path"`
	Kind       string `json:"kind,omitempty"`
	Number     int    `json:"number,omitempty"`
	Present    bool   `json:"present"`
	Registered bool   `json:"registered"`
}

// parseCleanupWorktreeName accepts only issue-N and pr-N with a canonical
// positive decimal N, so pr-007 or issue-+3 never alias another resource.
func parseCleanupWorktreeName(name string) (string, int) {
	for _, kind := range []string{"issue", "pr"} {
		digits, ok := strings.CutPrefix(name, kind+"-")
		if !ok || digits == "" || digits[0] == '0' {
			continue
		}
		n, err := strconv.Atoi(digits)
		if err != nil || n <= 0 || strconv.Itoa(n) != digits {
			continue
		}
		return kind, n
	}
	return "", 0
}

// DiscoverCleanupWorktrees unions managed directory entries with Git worktree
// registrations below .swarm/worktrees. It never follows entries, prunes,
// fetches or writes; per-resource safety is left to InspectCleanupWorktree.
func DiscoverCleanupWorktrees(ctx context.Context, root string) ([]CleanupWorktree, error) {
	canonical, managed, err := cleanupPaths(root, filepath.Join(root, ".swarm", "worktrees"))
	if err != nil {
		return nil, fmt.Errorf("resolve cleanup paths: %w", err)
	}
	found := map[string]*CleanupWorktree{}
	add := func(name string) *CleanupWorktree {
		path := filepath.Join(managed, name)
		if w := found[path]; w != nil {
			return w
		}
		kind, n := parseCleanupWorktreeName(name)
		w := &CleanupWorktree{Path: path, Kind: kind, Number: n}
		found[path] = w
		return w
	}
	info, err := os.Lstat(managed)
	switch {
	case os.IsNotExist(err):
	case err != nil:
		return nil, fmt.Errorf("inspect managed worktrees: %w", err)
	case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
		return nil, fmt.Errorf("unsafe managed worktree directory: %s", managed)
	default:
		entries, err := os.ReadDir(managed)
		if err != nil {
			return nil, fmt.Errorf("list managed worktrees: %w", err)
		}
		for _, e := range entries {
			add(e.Name()).Present = true
		}
	}
	registrations, err := cleanupOutput(ctx, canonical, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	for _, record := range strings.Split(registrations, "\x00\x00") {
		fields := strings.Split(record, "\x00")
		path, ok := strings.CutPrefix(fields[0], "worktree ")
		if !ok {
			continue
		}
		rel, err := filepath.Rel(managed, filepath.Clean(path))
		if err != nil || !filepath.IsLocal(rel) || strings.ContainsRune(rel, filepath.Separator) {
			continue
		}
		add(rel).Registered = true
	}
	out := make([]CleanupWorktree, 0, len(found))
	for _, w := range found {
		out = append(out, *w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
