package registry

import (
	"bytes"
	"context"
	"fmt"
	"github.com/supernovae/swarmos/internal/safefs"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/filelock"
	"gopkg.in/yaml.v3"
)

var validRepoSlugRegex = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

type RepoEntry struct {
	Repo    string    `yaml:"repo" json:"repo"`                           // "owner/repo"
	Path    string    `yaml:"path" json:"path"`                           // Absolute path to git repository root
	Backend string    `yaml:"backend,omitempty" json:"backend,omitempty"` // "github" or "local"
	AddedAt time.Time `yaml:"added_at" json:"added_at"`
}

type registryFile struct {
	Version int                  `yaml:"version"`
	Repos   map[string]RepoEntry `yaml:"repos"`
}

type Registry struct {
	filePath string
}

func DefaultRegistryPath() string {
	if env := os.Getenv("SWARM_REGISTRY_PATH"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".swarm", "repos.yaml")
	}
	return filepath.Join(home, ".config", "swarm", "repos.yaml")
}

func DefaultCloneBaseDir() string {
	if env := os.Getenv("SWARM_REPOS_DIR"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".swarm", "repos")
	}
	return filepath.Join(home, ".swarm", "repos")
}

func New(customPath string) *Registry {
	if customPath == "" {
		customPath = DefaultRegistryPath()
	}
	return &Registry{filePath: customPath}
}

func ValidateRepoSlug(repo string) error {
	repo = strings.TrimSpace(repo)
	if !validRepoSlugRegex.MatchString(repo) || strings.Contains(repo, "..") {
		return fmt.Errorf("invalid repository identifier %q: expected owner/repo", repo)
	}
	return nil
}

func (r *Registry) withLock(mutate bool, fn func(rf *registryFile) error) error {
	if err := os.MkdirAll(filepath.Dir(r.filePath), 0o755); err != nil {
		return err
	}
	lockPath := r.filePath + ".lock"
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lf.Close()

	if err := filelock.LockExclusive(lf); err != nil {
		return fmt.Errorf("filelock registry failed: %w", err)
	}
	defer func() {
		_ = filelock.Unlock(lf)
	}()

	rf := &registryFile{
		Version: 1,
		Repos:   make(map[string]RepoEntry),
	}
	if data, err := os.ReadFile(r.filePath); err == nil && len(data) > 0 {
		if err := yaml.Unmarshal(data, rf); err != nil {
			return fmt.Errorf("corrupt repo registry %s: %w", r.filePath, err)
		}
		if rf.Repos == nil {
			rf.Repos = make(map[string]RepoEntry)
		}
	}

	if err := fn(rf); err != nil {
		return err
	}

	if mutate {
		out, err := yaml.Marshal(rf)
		if err != nil {
			return err
		}
		return os.WriteFile(r.filePath, out, 0o644)
	}
	return nil
}

// Add registers a repository and verifies logical git safety:
// 1. Path must be a valid git repository top-level root.
// 2. If the git repository has an origin remote pointing to a GitHub owner/repo, it must match `repo` (preventing cross-repo mixups).
func (r *Registry) Add(repo, localPath, backend string) (*RepoEntry, error) {
	return r.AddFromRemote(repo, localPath, backend, "")
}

// AddFromRemote is Add with the repository identity read from the named
// upstream remote; empty means origin.
func (r *Registry) AddFromRemote(repo, localPath, backend, upstreamRemote string) (*RepoEntry, error) {
	if localPath == "" {
		localPath = "."
	}
	absPath, err := filepath.Abs(localPath)
	if err != nil {
		return nil, err
	}

	topOut, err := exec.Command("git", "-C", absPath, "rev-parse", "--show-toplevel").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("path %q is not a git repository: %w (%s)", absPath, err, strings.TrimSpace(string(topOut)))
	}
	gitRoot := strings.TrimSpace(string(topOut))

	detectedSlug := DetectGitHubSlugFrom(gitRoot, upstreamRemote)
	if repo == "" {
		if detectedSlug != "" {
			repo = detectedSlug
		} else {
			repo = "local/" + filepath.Base(gitRoot)
		}
	}

	if err := ValidateRepoSlug(repo); err != nil {
		return nil, err
	}

	// Safety check: if git remote origin has a GitHub slug and user passed a different github owner/repo, reject mismatch
	if detectedSlug != "" && !strings.HasPrefix(repo, "local/") && !strings.EqualFold(detectedSlug, repo) {
		return nil, fmt.Errorf("repository mismatch: path %q has remote %q, which does not match %q", gitRoot, detectedSlug, repo)
	}

	if backend == "" {
		if strings.HasPrefix(repo, "local/") || detectedSlug == "" {
			backend = "local"
		} else {
			backend = "github"
		}
	}

	key := strings.ToLower(repo)
	var saved RepoEntry
	err = r.withLock(true, func(rf *registryFile) error {
		entry := RepoEntry{
			Repo:    repo,
			Path:    gitRoot,
			Backend: backend,
			AddedAt: time.Now().UTC(),
		}
		rf.Repos[key] = entry
		saved = entry
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &saved, nil
}

func (r *Registry) Get(repo string) (*RepoEntry, bool, error) {
	key := strings.ToLower(strings.TrimSpace(repo))
	var found RepoEntry
	var ok bool
	err := r.withLock(false, func(rf *registryFile) error {
		found, ok = rf.Repos[key]
		return nil
	})
	if err != nil || !ok {
		return nil, false, err
	}
	return &found, true, nil
}

func (r *Registry) Remove(repo string) error {
	key := strings.ToLower(strings.TrimSpace(repo))
	return r.withLock(true, func(rf *registryFile) error {
		if _, ok := rf.Repos[key]; !ok {
			return fmt.Errorf("repository %q is not registered", repo)
		}
		delete(rf.Repos, key)
		return nil
	})
}

func (r *Registry) List() ([]RepoEntry, error) {
	var list []RepoEntry
	err := r.withLock(false, func(rf *registryFile) error {
		for _, e := range rf.Repos {
			list = append(list, e)
		}
		sort.Slice(list, func(i, j int) bool {
			return strings.ToLower(list[i].Repo) < strings.ToLower(list[j].Repo)
		})
		return nil
	})
	return list, err
}

func DetectGitHubSlug(repoRoot string) string {
	return DetectGitHubSlugFrom(repoRoot, "")
}

// DetectGitHubSlugFrom reads the GitHub owner/repo of the named remote; empty
// means origin. It returns "" for a missing, invalid, or non-GitHub remote.
func DetectGitHubSlugFrom(repoRoot, remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		remote = "origin"
	}
	if strings.HasPrefix(remote, "-") || strings.ContainsAny(remote, " /\\:\x00\n") {
		return ""
	}
	out, err := exec.Command("git", "-C", repoRoot, "remote", "get-url", remote).Output()
	if err != nil {
		return ""
	}
	raw := strings.TrimSpace(string(out))
	raw = strings.TrimSuffix(raw, ".git")
	for _, prefix := range []string{"https://github.com/", "http://github.com/", "git@github.com:", "ssh://git@github.com/"} {
		if strings.HasPrefix(raw, prefix) {
			slug := strings.TrimPrefix(raw, prefix)
			if ValidateRepoSlug(slug) == nil {
				return slug
			}
		}
	}
	return ""
}

// ReadSnapshot reads an existing registry without taking locks or initializing
// storage. The caller reserves parser working space before any input is read.
// Oversized documents fail closed rather than being decoded and truncated.
func (r *Registry) ReadSnapshot(ctx context.Context, maxRepos int, reserve func(int64) error) ([]RepoEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxRepos <= 0 || reserve == nil {
		return nil, fmt.Errorf("snapshot budget required")
	}
	path, err := filepath.Abs(r.filePath)
	if err != nil {
		return nil, err
	}
	volume := filepath.VolumeName(path) + string(filepath.Separator)
	root, err := safefs.Open(volume)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	rel, err := filepath.Rel(volume, path)
	if err != nil {
		return nil, err
	}
	f, err := root.OpenRegular(rel)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	// yaml.v3 builds a syntax tree. Bound both its input and conservative
	// parser/tree/decoded working space, independently of record count.
	const maxDocument = 256 << 10
	if info.Size() < 0 || info.Size() > maxDocument {
		return nil, fmt.Errorf("registry exceeds bounded document size")
	}
	if err := reserve(65536 + 128*info.Size() + int64(maxRepos)*1024); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, info.Size()+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != info.Size() {
		return nil, fmt.Errorf("registry changed during observation")
	}
	// Avoid aliases and deeply nested input before invoking the YAML parser.
	// Canonical registries contain only mappings and scalar fields.
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) > 8192 {
			return nil, fmt.Errorf("registry field exceeds limit")
		}
		indent := 0
		for indent < len(line) && line[indent] == ' ' {
			indent++
		}
		if indent > 8 || bytes.ContainsAny(line, "&*[]{}") {
			return nil, fmt.Errorf("unsupported bounded registry encoding")
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var rf registryFile
	if err := yaml.Unmarshal(data, &rf); err != nil {
		return nil, err
	}
	if rf.Version != 1 {
		return nil, fmt.Errorf("unsupported registry version")
	}
	out := make([]RepoEntry, 0, min(len(rf.Repos), maxRepos))
	keys := make([]string, 0, len(rf.Repos))
	for key := range rf.Repos {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		e := rf.Repos[key]
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if len(out) >= maxRepos {
			return out, fmt.Errorf("registry repository limit reached")
		}
		if ValidateRepoSlug(e.Repo) != nil || !filepath.IsAbs(e.Path) {
			return out, fmt.Errorf("invalid registry entry")
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Repo < out[j].Repo })
	after, err := f.Stat()
	if err != nil {
		return out, err
	}
	if after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return out, fmt.Errorf("registry changed during observation")
	}
	return out, nil
}
