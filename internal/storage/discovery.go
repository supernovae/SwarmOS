package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/registry"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/supernovae/swarmos/internal/gitcredential"
)

// cappedOutput terminates a command as soon as its combined stdout/stderr
// budget is exhausted. exec.CommandContext also terminates on cancellation.
type cappedOutput struct {
	buffer    bytes.Buffer
	remaining int
	cancel    context.CancelFunc
	reserve   func(int64) error
}

func (w *cappedOutput) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		w.cancel()
		return 0, ErrLimit
	}
	if w.reserve != nil {
		if err := w.reserve(int64(len(p)) * 8); err != nil {
			w.cancel()
			return 0, err
		}
	}
	w.remaining -= len(p)
	return w.buffer.Write(p)
}
func (s *scanner) git(root string, args ...string) (string, error) {
	if err := s.ctx.Err(); err != nil {
		return "", err
	}
	const capBytes = 128 << 10
	if err := s.b.Charge(2048); err != nil {
		return "", err
	}
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	out := &cappedOutput{remaining: capBytes, cancel: cancel, reserve: s.b.Charge}
	if s.opts.GitOutput != nil {
		reader, err := s.opts.GitOutput(ctx, root, args...)
		if err != nil {
			return "", err
		}
		defer reader.Close()
		if _, err = io.Copy(out, reader); err != nil {
			return "", err
		}
		if err = ctx.Err(); err != nil {
			return "", err
		}
		return strings.TrimSpace(out.buffer.String()), nil
	}
	cmd, err := gitcredential.ObservationCommand(ctx, root, args...)
	if err != nil {
		return "", err
	}
	cmd.Env = append(cmd.Env, "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.buffer.String()), nil
}
func (s *scanner) repository(owner, path, backend string) {
	if err := s.ctx.Err(); err != nil {
		s.fail("repositories", err)
		return
	}
	if !filepath.IsAbs(path) {
		s.fail("repositories", errors.New("nonabsolute path"))
		return
	}
	root, err := openExisting(path)
	if err != nil {
		s.fail("repositories", err)
		return
	}
	root.Close()
	common, err := s.git(path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || !filepath.IsAbs(common) {
		s.fail("repositories", err)
		return
	}
	if _, ok := s.repos[common]; ok {
		return
	}
	if s.b.Counters.Repositories >= s.b.Limits.Repositories {
		s.fail("repositories", ErrLimit)
		return
	}
	if err := s.b.Charge(int64(4096 + 8*(len(common)+len(owner)))); err != nil {
		s.fail("repositories", err)
		return
	}
	s.b.Counters.Repositories++
	// A selected linked worktree shares the main checkout's managed paths.
	// Only use the conventional .git anchor after component-safe validation.
	if filepath.Base(common) == ".git" {
		primary := filepath.Dir(common)
		if anchor, err := openExisting(primary); err == nil {
			anchor.Close()
			path = primary
		}
	}
	if owner == "" {
		owner = "local/" + filepath.Base(path)
	}
	s.repos[common] = owner
	s.add(Resource{Repository: owner, Owner: owner, Class: "shared-git", Path: filepath.Join(common, "objects"), Identity: "git:" + common, Eligibility: "retained", Reason: "shared repository objects; no independent branch savings"})
	start := len(s.out.Resources)
	managed := filepath.Join(path, ".swarm", "worktrees")
	s.directory(managed, func(e os.DirEntry) {
		p := filepath.Join(managed, e.Name())
		class := "unknown-worktree"
		if strings.HasPrefix(e.Name(), "issue-") {
			class = "issue-worktree"
		}
		if strings.HasPrefix(e.Name(), "pr-") {
			class = "pr-worktree"
		}
		s.add(Resource{Repository: owner, Owner: owner, Class: class, Path: p, Identity: "worktree:" + p})
	})
	registrations, err := s.git(path, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		s.fail("worktree registrations", err)
	} else {
		for _, record := range strings.Split(registrations, "\x00\x00") {
			fields := strings.Split(record, "\x00")
			if len(fields) == 0 {
				continue
			}
			p, ok := strings.CutPrefix(fields[0], "worktree ")
			if !ok {
				continue
			}
			rel, e := filepath.Rel(managed, p)
			if e != nil || !filepath.IsLocal(rel) || strings.ContainsRune(rel, filepath.Separator) {
				continue
			}
			class := "unknown-worktree"
			if strings.HasPrefix(rel, "issue-") {
				class = "issue-worktree"
			}
			if strings.HasPrefix(rel, "pr-") {
				class = "pr-worktree"
			}
			r := Resource{Repository: owner, Owner: owner, Class: class, Path: p, Identity: "worktree:" + p}
			for _, field := range fields {
				if strings.HasPrefix(field, "locked") {
					r.Eligibility = "blocked"
					r.Reason = "locked Git worktree"
				}
			}
			s.add(r)
		}
	}
	refs, err := s.git(path, "for-each-ref", "--format=%(refname)", "refs/heads/swarm/")
	if err != nil {
		s.fail("code branches", err)
	} else {
		for _, ref := range strings.Split(refs, "\n") {
			if ref == "" {
				continue
			}
			s.add(Resource{Repository: owner, Owner: owner, Class: "code-branch", Path: path, Ref: ref, Identity: "ref:" + common + ":" + ref})
		}
	}
	s.retention(owner, path, backend, start)
}
func (s *scanner) cacheOwner(key string) string {
	if len(key) != 32 {
		return ""
	}
	if _, err := hex.DecodeString(key); err != nil {
		return ""
	}
	for common, owner := range s.repos {
		h := sha256.Sum256([]byte(filepath.Clean(common)))
		if hex.EncodeToString(h[:16]) == key {
			return owner
		}
	}
	return ""
}
func (s *scanner) caches() {
	if s.opts.CacheRoot == "" {
		return
	}
	for _, class := range []string{"agent-build", "validate-build", "go-build", "agent-deps", "validate-deps"} {
		base := filepath.Join(s.opts.CacheRoot, class)
		s.directory(base, func(key os.DirEntry) {
			p := filepath.Join(base, key.Name())
			owner := s.cacheOwner(key.Name())
			if !privateLayout(s.opts.CacheRoot, filepath.Join(class, key.Name())) {
				owner = ""
			}
			if class == "go-build" {
				r := Resource{Owner: owner, Repository: owner, Class: "legacy-go-build", Path: p, Identity: "cache:" + p}
				if owner == "" {
					r.Reason = "unproven legacy repository key"
					r.Eligibility = "unknown"
				}
				s.add(r)
				return
			}
			children := 0
			s.directory(p, func(eco os.DirEntry) {
				children++
				path := filepath.Join(p, eco.Name())
				r := Resource{Owner: class, Repository: owner, Class: class, Path: path, Identity: "cache:" + path}
				if strings.HasPrefix(eco.Name(), ".prefetch-rust-") {
					r.Class = "prefetch-scratch"
					r.Owner = "unknown"
					r.Identity = "scratch:" + path
				}
				if owner == "" {
					r.Eligibility = "unknown"
					r.Reason = "orphan or unproven repository key; abandonment unknown"
				}
				s.add(r)
			})
			if children == 0 {
				s.add(Resource{Owner: class, Repository: owner, Class: class, Path: p, Identity: "cache:" + p})
			}
		})
	}
}
func (s *scanner) scratch() {
	if s.opts.ScratchRoot == "" {
		return
	}
	s.directory(s.opts.ScratchRoot, func(e os.DirEntry) {
		class := scratchClass(e.Name())
		if class == "" {
			return
		}
		path := filepath.Join(s.opts.ScratchRoot, e.Name())
		s.add(Resource{Owner: "unknown", Class: class, Path: path, Identity: "scratch:" + path, Eligibility: "unknown", Reason: "prefix identifies a candidate only; ownership and abandonment unproven"})
	})
}

var _ io.Writer = (*cappedOutput)(nil)

// Legacy association requires the same non-writable parent layout as cache
// producers. A matching hash alone cannot establish ownership.
func privateLayout(base, rel string) bool {
	root, err := openExisting(base)
	if err != nil {
		return false
	}
	defer root.Close()
	directory, err := root.OpenDirectory(".")
	if err != nil {
		return false
	}
	info, err := directory.Stat()
	directory.Close()
	if err != nil || info.Mode().Perm()&0022 != 0 {
		return false
	}
	prefix := ""
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		prefix = filepath.Join(prefix, part)
		info, err := root.Lstat(prefix)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
			return false
		}
	}
	return true
}

// scratchClass accepts only the managed temporary-directory naming layouts.
func scratchClass(name string) string {
	for prefix, class := range map[string]string{"swarm-worker-": "sandbox-scratch", "swarm-native-": "native-scratch", "swarm-prefetch-": "prefetch-scratch", "swarm-review-": "review-scratch", "swarm-upstream-review-": "review-scratch", "swarm-checkpoint-plan-": "review-scratch", "swarm-check-home-": "validation-scratch", "swarm-warmup-": "validation-scratch", "swarm-repair-base-": "validation-scratch", "swarm-harness-inspect-": "sandbox-scratch", "swarm-executor-check-": "sandbox-scratch", "swarm-overlay-check-": "sandbox-scratch"} {
		if strings.HasPrefix(name, prefix) && len(name) > len(prefix) {
			return class
		}
	}
	return ""
}

// SelectedRepository is a validated checkout identity, not registry authority.
type SelectedRepository struct {
	Repository string `json:"repository"`
	RepositoryScope
}

// DiscoverScopes reads only existing registrations and Git metadata. All-known
// scope never falls back to the caller's current checkout.
func DiscoverScopes(ctx context.Context, opts Options, all bool) ([]SelectedRepository, []string, error) {
	b := Budget{Limits: DefaultLimits()}
	s := &scanner{ctx: ctx, opts: opts, b: b}
	entries, err := registry.New(opts.RegistryPath).ReadSnapshot(ctx, b.Limits.Repositories, s.b.Charge)
	reasons := []string{}
	if err != nil {
		reasons = append(reasons, "registry snapshot unavailable")
	}
	chosen := []registry.RepoEntry{}
	if all {
		chosen = entries
	} else if opts.SelectedRoot != "" {
		chosen = append(chosen, registry.RepoEntry{Repo: opts.Repository, Path: opts.SelectedRoot, Backend: opts.Backend})
		for _, e := range entries {
			if filepath.Clean(e.Path) == filepath.Clean(opts.SelectedRoot) {
				chosen[0] = e
				if opts.Repository != "" && !strings.EqualFold(e.Repo, opts.Repository) {
					return nil, reasons, errors.New("repository selector mismatch")
				}
				if opts.Backend != "" && opts.Backend != e.Backend {
					return nil, reasons, errors.New("backend selector mismatch")
				}
			}
		}
	} else if opts.Repository != "" {
		for _, e := range entries {
			if strings.EqualFold(e.Repo, opts.Repository) {
				chosen = append(chosen, e)
			}
		}
		if len(chosen) == 0 {
			reasons = append(reasons, "selected repository is not registered")
		}
	} else {
		cwd, e := os.Getwd()
		if e != nil {
			return nil, reasons, e
		}
		root, e := s.git(cwd, "rev-parse", "--show-toplevel")
		if e != nil {
			return nil, reasons, errors.New("select a repository checkout or --all-repos")
		}
		chosen = append(chosen, registry.RepoEntry{Path: root, Backend: opts.Backend})
	}
	out := []SelectedRepository{}
	seen := map[string]int{}
	bad := map[string]bool{}
	claims := map[string]registry.RepoEntry{}
	for _, e := range chosen {
		if err := ctx.Err(); err != nil {
			return out, reasons, err
		}
		root, err := filepath.Abs(e.Path)
		if err != nil || !filepath.IsAbs(e.Path) {
			reasons = append(reasons, "registered root is not absolute")
			continue
		}
		common, err := s.git(root, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			reasons = append(reasons, "repository identity unavailable")
			continue
		}
		if !all && e.Backend == "" {
			for _, registered := range entries {
				observed, e2 := s.git(registered.Path, "rev-parse", "--path-format=absolute", "--git-common-dir")
				if e2 != nil || observed != common {
					continue
				}
				if e.Repo != "" && !strings.EqualFold(e.Repo, registered.Repo) {
					bad[common] = true
					reasons = append(reasons, "repository selector mismatch")
					continue
				}
				if e.Backend != "" && e.Backend != registered.Backend {
					bad[common] = true
					reasons = append(reasons, "conflicting registrations")
					continue
				}
				e.Repo = registered.Repo
				e.Backend = registered.Backend
			}
		}
		if prior, ok := claims[common]; ok && (prior.Backend != e.Backend || !strings.EqualFold(prior.Repo, e.Repo)) {
			bad[common] = true
			reasons = append(reasons, "conflicting registrations")
		}
		claims[common] = e
		if opts.Backend != "" && e.Backend != "" && opts.Backend != e.Backend {
			reasons = append(reasons, "backend selector mismatch")
			continue
		}
		backend := e.Backend
		if backend == "" {
			backend = opts.Backend
		}
		if backend == "" {
			backend = "github"
		}
		scope, err := canonicalScope(s, RepositoryScope{root, common, backend})
		if err != nil {
			reasons = append(reasons, "invalid repository scope")
			continue
		}
		if filepath.Base(scope.CommonDirectory) == ".git" {
			scope.Root = filepath.Dir(scope.CommonDirectory)
		}
		if backend == "github" {
			owner := registry.DetectGitHubSlugFrom(scope.Root, firstRemote(opts.Remote))
			if owner == "" || e.Repo != "" && !strings.EqualFold(owner, e.Repo) {
				reasons = append(reasons, "repository remote identity mismatch")
				continue
			}
			e.Repo = owner
		}
		if i, ok := seen[scope.CommonDirectory]; ok {
			if out[i].Backend != backend || !strings.EqualFold(out[i].Repository, e.Repo) {
				bad[scope.CommonDirectory] = true
				reasons = append(reasons, "conflicting registrations")
			}
			continue
		}
		seen[scope.CommonDirectory] = len(out)
		out = append(out, SelectedRepository{e.Repo, scope})
	}
	filtered := out[:0]
	for _, e := range out {
		if !bad[e.CommonDirectory] {
			filtered = append(filtered, e)
		}
	}
	if err != nil {
		return filtered, reasons, fmt.Errorf("discover registered repositories: %w", err)
	}
	return filtered, reasons, nil
}
func firstRemote(remote string) string {
	if remote == "" {
		return "origin"
	}
	return remote
}
