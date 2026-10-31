package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
		class := ""
		for prefix, c := range map[string]string{"swarm-worker-": "sandbox-scratch", "swarm-native-": "native-scratch", "swarm-prefetch-": "prefetch-scratch", "swarm-review-": "review-scratch", "swarm-check-home-": "validation-scratch", "swarm-harness-inspect-": "sandbox-scratch", "swarm-executor-check-": "sandbox-scratch", "swarm-overlay-check-": "sandbox-scratch"} {
			if strings.HasPrefix(e.Name(), prefix) {
				class = c
				break
			}
		}
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
