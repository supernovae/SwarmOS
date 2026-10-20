package agent

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const maxReviewBytes = 1 << 20

// RunReview runs a reviewer in an isolated copy of the worktree. The source
// worktree is never reset, checked out, or otherwise modified. The sole return
// channel is REVIEW.md, which is atomically imported after a successful run.
func RunReview(ctx context.Context, runner Runner, req RunRequest) (*RunResult, error) {
	if req.Role != "reviewer" {
		return nil, fmt.Errorf("RunReview requires reviewer role")
	}
	if req.WorktreePath == "" {
		return nil, fmt.Errorf("review worktree path is required")
	}
	originalWorktree := req.WorktreePath
	temp, err := os.MkdirTemp("", "swarm-review-*")
	if err != nil {
		return nil, fmt.Errorf("create review snapshot: %w", err)
	}
	defer os.RemoveAll(temp)
	temp, err = filepath.EvalSymlinks(temp)
	if err != nil {
		return nil, fmt.Errorf("resolve review snapshot: %w", err)
	}
	snapshot := filepath.Join(temp, "source")
	if err := copySnapshot(originalWorktree, snapshot); err != nil {
		return nil, err
	}
	output := filepath.Join(snapshot, ".swarm", "active-output")
	if err := os.MkdirAll(output, 0o700); err != nil {
		return nil, err
	}
	active := filepath.Join(snapshot, ".swarm", "active")
	if err := os.MkdirAll(active, 0o755); err != nil {
		return nil, err
	}
	reviewInSnapshot := filepath.Join(active, "REVIEW.md")
	_ = os.RemoveAll(reviewInSnapshot)
	if err := os.Symlink(filepath.Join(output, "REVIEW.md"), reviewInSnapshot); err != nil {
		return nil, fmt.Errorf("create review output channel: %w", err)
	}
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		req.DenyPaths = append(req.DenyPaths, originalWorktree, home)
	} else {
		req.DenyPaths = append(req.DenyPaths, originalWorktree)
	}
	req.WorktreePath, req.ReadOnly = snapshot, true
	result, runErr := runner.Run(ctx, req)
	if runErr != nil {
		return result, runErr
	}
	reviewPath := filepath.Join(output, "REVIEW.md")
	info, err := os.Lstat(reviewPath)
	if err != nil {
		return result, fmt.Errorf("reviewer did not produce isolated REVIEW.md: %w", err)
	}
	if !info.Mode().IsRegular() {
		return result, fmt.Errorf("reviewer output REVIEW.md must be a regular file")
	}
	if info.Size() > maxReviewBytes {
		return result, fmt.Errorf("reviewer output REVIEW.md exceeds %d bytes", maxReviewBytes)
	}
	data, err := os.ReadFile(reviewPath)
	if err != nil {
		return result, fmt.Errorf("read isolated reviewer output: %w", err)
	}
	dest := filepath.Join(originalWorktree, ".swarm", "active", "REVIEW.md")
	destDir := filepath.Dir(dest)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return result, err
	}
	resolvedDestDir, err := filepath.EvalSymlinks(destDir)
	if err != nil {
		return result, err
	}
	canonicalOriginal, err := filepath.EvalSymlinks(originalWorktree)
	if err != nil {
		return result, err
	}
	relativeDestDir, err := filepath.Rel(canonicalOriginal, resolvedDestDir)
	if err != nil || relativeDestDir == ".." || strings.HasPrefix(relativeDestDir, ".."+string(os.PathSeparator)) {
		return result, fmt.Errorf("review output destination escapes worktree")
	}
	stage, err := os.CreateTemp(filepath.Dir(dest), ".REVIEW.*")
	if err != nil {
		return result, err
	}
	stageName := stage.Name()
	defer os.Remove(stageName)
	if _, err := stage.Write(data); err != nil {
		stage.Close()
		return result, err
	}
	if err := stage.Chmod(0o644); err != nil {
		stage.Close()
		return result, err
	}
	if err := stage.Close(); err != nil {
		return result, err
	}
	if err := os.Rename(stageName, dest); err != nil {
		return result, err
	}
	return result, nil
}

func copySnapshot(source, target string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == ".git" || len(rel) > 5 && rel[:5] == ".git"+string(os.PathSeparator) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		dest := filepath.Join(target, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if filepath.IsAbs(link) {
				return fmt.Errorf("review snapshot rejects absolute symlink %q", rel)
			}
			resolved, err := filepath.EvalSymlinks(filepath.Join(filepath.Dir(path), link))
			if err != nil {
				return fmt.Errorf("review snapshot rejects unresolved symlink %q: %w", rel, err)
			}
			inside, err := filepath.Rel(source, resolved)
			if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(os.PathSeparator)) {
				return fmt.Errorf("review snapshot rejects escaping symlink %q", rel)
			}
			return os.Symlink(link, dest)
		}
		if entry.IsDir() {
			return os.MkdirAll(dest, info.Mode().Perm())
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}
