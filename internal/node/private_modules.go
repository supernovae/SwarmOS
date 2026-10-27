package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/registry"
	"github.com/supernovae/swarmos/internal/sandbox"
	"gopkg.in/yaml.v3"
)

// PrivateGoEntry is one private_go_modules entry of the node profile, as
// node status and doctor report it.
type PrivateGoEntry struct {
	Repository string   `json:"repository"`
	Patterns   []string `json:"patterns"`
	Registered bool     `json:"registered"`
	// Problem says why the entry allows nothing, or is empty.
	Problem string `json:"problem,omitempty"`
}

// ValidatePrivateGoPattern accepts a GOPRIVATE-style module path pattern
// whose first path element is a literal host name, so no pattern can match
// every module or every host.
func ValidatePrivateGoPattern(pattern string) error {
	if pattern == "" || strings.ContainsAny(pattern, ", \t\r\n") {
		return fmt.Errorf("private Go module pattern %q must be one non-empty pattern without commas or spaces", pattern)
	}
	if _, err := path.Match(pattern, ""); err != nil {
		return fmt.Errorf("private Go module pattern %q: %w", pattern, err)
	}
	host, _, _ := strings.Cut(pattern, "/")
	if strings.ContainsAny(host, `*?[\`) || !strings.Contains(host, ".") {
		return fmt.Errorf("private Go module pattern %q must start with a literal host name such as github.com", pattern)
	}
	return nil
}

// PrivateGoEntries lists prof's private_go_modules entries in repository
// order, marking each registered or not in the registry at registryPath. An
// entry with an invalid repository or pattern has a Problem and allows
// nothing.
func PrivateGoEntries(prof *contract.NodeProfile, registryPath string) []PrivateGoEntry {
	if prof == nil || len(prof.PrivateGoModules) == 0 {
		return nil
	}
	registered := map[string]bool{}
	for _, e := range registeredRepos(registryPath) {
		registered[strings.ToLower(e.Repo)] = true
	}
	entries := make([]PrivateGoEntry, 0, len(prof.PrivateGoModules))
	for repo, patterns := range prof.PrivateGoModules {
		entry := PrivateGoEntry{Repository: repo, Patterns: append([]string{}, patterns...), Registered: registered[strings.ToLower(repo)]}
		if err := registry.ValidateRepoSlug(repo); err != nil || strings.TrimSpace(repo) != repo {
			entry.Problem = fmt.Sprintf("invalid repository %q: expected owner/repo", repo)
		} else if len(patterns) == 0 {
			entry.Problem = "no patterns"
		} else {
			for _, p := range patterns {
				if err := ValidatePrivateGoPattern(p); err != nil {
					entry.Problem = err.Error()
					break
				}
			}
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].Repository) < strings.ToLower(entries[j].Repository)
	})
	return entries
}

// ProfilePrivateGo is prof's private Go module allowance for the registered
// repository whose Git common directory is worktree's. An unregistered
// worktree, or an entry with a Problem, allows nothing; warnings explain why.
func ProfilePrivateGo(prof *contract.NodeProfile, registryPath, worktree string) (sandbox.PrivateGo, []string) {
	if prof == nil || len(prof.PrivateGoModules) == 0 {
		return sandbox.PrivateGo{}, nil
	}
	var warnings []string
	allow := sandbox.PrivateGo{Repository: registeredRepoFor(registryPath, worktree)}
	invalid := false
	for _, entry := range PrivateGoEntries(prof, registryPath) {
		mine := allow.Repository != "" && strings.EqualFold(entry.Repository, allow.Repository)
		if entry.Problem != "" {
			warnings = append(warnings, fmt.Sprintf("private_go_modules[%q] allows nothing: %s", entry.Repository, entry.Problem))
			invalid = invalid || mine
		} else if mine {
			allow.Patterns = append(allow.Patterns, entry.Patterns...)
		}
	}
	// A repository with an invalid entry gets nothing, even when another
	// spelling of its name is valid.
	if invalid {
		allow.Patterns = nil
	}
	sort.Strings(allow.Patterns)
	return allow, warnings
}

// LoadDependencyPolicy reads the node profile at profilePath and returns its
// dependency cache policy for worktree. A missing profile gives the default
// per-repository caches and no private modules; a malformed profile gives the
// same, with an error.
func LoadDependencyPolicy(profilePath, registryPath, worktree string) (sandbox.DependencyPolicy, []string, error) {
	prof, err := readProfile(profilePath)
	if err != nil || prof == nil {
		return sandbox.DependencyPolicy{Mode: sandbox.PerRepository}, nil, err
	}
	allow, warnings := ProfilePrivateGo(prof, registryPath, worktree)
	return sandbox.DependencyPolicy{Mode: ProfileDependencyCacheMode(prof), PrivateGo: allow}, warnings, nil
}

// DependencyPolicy is LoadDependencyPolicy without its diagnostics, which
// doctor reports. Every failure allows no private modules.
func DependencyPolicy(profilePath, registryPath, worktree string) sandbox.DependencyPolicy {
	policy, _, _ := LoadDependencyPolicy(profilePath, registryPath, worktree)
	return policy
}

// readProfile returns the profile at customPath, or nil when it is missing.
func readProfile(customPath string) (*contract.NodeProfile, error) {
	if customPath == "" {
		customPath = DefaultNodeConfigPath()
	}
	data, err := os.ReadFile(customPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read node profile %s: %w", customPath, err)
	}
	var prof contract.NodeProfile
	if err := yaml.Unmarshal(data, &prof); err != nil {
		return nil, fmt.Errorf("parse node profile %s: %w", customPath, err)
	}
	return &prof, nil
}

// registeredRepos lists the registry without creating it.
func registeredRepos(registryPath string) []registry.RepoEntry {
	if registryPath == "" {
		registryPath = registry.DefaultRegistryPath()
	}
	if _, err := os.Stat(registryPath); err != nil {
		return nil
	}
	entries, err := registry.New(registryPath).List()
	if err != nil {
		return nil
	}
	return entries
}

// registeredRepoFor names the registered repository sharing worktree's Git
// common directory, so issue and PR worktrees resolve to their repository.
// It returns "" when none does.
func registeredRepoFor(registryPath, worktree string) string {
	if worktree == "" {
		return ""
	}
	want := gitCommonDir(worktree)
	if want == "" {
		return ""
	}
	for _, e := range registeredRepos(registryPath) {
		if e.Path != "" && gitCommonDir(e.Path) == want {
			return e.Repo
		}
	}
	return ""
}

func gitCommonDir(dir string) string {
	out, err := gitcredential.Output(context.Background(), dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return ""
	}
	common := strings.TrimSpace(string(out))
	if !filepath.IsAbs(common) {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(common); err == nil {
		common = resolved
	}
	return filepath.Clean(common)
}
