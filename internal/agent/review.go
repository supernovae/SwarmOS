package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/safefs"
)

const maxReviewBytes = 1 << 20

// Snapshot limits apply before a reviewer starts, including to untracked files.
// Count directories as well as files so empty trees cannot exhaust resources.
type snapshotLimits struct {
	entries               int
	fileBytes, totalBytes int64
	duration              time.Duration
}

var defaultSnapshotLimits = snapshotLimits{entries: 100000, fileBytes: 64 << 20, totalBytes: 512 << 20, duration: 2 * time.Minute}

// RunReview runs a reviewer in an isolated copy of the worktree. The source
// worktree is never reset, checked out, or otherwise modified. The sole return
// channel is REVIEW.md, which is atomically imported after a successful run.
// Runner.Run must terminate its execution boundary before returning success.
func RunReview(ctx context.Context, runner Runner, req RunRequest) (result *RunResult, err error) {
	if req.Role != "reviewer" {
		return nil, fmt.Errorf("RunReview requires reviewer role")
	}
	if req.WorktreePath == "" {
		return nil, fmt.Errorf("review worktree path is required")
	}
	originalWorktree := req.WorktreePath
	req.RecoveryRoot = originalWorktree
	destination, err := safefs.Open(originalWorktree)
	if err != nil {
		return nil, err
	}
	defer destination.Close()
	temp, err := os.MkdirTemp("", "swarm-review-*")
	if err != nil {
		return nil, fmt.Errorf("create review snapshot: %w", err)
	}
	defer func() {
		if cleanupErr := os.RemoveAll(temp); cleanupErr != nil && err == nil {
			err = fmt.Errorf("clean up review snapshot: %w", cleanupErr)
		}
	}()
	temp, err = filepath.EvalSymlinks(temp)
	if err != nil {
		return nil, fmt.Errorf("resolve review snapshot: %w", err)
	}
	snapshot := filepath.Join(temp, "source")
	if err := copySnapshotContext(ctx, originalWorktree, snapshot, defaultSnapshotLimits); err != nil {
		return nil, err
	}
	output := filepath.Join(snapshot, ".swarm", "active-output")
	// Never reuse candidate-controlled output directories or instruction paths.
	if err := os.RemoveAll(output); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(output, 0o700); err != nil {
		return nil, err
	}
	outputRoot, err := safefs.Open(output)
	if err != nil {
		return nil, err
	}
	defer outputRoot.Close()
	active := filepath.Join(snapshot, ".swarm", "active")
	if err := os.MkdirAll(active, 0o755); err != nil {
		return nil, err
	}
	reviewInSnapshot := filepath.Join(active, "REVIEW.md")
	if err := os.RemoveAll(reviewInSnapshot); err != nil {
		return nil, err
	}
	if err := os.Symlink(filepath.Join(output, "REVIEW.md"), reviewInSnapshot); err != nil {
		return nil, fmt.Errorf("create review output channel: %w", err)
	}
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		req.DenyPaths = append(req.DenyPaths, originalWorktree, home)
	} else {
		req.DenyPaths = append(req.DenyPaths, originalWorktree)
	}
	req.WorktreePath, req.ReadOnly = snapshot, true
	result, err = runner.Run(ctx, req)
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	data, err := outputRoot.ReadFile("REVIEW.md", maxReviewBytes)
	if err != nil {
		return result, fmt.Errorf("read isolated reviewer output: %w", err)
	}
	if err := destination.WriteFile(filepath.Join(".swarm", "active", "REVIEW.md"), data, 0o644); err != nil {
		return result, fmt.Errorf("import isolated reviewer output: %w", err)
	}
	return result, nil
}

func isEphemeralReviewDir(name string) bool {
	switch name {
	case "node_modules", ".venv", "venv", "__pycache__", ".pytest_cache", ".mypy_cache", ".ruff_cache", ".tox", ".nox", ".gradle", ".next", ".nuxt", ".turbo", ".parcel-cache":
		return true
	default:
		return false
	}
}

func copySnapshot(source, target string) error {
	return copySnapshotContext(context.Background(), source, target, defaultSnapshotLimits)
}

func copySnapshotContext(ctx context.Context, source, target string, limits snapshotLimits) error {
	ctx, cancel := context.WithTimeout(ctx, limits.duration)
	defer cancel()
	root, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer root.Close()
	files, err := safefs.Open(source)
	if err != nil {
		return err
	}
	defer files.Close()
	if err := os.Mkdir(target, 0o700); err != nil {
		return err
	}
	count := 0
	var links []string
	var copied int64
	var walk func(string) error
	walk = func(dir string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if strings.Count(dir, string(filepath.Separator)) >= 64 {
			return fmt.Errorf("review snapshot exceeds maximum directory depth")
		}
		in, err := files.OpenDirectory(dir)
		if err != nil {
			return err
		}
		defer in.Close()
		for {
			entries, readErr := in.ReadDir(128)
			if readErr != nil && readErr != io.EOF {
				return readErr
			}
			for _, entry := range entries {
				if err := ctx.Err(); err != nil {
					return err
				}
				rel := filepath.Join(dir, entry.Name())
				if rel == ".git" || rel == filepath.Join(".swarm", "active-output") && entry.IsDir() || (entry.IsDir() && isEphemeralReviewDir(entry.Name())) {
					continue
				}
				count++
				if count > limits.entries {
					return fmt.Errorf("review snapshot exceeds %d entries", limits.entries)
				}
				info, err := root.Lstat(rel)
				if err != nil {
					return err
				}
				dest := filepath.Join(target, rel)
				if info.Mode()&os.ModeSymlink != 0 {
					if rel == ".swarm" || rel == filepath.Join(".swarm", "active") || rel == filepath.Join(".swarm", "active-output") {
						return fmt.Errorf("review snapshot rejects internal directory symlink %q", rel)
					}
					link, err := root.Readlink(rel)
					if err != nil {
						return err
					}
					if filepath.IsAbs(link) {
						return fmt.Errorf("review snapshot rejects absolute symlink %q", rel)
					}
					// Root.Stat resolves the complete link chain inside the source root.
					if _, err := root.Stat(filepath.Join(filepath.Dir(rel), link)); err != nil {
						return fmt.Errorf("review snapshot rejects unsafe symlink %q: %w", rel, err)
					}
					if err := os.Symlink(link, dest); err != nil {
						return err
					}
					links = append(links, rel)
					continue
				}
				if info.IsDir() {
					if err := os.Mkdir(dest, 0o700); err != nil {
						return err
					}
					if err := walk(rel); err != nil {
						return err
					}
					continue
				}
				if !info.Mode().IsRegular() {
					return fmt.Errorf("review snapshot rejects special file %q", rel)
				}
				file, err := files.OpenRegular(rel)
				if err != nil {
					return err
				}
				budget := min(limits.fileBytes, limits.totalBytes-copied)
				n, err := copySnapshotFile(ctx, file, dest, info.Mode().Perm(), budget)
				closeErr := file.Close()
				if err != nil {
					return fmt.Errorf("review snapshot file %q: %w", rel, err)
				}
				if closeErr != nil {
					return closeErr
				}
				copied += n
			}
			if readErr == io.EOF {
				return nil
			}
		}
	}
	if err := walk("."); err != nil {
		return err
	}
	// Validate the completed graph as well: concurrent source changes could
	// otherwise combine individually safe links into an escaping chain.
	copiedRoot, err := os.OpenRoot(target)
	if err != nil {
		return err
	}
	defer copiedRoot.Close()
	for _, name := range links {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := copiedRoot.Stat(name); err != nil {
			return fmt.Errorf("review snapshot contains unsafe copied link %q: %w", name, err)
		}
	}
	return nil
}

func copySnapshotFile(ctx context.Context, in *os.File, target string, mode os.FileMode, maxBytes int64) (int64, error) {
	info, err := in.Stat()
	if err != nil {
		return 0, err
	}
	if maxBytes < 0 || info.Size() > maxBytes {
		return 0, fmt.Errorf("snapshot byte limit exceeded")
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	reader := &contextReader{ctx: ctx, reader: in}
	n, err := io.Copy(out, io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return n, err
	}
	if n > maxBytes {
		return n, fmt.Errorf("snapshot byte limit exceeded")
	}
	after, err := in.Stat()
	if err != nil {
		return n, err
	}
	if !os.SameFile(info, after) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) || n != info.Size() {
		return n, fmt.Errorf("snapshot source changed while copying")
	}
	return n, out.Close()
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
