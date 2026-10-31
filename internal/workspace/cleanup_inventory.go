package workspace

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/supernovae/swarmos/internal/safefs"
	"hash"
	"io"
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

// InventoryInspection describes filesystem evidence only, never cleanup
// authority. The caller must separately verify claims and the submitted PR.
type InventoryInspection struct {
	Complete, Blocked bool
	Reason            string
}

// InspectInventoryWorktree uses a caller-owned bounded metadata reader and
// shared entry/memory admissions. It never refreshes the index or runs filters.
func InspectInventoryWorktree(ctx context.Context, root, path, branch, expected string, readGit func(string, ...string) (string, error), entry func() error, reserve func(int64) error) InventoryInspection {
	unknown := func() InventoryInspection { return InventoryInspection{Reason: "worktree inspection incomplete"} }
	blocked := func(reason string) InventoryInspection {
		return InventoryInspection{Complete: true, Blocked: true, Reason: reason}
	}
	if ctx.Err() != nil || entry == nil || reserve == nil || readGit == nil {
		return unknown()
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || !filepath.IsLocal(rel) || !strings.HasPrefix(filepath.ToSlash(rel), ".swarm/worktrees/") {
		return unknown()
	}
	volume := filepath.VolumeName(path) + string(filepath.Separator)
	base, err := safefs.Open(volume)
	if err != nil {
		return unknown()
	}
	defer base.Close()
	absoluteRel, err := filepath.Rel(volume, path)
	if err != nil {
		return unknown()
	}
	pinned, err := base.Sub(absoluteRel, false, 0)
	if err != nil {
		return unknown()
	}
	defer pinned.Close()
	top, err := readGit(path, "rev-parse", "--show-toplevel")
	if err != nil || top != path {
		return unknown()
	}
	common, err := readGit(root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return unknown()
	}
	other, err := readGit(path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || common != other {
		return unknown()
	}
	invoking, _ := readGit(".", "rev-parse", "--show-toplevel")
	if invoking == path {
		return blocked("invoking checkout is retained")
	}
	registrations, err := readGit(root, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return unknown()
	}
	registered := false
	for _, record := range strings.Split(registrations, "\x00\x00") {
		fields := strings.Split(record, "\x00")
		if len(fields) == 0 || fields[0] != "worktree "+path {
			continue
		}
		registered = true
		for _, field := range fields {
			if strings.HasPrefix(field, "locked") {
				return blocked("locked Git worktree")
			}
		}
	}
	if !registered {
		return unknown()
	}
	head, err := readGit(path, "rev-parse", "--verify", "HEAD")
	if err != nil || head != expected || expected == "" {
		return blocked("worktree head differs from recorded submission")
	}
	actual, err := readGit(path, "symbolic-ref", "-q", "HEAD")
	if err != nil || actual != "refs/heads/"+branch {
		return blocked("worktree branch differs from recorded submission")
	}
	// Index-only enumeration and tree reads do not execute clean/smudge filters.
	index, err := readGit(path, "ls-files", "--stage", "-z")
	if err != nil {
		return unknown()
	}
	tree, err := readGit(path, "ls-tree", "-r", "-z", "HEAD")
	if err != nil {
		return unknown()
	}
	if err = reserve((1 << 20) + int64(len(index)+len(tree))*8); err != nil {
		return unknown()
	}
	tracked := map[string]string{}
	for _, line := range strings.Split(tree, "\x00") {
		if line == "" {
			continue
		}
		meta, name, ok := strings.Cut(line, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || !filepath.IsLocal(name) {
			return unknown()
		}
		if fields[0] == "160000" {
			return blocked("submodule requires independent retention review")
		}
		tracked[name] = fields[0] + " " + fields[2]
	}
	count := 0
	for _, line := range strings.Split(index, "\x00") {
		if line == "" {
			continue
		}
		meta, name, ok := strings.Cut(line, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || fields[2] != "0" || tracked[name] != fields[0]+" "+fields[1] {
			return blocked("index differs from submitted tree")
		}
		count++
	}
	if count != len(tracked) {
		return blocked("index differs from submitted tree")
	}
	visited := map[string]bool{}
	result := InventoryInspection{Complete: true}
	var buffer [32768]byte
	var walk func(string, int)
	walk = func(name string, depth int) {
		if !result.Complete || result.Blocked {
			return
		}
		if ctx.Err() != nil || depth > 64 || entry() != nil {
			result = unknown()
			return
		}
		if name != "." && filepath.Base(name) == ".git" {
			if name == ".git" {
				return
			}
			result = blocked("nested Git repository is retained")
			return
		}
		info, e := pinned.Lstat(name)
		if e != nil {
			result = unknown()
			return
		}
		if info.IsDir() {
			directory, e := pinned.OpenDirectory(name)
			if e != nil {
				result = unknown()
				return
			}
			defer directory.Close()
			for result.Complete && !result.Blocked {
				if ctx.Err() != nil {
					result = unknown()
					return
				}
				entries, e := directory.ReadDir(1)
				if len(entries) == 0 {
					if e != io.EOF {
						result = unknown()
					}
					break
				}
				child := filepath.Join(name, entries[0].Name())
				if e = reserve(int64(512 + 8*len(child))); e != nil {
					result = unknown()
					return
				}
				walk(child, depth+1)
			}
			after, e := directory.Stat()
			if e != nil || !info.ModTime().Equal(after.ModTime()) {
				result = unknown()
			}
			return
		}
		blob, trackedFile := tracked[filepath.ToSlash(name)]
		if !trackedFile {
			if cleanupManaged(filepath.ToSlash(name)) && info.Mode().IsRegular() {
				return
			}
			result = blocked("untracked or unsafe worktree data is retained")
			return
		}
		if !info.Mode().IsRegular() {
			result = blocked("non-regular tracked worktree data is retained")
			return
		}
		if info.Size() < 0 || info.Size() > 8<<20 {
			result = unknown()
			return
		}
		visited[filepath.ToSlash(name)] = true
		file, e := pinned.OpenRegular(name)
		if e != nil {
			result = unknown()
			return
		}
		defer file.Close()
		parts := strings.Fields(blob)
		if len(parts) != 2 {
			result = unknown()
			return
		}
		var hash hash.Hash
		switch len(parts[1]) {
		case 40:
			hash = sha1.New()
		case 64:
			hash = sha256.New()
		default:
			result = unknown()
			return
		}
		fmt.Fprintf(hash, "blob %d\x00", info.Size())
		remaining := info.Size()
		for remaining > 0 {
			if ctx.Err() != nil {
				result = unknown()
				return
			}
			n, e := file.Read(buffer[:min(int64(len(buffer)), remaining)])
			if n > 0 {
				hash.Write(buffer[:n])
				remaining -= int64(n)
			}
			if e != nil || n == 0 {
				result = unknown()
				return
			}
		}
		after, e := file.Stat()
		if e != nil || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) || info.Mode() != after.Mode() || !os.SameFile(info, after) {
			result = unknown()
			return
		}
		if hex.EncodeToString(hash.Sum(nil)) != parts[1] {
			result = blocked("working file differs from submitted blob or needs conversion")
			return
		}
		executable := info.Mode().Perm()&0111 != 0
		if executable != (parts[0] == "100755") {
			result = blocked("working file mode differs from submitted tree")
		}
	}
	walk(".", 0)
	if result.Complete && !result.Blocked && len(visited) != len(tracked) {
		return blocked("tracked worktree files are missing")
	}
	if result.Complete && !result.Blocked {
		after, err := readGit(path, "rev-parse", "--verify", "HEAD")
		if err != nil || after != head {
			return unknown()
		}
		result.Reason = "worktree files match submitted blobs"
	}
	return result
}
