// Package gitfixture provides isolated, maintenance-free Git commands for tests.
package gitfixture

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Command constructs a Git command without initializing a repository.
// Commits use the fixed identity Fixture <fixture@example.invalid>.
func Command(t testing.TB, dir string, args ...string) *exec.Cmd {
	t.Helper()
	return CommandContext(t, context.Background(), dir, args...)
}

// CommandContext is Command with cancellation.
func CommandContext(t testing.TB, ctx context.Context, dir string, args ...string) *exec.Cmd {
	t.Helper()
	global := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(global, nil, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "gc.auto=0", "-c", "maintenance.auto=false"}, args...)...)
	cmd.Dir = dir
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_CONFIG_") || strings.HasPrefix(key, "GIT_AUTHOR_") || strings.HasPrefix(key, "GIT_COMMITTER_") {
			continue
		}
		switch key {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES":
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+global, "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
	return cmd
}

// Run returns untrimmed stdout and reports stderr if Git fails.
func Run(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := Command(t, dir, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, &stderr)
	}
	return string(out)
}

// Init initializes dir, allocating a temporary directory when it is empty.
func Init(t testing.TB, dir string, args ...string) string {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	Run(t, dir, append([]string{"init"}, args...)...)
	return dir
}

// Commit commits already staged files and returns HEAD. Options precede the message.
func Commit(t testing.TB, dir, message string, options ...string) string {
	t.Helper()
	args := append([]string{"commit"}, options...)
	Run(t, dir, append(args, "-m", message)...)
	return strings.TrimSpace(Run(t, dir, "rev-parse", "HEAD"))
}
