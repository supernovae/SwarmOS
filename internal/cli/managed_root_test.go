package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/registry"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workflow"
	"github.com/supernovae/swarmos/internal/workspace"
)

// runManaged executes swarm from dir without the fixture's --root, so the
// command resolves its root the way an interactive shell would. It reads the
// fixture's environment; changing the working directory keeps callers serial.
func runManaged(t *testing.T, f *cliFixture, opts *RootOptions, dir string, args ...string) (string, string, error) {
	t.Helper()
	t.Chdir(dir)
	cmd := f.command(opts)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func mustManaged(t *testing.T, f *cliFixture, dir string, args ...string) (string, string) {
	t.Helper()
	stdout, stderr, err := runManaged(t, f, nil, dir, append([]string{"--backend", "local"}, args...)...)
	if err != nil {
		t.Fatalf("swarm %v in %s: %v\n%s%s", args, dir, err, stdout, stderr)
	}
	return stdout, stderr
}

func canonicalDir(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// managedRootFixture approves two issues and leaves the first one's task
// worktree claimed, with a source subdirectory to start commands from.
func managedRootFixture(t *testing.T) (f *cliFixture, first, second int, firstWorktree string) {
	t.Helper()
	f = newCLIFixture(t)
	first, second = f.create(t, "agent"), f.create(t, "agent")
	f.must(t, "issue", "approve", strconv.Itoa(first))
	f.must(t, "issue", "approve", strconv.Itoa(second))
	f.must(t, "issue", "work", strconv.Itoa(first), "--agent", "mock", "--headless", "--progress=false")
	firstWorktree = workspace.WorktreePath(f.root, first)
	if err := os.MkdirAll(filepath.Join(firstWorktree, "internal", "cli"), 0o755); err != nil {
		t.Fatal(err)
	}
	return f, first, second, firstWorktree
}

func assertRedirectNotice(t *testing.T, stderr string) {
	t.Helper()
	notices := 0
	for _, line := range strings.Split(stderr, "\n") {
		if strings.Contains(line, "is inside a task worktree; using main checkout") {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("want exactly one redirect notice, got %d:\n%s", notices, stderr)
	}
}

func TestManagedRootSecondIssueWork(t *testing.T) {
	for _, mode := range []string{"implicit", "explicit relative", "explicit absolute"} {
		t.Run(mode, func(t *testing.T) {
			f, _, second, firstWorktree := managedRootFixture(t)
			dir, args := filepath.Join(firstWorktree, "internal", "cli"), []string{}
			switch mode {
			case "explicit relative":
				dir, args = f.root, []string{"--root", filepath.Join(".swarm", "worktrees", filepath.Base(firstWorktree), "internal")}
			case "explicit absolute":
				dir, args = t.TempDir(), []string{"--root", firstWorktree}
			}
			args = append(args, "issue", "work", strconv.Itoa(second), "--agent", "mock", "--headless", "--json", "--progress=false")
			stdout, stderr := mustManaged(t, f, dir, args...)
			var outcome struct {
				Status string              `json:"status"`
				Result workflow.WorkResult `json:"result"`
			}
			if err := json.Unmarshal([]byte(stdout), &outcome); err != nil {
				t.Fatalf("stdout is not JSON: %v: %s", err, stdout)
			}
			if outcome.Status != "completed" {
				t.Fatalf("unexpected outcome: %+v", outcome)
			}
			assertRedirectNotice(t, stderr)
			want := workspace.WorktreePath(f.root, second)
			if info, err := os.Stat(want); err != nil || !info.IsDir() {
				t.Fatalf("second worktree missing under main checkout: %v", err)
			}
			if registered := prGit(t, f.root, "worktree", "list", "--porcelain"); !strings.Contains(registered, "worktree "+canonicalDir(t, want)+"\n") {
				t.Fatalf("second worktree not registered at %s:\n%s", want, registered)
			}
			if _, err := os.Lstat(filepath.Join(firstWorktree, ".swarm", "worktrees")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("worktree nested inside the first task: %v", err)
			}
			if st := f.state(t, second); st.Lease.ClaimID == "" {
				t.Fatalf("main checkout state lacks the second claim: %+v", st)
			}
		})
	}
}

func TestManagedRootIssueLifecycle(t *testing.T) {
	f, _, second, firstWorktree := managedRootFixture(t)
	n := strconv.Itoa(second)
	mustManaged(t, f, firstWorktree, "issue", "work", n, "--agent", "mock", "--headless", "--progress=false")
	secondWorktree := workspace.WorktreePath(f.root, second)

	stdout, stderr := mustManaged(t, f, firstWorktree, "issue", "status", n, "--json")
	assertRedirectNotice(t, stderr)
	var status struct {
		Issues []issueStatus `json:"issues"`
	}
	if err := json.Unmarshal([]byte(stdout), &status); err != nil {
		t.Fatalf("%v: %s", err, stdout)
	}
	if len(status.Issues) != 1 || status.Issues[0].Checkpoint == nil || canonicalDir(t, status.Issues[0].Checkpoint.WorktreePath) != canonicalDir(t, secondWorktree) || status.Issues[0].Checkpoint.Worktree.Error != "" {
		t.Fatalf("status did not find the second worktree under main: %s", stdout)
	}
	if _, stderr := mustManaged(t, f, f.root, "issue", "status", n, "--json"); stderr != "" {
		t.Fatalf("main checkout printed a notice: %s", stderr)
	}

	mustManaged(t, f, firstWorktree, "issue", "submit", n)
	submitted := f.state(t, second)
	if submitted.PRNumber == 0 || submitted.Lease.Status != "completed" {
		t.Fatalf("submission not recorded in main checkout state: %+v", submitted)
	}
	mustManaged(t, f, filepath.Join(firstWorktree, "internal"), "issue", "submit", n)
	if retry := f.state(t, second); retry.PRNumber != submitted.PRNumber || retry.HeadSHA != submitted.HeadSHA {
		t.Fatal("retry changed the submission identity")
	}

	seedLocalCleanupMergedPR(t, f, submitted.PRNumber)
	// The checkout a command runs in is never retired, even after redirection.
	stdout, _ = mustManaged(t, f, secondWorktree, "issue", "cleanup", n, "--json")
	if !strings.Contains(stdout, "invoking checkout is retained") {
		t.Fatalf("cleanup from the second worktree did not retain it: %s", stdout)
	}
	mustManaged(t, f, firstWorktree, "issue", "cleanup", n, "--json")
	stdout, _ = mustManaged(t, f, firstWorktree, "issue", "cleanup", n, "--apply", "--json")
	if _, err := os.Stat(secondWorktree); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("second worktree survives cleanup: %v\n%s", err, stdout)
	}
	registered := prGit(t, f.root, "worktree", "list", "--porcelain")
	if strings.Contains(registered, canonicalDir(t, f.root)+"/.swarm/worktrees/issue-"+n+"\n") {
		t.Fatalf("second worktree still registered:\n%s", registered)
	}
	if _, err := os.Stat(firstWorktree); err != nil {
		t.Fatalf("first worktree was removed: %v", err)
	}
}

// snapshotTree records every path, size and mode under the roots.
func snapshotTree(t *testing.T, roots ...string) map[string]string {
	t.Helper()
	snap := make(map[string]string)
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			snap[path] = info.Mode().String() + " " + strconv.FormatInt(info.Size(), 10) + " " + info.ModTime().String()
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return snap
}

func TestManagedRootRefusesBeforeWrites(t *testing.T) {
	assertRefusals := func(t *testing.T, f *cliFixture, dir string, issue int, roots ...string) {
		t.Helper()
		n := strconv.Itoa(issue)
		for _, args := range [][]string{
			{"issue", "work", n, "--agent", "mock", "--headless", "--progress=false"},
			{"issue", "submit", n},
			{"issue", "cleanup", n, "--apply", "--json"},
			{"issue", "status", n},
		} {
			before := snapshotTree(t, roots...)
			stdout, stderr, err := runManaged(t, f, nil, dir, append([]string{"--backend", "local"}, args...)...)
			if err == nil {
				t.Fatalf("swarm %v succeeded without a main checkout: %s", args, stdout)
			}
			for _, want := range []string{"cannot determine the main checkout", "cd to the main checkout", "--root"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("swarm %v error %q does not name %q", args, err, want)
				}
			}
			if stdout != "" || stderr != "" {
				t.Fatalf("swarm %v wrote output before refusing: %q %q", args, stdout, stderr)
			}
			after := snapshotTree(t, roots...)
			for path, entry := range after {
				if before[path] != entry {
					t.Fatalf("swarm %v changed %s", args, path)
				}
			}
			if len(before) != len(after) {
				t.Fatalf("swarm %v removed paths", args)
			}
		}
	}

	t.Run("main Git metadata moved", func(t *testing.T) {
		f, _, second, firstWorktree := managedRootFixture(t)
		moved := filepath.Join(t.TempDir(), "moved.git")
		if err := os.Rename(filepath.Join(f.root, ".git"), moved); err != nil {
			t.Fatal(err)
		}
		assertRefusals(t, f, filepath.Join(firstWorktree, "internal"), second, f.root, moved)
	})
	t.Run("enclosing checkout is another repository", func(t *testing.T) {
		f, _, second, _ := managedRootFixture(t)
		other := t.TempDir()
		prGit(t, other, "init", "-b", "main")
		prGit(t, other, "-c", "user.name=T", "-c", "user.email=t@example.invalid", "commit", "--allow-empty", "-m", "other")
		stray := filepath.Join(other, ".swarm", "worktrees", "issue-99")
		prGit(t, f.root, "worktree", "add", "-b", "stray", stray)
		assertRefusals(t, f, stray, second, f.root, other)
		// Naming the verified main checkout explicitly still works.
		stdout, _, err := runManaged(t, f, nil, stray, "--backend", "local", "--root", f.root, "issue", "status", strconv.Itoa(second), "--json")
		if err != nil || !strings.Contains(stdout, `"kind": "status"`) && !strings.Contains(stdout, `"kind":"status"`) {
			t.Fatalf("explicit main --root: %v %s", err, stdout)
		}
	})
}

func TestManagedRootBackendRouting(t *testing.T) {
	f, first, _, firstWorktree := managedRootFixture(t)
	var roots []string
	opts := &RootOptions{
		Backend:           "github",
		githubTrackerFunc: func(string) (tracker.Tracker, error) { return f.tracker, nil },
		remoteSlugFunc: func(root, remote string) string {
			roots = append(roots, root)
			return "acme/widgets"
		},
	}
	stdout, stderr, err := runManaged(t, f, opts, firstWorktree, "issue", "status", strconv.Itoa(first), "--json")
	if err != nil {
		t.Fatalf("%v\n%s%s", err, stdout, stderr)
	}
	assertRedirectNotice(t, stderr)
	if len(roots) == 0 {
		t.Fatal("GitHub repository identity was never resolved")
	}
	for _, root := range roots {
		if canonicalDir(t, root) != canonicalDir(t, f.root) {
			t.Fatalf("GitHub identity resolved from %s, want main checkout %s", root, f.root)
		}
	}
}

func TestManagedRootRegistryTargets(t *testing.T) {
	f, first, _, firstWorktree := managedRootFixture(t)
	opts := f.options(&RootOptions{Backend: "local"})
	reg := registry.New(opts.registryPath())
	if _, err := reg.Add("local/main", firstWorktree, "local"); err != nil {
		t.Fatal(err)
	}
	if _, err := opts.resolveTargets(true); err != nil {
		t.Fatal(err)
	}
	targets, _ := opts.resolveTargets(true)
	if len(targets) != 1 || canonicalDir(t, targets[0].Path) != canonicalDir(t, f.root) {
		t.Fatalf("registry target not normalized: %+v", targets)
	}
	stdout, _ := mustManaged(t, f, t.TempDir(), "issue", "status", "--all", "--json")
	if !strings.Contains(stdout, `"number": `+strconv.Itoa(first)) && !strings.Contains(stdout, `"number":`+strconv.Itoa(first)) {
		t.Fatalf("status --all did not read main checkout state: %s", stdout)
	}

	other := t.TempDir()
	prGit(t, other, "init", "-b", "main")
	prGit(t, other, "-c", "user.name=T", "-c", "user.email=t@example.invalid", "commit", "--allow-empty", "-m", "other")
	stray := filepath.Join(other, ".swarm", "worktrees", "issue-99")
	prGit(t, f.root, "worktree", "add", "-b", "stray", stray)
	if _, err := reg.Add("local/stray", stray, "local"); err != nil {
		t.Fatal(err)
	}
	if _, err := opts.resolveTargets(true); err == nil || !strings.Contains(err.Error(), "cannot determine the main checkout") {
		t.Fatalf("unknown main checkout in registry was not refused: %v", err)
	}
}

func TestManagedRootNonCoordinatorCommands(t *testing.T) {
	f, _, _, firstWorktree := managedRootFixture(t)
	if err := os.Rename(filepath.Join(f.root, ".git"), filepath.Join(t.TempDir(), "moved.git")); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"version"}, {"help"}, {"completion", "bash"}} {
		if stdout, stderr, err := runManaged(t, f, nil, firstWorktree, args...); err != nil {
			t.Fatalf("swarm %v probed the repository: %v %s%s", args, err, stdout, stderr)
		}
	}
}

func TestManagedRootIssueArgsFromInvokingWorktree(t *testing.T) {
	t.Parallel()
	f, first, _, firstWorktree := managedRootFixture(t)
	opts := f.options(&RootOptions{RepoRoot: firstWorktree})
	cmd := newRootCmdWithOptions(opts)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--backend", "local", "issue", "status", strconv.Itoa(first)})
	cmd.SetOut(new(bytes.Buffer))
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if canonicalDir(t, opts.RepoRoot) != canonicalDir(t, f.root) {
		t.Fatalf("RepoRoot = %s, want %s", opts.RepoRoot, f.root)
	}
	// The redirected root no longer holds the bundle; the invoking worktree does.
	if got := opts.issueArgs(nil); len(got) != 1 || got[0] != strconv.Itoa(first) {
		t.Fatalf("issueArgs = %v, want [%d]", got, first)
	}
	if got := opts.issueArgs([]string{"7"}); len(got) != 1 || got[0] != "7" {
		t.Fatalf("explicit issue replaced: %v", got)
	}
}
