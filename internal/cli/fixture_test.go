package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/tracker"
)

// Seeds contain only the committed harness, never tracker, lease, or worktree state.
// Keying by basename preserves InitRepository's generated project name.
var cliSeeds = struct {
	sync.Mutex
	roots   map[string]string
	cleanup []string
}{roots: make(map[string]string)}

func materializeCLISeed(t *testing.T, root string) {
	t.Helper()
	cliSeeds.Lock()
	defer cliSeeds.Unlock()
	name := filepath.Base(root)
	seed := cliSeeds.roots[name]
	if seed == "" {
		parent, err := os.MkdirTemp("", "swarm-cli-seed-")
		if err != nil {
			t.Fatal(err)
		}
		cliSeeds.cleanup = append(cliSeeds.cleanup, parent)
		seed = filepath.Join(parent, name)
		if err := os.Mkdir(seed, 0700); err != nil {
			t.Fatal(err)
		}
		initializeCLISeed(t, seed)
		cliSeeds.roots[name] = seed
	}
	// CopyFS copies file contents and modes, including independent Git metadata.
	if err := os.CopyFS(root, os.DirFS(seed)); err != nil {
		t.Fatal(err)
	}
}

func initializeCLISeed(t *testing.T, root string) {
	t.Helper()
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-b", "main")
	git("config", "user.name", "CLI Test")
	git("config", "user.email", "test@example.invalid")
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	policy := "required_checks:\n  - name: git-available\n    command: git --version\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**']\n"
	if err := os.WriteFile(filepath.Join(root, ".swarm", "validation", "required-checks.yaml"), []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "approved harness")
}

func TestCLIFixtureIsolation(t *testing.T) {
	roots := []string{filepath.Join(t.TempDir(), "fixture"), filepath.Join(t.TempDir(), "fixture")}
	for _, root := range roots {
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	a := newCLIFixtureAt(t, roots[0])
	b := newCLIFixtureAt(t, roots[1])
	head := prGit(t, b.root, "rev-parse", "HEAD")
	project := filepath.Join(".swarm", "project.md")
	original, err := os.ReadFile(filepath.Join(b.root, project))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.root, project), []byte("# Changed project\n"), 0600); err != nil {
		t.Fatal(err)
	}
	prGit(t, a.root, "config", "fixture.private", "a")
	prGit(t, a.root, "add", ".")
	prGit(t, a.root, "commit", "-m", "independent mutation")
	n := a.create(t, "agent")
	a.must(t, "issue", "approve", strconv.Itoa(n))
	a.must(t, "issue", "claim", strconv.Itoa(n), "--agent", "mock")
	if state := a.state(t, n); state.Lease.ClaimID == "" {
		t.Fatal("missing claim in first fixture")
	}
	if got := prGit(t, b.root, "rev-parse", "HEAD"); got != head {
		t.Fatal("sibling HEAD changed")
	}
	got, err := os.ReadFile(filepath.Join(b.root, project))
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("sibling content changed: %v", err)
	}
	if out, err := exec.Command("git", "-C", b.root, "config", "--get", "fixture.private").CombinedOutput(); err == nil || len(out) != 0 {
		t.Fatalf("sibling config changed: %s, %v", out, err)
	}
	issues, err := b.tracker.ListIssues(context.Background(), tracker.IssueFilter{State: "open"})
	if err != nil || len(issues) != 0 {
		t.Fatalf("sibling tracker changed: %v %v", issues, err)
	}
	if _, err := b.manager.GetCurrentState(context.Background(), n); !errors.Is(err, coordination.ErrNotFound) {
		t.Fatalf("sibling coordination changed: %v", err)
	}
	for _, f := range []*cliFixture{a, b} {
		// Generated files and Git metadata must not retain the seed's absolute path.
		seed := cliSeeds.roots[filepath.Base(f.root)]
		if err := filepath.WalkDir(f.root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(f.root, path)
			if err != nil {
				return err
			}
			original, err := os.Stat(filepath.Join(seed, rel))
			if err == nil {
				copied, err := entry.Info()
				if err != nil {
					return err
				}
				if os.SameFile(original, copied) {
					t.Errorf("shared file inode: %s", rel)
				}
			} else if !os.IsNotExist(err) {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if bytes.Contains(data, []byte(seed)) {
				t.Errorf("seed path retained in %s", path)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if dir := prGit(t, f.root, "rev-parse", "--absolute-git-dir"); dir != filepath.Join(f.root, ".git") {
			t.Fatalf("external Git metadata: %s", dir)
		}
		if _, err := os.Lstat(filepath.Join(f.root, ".git", "objects", "info", "alternates")); !os.IsNotExist(err) {
			t.Fatalf("shared Git objects: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(f.root, ".git", "commondir")); !os.IsNotExist(err) {
			t.Fatalf("shared Git common directory: %v", err)
		}
	}
}
