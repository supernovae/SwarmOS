package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"github.com/supernovae/swarmos/internal/tracker"
)

type cliFixture struct {
	root    string
	tracker *tracker.LocalTracker
	manager *lease.Manager
	env     *fixtureEnv
	// resolverRunner replaces PR work and conflict resolver runners for this
	// fixture's later commands.
	resolverRunner func(name string, cfg *contract.SwarmConfig, local bool) (agent.Runner, error)
}

// fixtureEnv is the environment one fixture's commands see in place of the
// process environment. Names it does not hold read the process environment,
// whose user directories TestMain points at a temporary root.
type fixtureEnv struct {
	mu   sync.Mutex
	vars map[string]*string
}

// newFixtureEnv owns execution mode, user directories and profile paths, and
// clears the SWARM_* selectors and mock controls a host might have set.
func newFixtureEnv(t *testing.T) *fixtureEnv {
	t.Helper()
	e := &fixtureEnv{vars: map[string]*string{}}
	// Fixtures intentionally run local mock agents and validation.
	e.set("SWARM_EXECUTION_MODE", "trusted-local")
	for _, name := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "APPDATA"} {
		e.set(name, t.TempDir())
	}
	e.set("SWARM_REGISTRY_PATH", filepath.Join(t.TempDir(), "repos.yaml"))
	e.set("SWARM_NODE_CONFIG", filepath.Join(t.TempDir(), "node.yaml"))
	for _, name := range []string{"SWARM_BACKEND", "SWARM_REPO", "SWARM_REPOS_DIR", "SWARM_MOCK_VERDICT", "SWARM_MOCK_DEVELOPER_CMD"} {
		e.unset(name)
	}
	return e
}

func (e *fixtureEnv) set(name, value string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.vars[name] = &value
}

func (e *fixtureEnv) unset(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.vars[name] = nil
}

// lookup returns a snapshot, so later set calls never change a command that
// is already built.
func (e *fixtureEnv) lookup() func(string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	vars := make(map[string]*string, len(e.vars))
	for name, value := range e.vars {
		vars[name] = value
	}
	return func(name string) (string, bool) {
		if value, held := vars[name]; held {
			if value == nil {
				return "", false
			}
			return *value, true
		}
		return os.LookupEnv(name)
	}
}

func newCLIFixture(t *testing.T) *cliFixture {
	t.Helper()
	env := newFixtureEnv(t)
	env.set("SWARM_USER", "alice")
	root := t.TempDir()
	built := false
	if err := pristineCLIFixture.populate(root, func(seed string) error {
		built = true
		return initializeCLIFixture(t, seed)
	}); err != nil {
		t.Fatal(err)
	}
	if !built {
		// Original setup issues five isolated Git commands, each allocating a
		// TempDir. Preserve later root basenames for multiple fixtures per test.
		for range 5 {
			t.TempDir()
		}
	}
	tr := tracker.NewLocalTracker(root).WithUser("alice", "")
	cfg, err := harness.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	manager := lease.NewManager(tr, cfg.Security)
	if err := manager.ConfigureStore(root); err != nil {
		t.Fatal(err)
	}
	return &cliFixture{root: root, tracker: tr, manager: manager, env: env}
}

// setenv changes a variable for this fixture's later commands only.
func (f *cliFixture) setenv(name, value string) { f.env.set(name, value) }

// options gives opts (nil for defaults) this fixture's environment and
// resolver runner in place of the process environment.
func (f *cliFixture) options(opts *RootOptions) *RootOptions {
	if opts == nil {
		opts = &RootOptions{}
	}
	opts.lookupEnv = f.env.lookup()
	opts.resolverRunner = f.resolverRunner
	return opts
}

// command builds a root command from opts (nil for defaults) that reads this
// fixture's environment.
func (f *cliFixture) command(opts *RootOptions) *cobra.Command {
	return newRootCmdWithOptions(f.options(opts))
}

func (f *cliFixture) run(args ...string) (string, error) {
	cmd := f.command(nil)
	var output, diagnostics bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&diagnostics)
	cmd.SetArgs(append([]string{"--root", f.root, "--backend", "local"}, args...))
	err := cmd.Execute()
	if err != nil {
		output.Write(diagnostics.Bytes())
	}
	return output.String(), err
}
func (f *cliFixture) must(t *testing.T, args ...string) string {
	t.Helper()
	out, err := f.run(args...)
	if err != nil {
		t.Fatalf("swarm %v: %v\n%s", args, err, out)
	}
	return out
}
func (f *cliFixture) create(t *testing.T, gate string) int {
	t.Helper()
	f.must(t, "issue", "create", "--title", "Approved task", "--objective", "Exercise the lifecycle", "--constraint", "Keep policy unchanged", "--acceptance", "Produce a checked checkpoint", "--gate", gate)
	issues, err := f.tracker.ListIssues(context.Background(), tracker.IssueFilter{State: "open"})
	if err != nil {
		t.Fatal(err)
	}
	number := 0
	for _, issue := range issues {
		if issue.Number > number {
			number = issue.Number
		}
	}
	return number
}
func (f *cliFixture) state(t *testing.T, issue int) *lease.IssueState {
	t.Helper()
	state, err := f.manager.GetCurrentState(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func initializeCLIFixture(t *testing.T, root string) error {
	git := func(args ...string) error {
		if out, err := gitfixture.Command(t, "", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %w: %s", args, err, out)
		}
		return nil
	}
	if err := git("init", "-b", "main"); err != nil {
		return err
	}
	if err := git("config", "user.name", "CLI Test"); err != nil {
		return err
	}
	if err := git("config", "user.email", "test@example.invalid"); err != nil {
		return err
	}
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: root}); err != nil {
		return err
	}
	policy := "required_checks:\n  - name: git-available\n    command: git --version\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**']\n"
	if err := os.WriteFile(filepath.Join(root, ".swarm", "validation", "required-checks.yaml"), []byte(policy), 0o644); err != nil {
		return err
	}
	if err := git("add", "."); err != nil {
		return err
	}
	if err := git("commit", "-m", "approved harness"); err != nil {
		return err
	}
	return nil
}

// cliFixtureSeed publishes one complete tree. Initialization errors are sticky;
// no caller's testing.T, cleanup, or initializer is retained after populate.
type cliFixtureSeed struct {
	mu       sync.Mutex
	selected bool
	key      string
	root     string
	err      error
}

var pristineCLIFixture cliFixtureSeed

func cliFixtureInputKey(root string) (string, bool, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", false, err
	}
	if len(entries) != 0 {
		return "", false, nil
	}
	// Custom templates can inject hooks, links and path-bound configuration.
	if os.Getenv("GIT_TEMPLATE_DIR") != "" {
		return "", false, nil
	}
	// Scaffold repository-name discovery reads Git configuration. Includes and
	// includeIf can depend on the absolute directory, so use original setup
	// whenever external configuration could influence that discovery.
	for _, path := range []string{
		filepath.Join(os.Getenv("HOME"), ".gitconfig"),
		filepath.Join(os.Getenv("HOME"), ".config", "git", "config"),
		filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "git", "config"),
		"/etc/gitconfig",
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			return "", false, nil
		}
	}
	// Mock controls affect agent execution, not the empty-root scaffold. In
	// particular #303 sets SWARM_MOCK_PLAN_APPEND before constructing a fixture.
	// Keep those process values visible to commands without partitioning seeds.
	var env []string
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if !strings.HasPrefix(name, "SWARM_MOCK_") {
			env = append(env, value)
		}
	}
	sort.Strings(env)
	return filepath.Base(root) + "\x00" + strings.Join(env, "\x00"), true, nil
}

func (s *cliFixtureSeed) populate(root string, initialize func(string) error) error {
	key, eligible, err := cliFixtureInputKey(root)
	if err != nil {
		return err
	}
	if !eligible {
		return initialize(root)
	}
	s.mu.Lock()
	if s.selected && s.key != key {
		s.mu.Unlock()
		return initialize(root)
	}
	if !s.selected {
		s.selected, s.key = true, key
		parent, err := os.MkdirTemp(os.Getenv("XDG_CACHE_HOME"), "cli-pristine-*")
		if err == nil {
			seed := filepath.Join(parent, filepath.Base(root))
			err = os.Mkdir(seed, 0700)
			if err == nil {
				err = initialize(seed)
			}
			if err == nil {
				err = inspectCLISeed(seed)
			}
			if err == nil {
				s.root = seed
			} else {
				os.RemoveAll(parent)
			}
		}
		s.err = err
	}
	seed, initErr := s.root, s.err
	s.mu.Unlock()
	if initErr != nil {
		return fmt.Errorf("initialize pristine CLI fixture: %w", initErr)
	}
	if err := copyCLISeed(root, seed); err != nil {
		return fmt.Errorf("copy pristine CLI fixture: %w", err)
	}
	return nil
}

func inspectCLISeed(root string) error {
	for _, rel := range []string{".git/objects/info/alternates", ".git/worktrees", ".git/commondir", ".git/index.lock", ".swarm/state", ".swarm/local", ".swarm/local-tracker", ".swarm/coordination", ".swarm/claims", ".git/refs/heads/swarm-state"} {
		if _, err := os.Lstat(filepath.Join(root, rel)); err == nil {
			return fmt.Errorf("unexpected pristine state: %s", rel)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unexpected pristine file type: %s", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(root)) {
			return fmt.Errorf("path-bound pristine file: %s", path)
		}
		return nil
	})
}

func copyCLISeed(dst, src string) error {
	entries, err := os.ReadDir(dst)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("CLI fixture destination is not empty")
	}
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.Mkdir(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unexpected pristine file type: %s", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// O_EXCL prevents accidental replacement and every file gets new storage.
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, writeErr := f.Write(data)
		closeErr := f.Close()
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	})
}

// cliTree records exact bytes and modes without normalizing generated content.
func cliTree(t *testing.T, root string, git bool) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == ".git" && !git {
			return filepath.SkipDir
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			tree[rel] = info.Mode().String()
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		tree[rel] = info.Mode().String() + ":" + string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func cliNamedRoot(t *testing.T, name string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func assertCLIEquivalent(t *testing.T, copied, original string) {
	t.Helper()
	if !reflect.DeepEqual(cliTree(t, copied, false), cliTree(t, original, false)) {
		t.Fatal("generated trees differ in paths, modes or bytes")
	}
	for _, args := range [][]string{
		{"rev-parse", "HEAD^{tree}"}, {"ls-files", "--stage"}, {"status", "--porcelain"},
		{"branch", "--show-current"}, {"config", "--local", "--list"},
		{"log", "-1", "--format=%an <%ae> %cn <%ce>"}, {"remote", "-v"},
	} {
		a, b := gitfixture.Run(t, copied, args...), gitfixture.Run(t, original, args...)
		if a != b {
			t.Fatalf("git %v differs: %q != %q", args, a, b)
		}
	}
}

func TestCLIFixtureSeedEquivalenceAndFallback(t *testing.T) {
	var seed cliFixtureSeed
	init := func(root string) error { return initializeCLIFixture(t, root) }
	for _, name := range []string{"008", "008", "other"} {
		copied, original := cliNamedRoot(t, name), cliNamedRoot(t, name)
		if err := seed.populate(copied, init); err != nil {
			t.Fatal(err)
		}
		if err := init(original); err != nil {
			t.Fatal(err)
		}
		assertCLIEquivalent(t, copied, original)
	}
	// Nonempty roots are ineligible, preserving stack detection and existing files.
	copied, original := cliNamedRoot(t, "008"), cliNamedRoot(t, "008")
	for _, root := range []string{copied, original} {
		if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.invalid/fixture\n\ngo 1.26\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := seed.populate(copied, init); err != nil {
		t.Fatal(err)
	}
	if err := init(original); err != nil {
		t.Fatal(err)
	}
	assertCLIEquivalent(t, copied, original)
}

func TestCLIFixtureSeedErrors(t *testing.T) {
	var seed cliFixtureSeed
	sentinel := fmt.Errorf("seed initialization failed")
	calls := 0
	for range 2 {
		err := seed.populate(cliNamedRoot(t, "008"), func(root string) error {
			calls++
			if err := os.WriteFile(filepath.Join(root, "partial"), []byte("partial"), 0600); err != nil {
				return err
			}
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("initialization error lost: %v", err)
		}
	}
	if calls != 1 || seed.root != "" {
		t.Fatal("partial seed published or initialization retried")
	}
	src := cliNamedRoot(t, "source")
	if err := os.Symlink("missing", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := copyCLISeed(cliNamedRoot(t, "destination"), src); err == nil {
		t.Fatal("copy accepted a symlink")
	}
	if err := inspectCLISeed(src); err == nil {
		t.Fatal("seed accepted a symlink")
	}
	if err := os.Remove(filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "bound"), []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	if err := inspectCLISeed(src); err == nil {
		t.Fatal("seed accepted an absolute seed path")
	}
}

func TestCLIFixtureSeedLifetimeAndIsolation(t *testing.T) {
	var seed cliFixtureSeed
	var first string
	t.Run("first caller", func(t *testing.T) {
		first = cliNamedRoot(t, "008")
		if err := seed.populate(first, func(root string) error { return initializeCLIFixture(t, root) }); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("first fixture survived cleanup: %v", err)
	}
	pristine := cliTree(t, seed.root, true)
	t.Run("concurrent copies", func(t *testing.T) {
		for i := range 6 {
			t.Run(fmt.Sprint(i), func(t *testing.T) {
				t.Parallel()
				name := "008"
				if i%2 != 0 {
					name = "different"
				}
				root := cliNamedRoot(t, name)
				if err := seed.populate(root, func(root string) error { return initializeCLIFixture(t, root) }); err != nil {
					t.Fatal(err)
				}
				if name == "008" {
					if !reflect.DeepEqual(pristine, cliTree(t, root, true)) {
						t.Fatal("copy differs from seed")
					}
					for rel := range pristine {
						a, err := os.Stat(filepath.Join(root, rel))
						if err != nil {
							t.Fatal(err)
						}
						b, err := os.Stat(filepath.Join(seed.root, rel))
						if err != nil {
							t.Fatal(err)
						}
						if a.Mode().IsRegular() && os.SameFile(a, b) {
							t.Fatalf("shared inode: %s", rel)
						}
					}
				}
				if err := inspectCLISeed(root); err != nil {
					t.Fatal(err)
				}
				gitfixture.Run(t, root, "config", "user.name", "mutated")
				gitfixture.Run(t, root, "update-ref", "refs/heads/independent", "HEAD")
				if err := os.WriteFile(filepath.Join(root, ".swarm", "project.md"), []byte("mutated"), 0644); err != nil {
					t.Fatal(err)
				}
				gitfixture.Run(t, root, "add", ".")
				gitfixture.Run(t, root, "commit", "-m", "independent")
				tr := tracker.NewLocalTracker(root).WithUser("alice", "")
				if _, err := tr.CreateIssue(context.Background(), "private", "private", nil); err != nil {
					t.Fatal(err)
				}
			})
		}
	})
	if !reflect.DeepEqual(pristine, cliTree(t, seed.root, true)) {
		t.Fatal("concurrent mutations changed seed")
	}
	later := cliNamedRoot(t, "008")
	if err := seed.populate(later, func(string) error { t.Fatal("seed initialized twice"); return nil }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pristine, cliTree(t, later, true)) {
		t.Fatal("later copy changed")
	}
}

func TestCLIFixtureAllocationOrder(t *testing.T) {
	first := newCLIFixture(t)
	second := newCLIFixture(t)
	if filepath.Base(first.root) != "008" || filepath.Base(second.root) != "021" {
		t.Fatalf("fixture allocation order changed: %s, %s", first.root, second.root)
	}
	if first.manager.Store() == second.manager.Store() {
		t.Fatal("fixture stores shared")
	}
	if first.manager == second.manager || first.tracker == second.tracker || first.env == second.env {
		t.Fatal("mutable fixture plumbing shared")
	}
	number := first.create(t, "agent")
	first.must(t, "issue", "approve", fmt.Sprint(number))
	first.must(t, "issue", "claim", fmt.Sprint(number), "--agent", "mock")
	if _, err := second.tracker.GetIssue(context.Background(), number); err == nil {
		t.Fatal("tracker state shared")
	}
	if _, _, err := second.manager.Store().Load(context.Background(), number); !errors.Is(err, coordination.ErrNotFound) {
		t.Fatalf("coordination state shared: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(second.root, ".swarm", "validation", "required-checks.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first.root, ".swarm", "validation", "required-checks.yaml"), []byte("private policy"), 0644); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(second.root, ".swarm", "validation", "required-checks.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("policy state shared")
	}
	first.setenv("SWARM_USER", "changed")
	if value, _ := second.env.lookup()("SWARM_USER"); value != "alice" {
		t.Fatal("fixture environments shared")
	}
}

func TestCLIFixtureConcurrentPublication(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			var seed cliFixtureSeed
			var calls atomic.Int32
			sentinel := errors.New("initialization rejected")
			t.Run("callers", func(t *testing.T) {
				for i := range 8 {
					t.Run(fmt.Sprint(i), func(t *testing.T) {
						t.Parallel()
						root := cliNamedRoot(t, "008")
						err := seed.populate(root, func(root string) error {
							calls.Add(1)
							if fail {
								return sentinel
							}
							return initializeCLIFixture(t, root)
						})
						if fail {
							if !errors.Is(err, sentinel) {
								t.Fatalf("lost initialization error: %v", err)
							}
						} else {
							if err != nil {
								t.Fatal(err)
							}
							if status := gitfixture.Run(t, root, "status", "--porcelain"); status != "" {
								t.Fatalf("partial publication: %s", status)
							}
						}
					})
				}
			})
			if calls.Load() != 1 {
				t.Fatalf("initialized %d times", calls.Load())
			}
			if !fail {
				// A lost seed is a copy error, never permission to initialize again.
				if err := os.RemoveAll(seed.root); err != nil {
					t.Fatal(err)
				}
				if err := seed.populate(cliNamedRoot(t, "008"), func(string) error { t.Fatal("copy error fell back"); return nil }); err == nil {
					t.Fatal("copy failure hidden")
				}
			}
		})
	}
}

func TestCLIFixtureManagedDigests(t *testing.T) {
	f := newCLIFixture(t)
	read := func(rel string) []byte {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(f.root, rel))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	project := read(".swarm/project.md")
	if !bytes.HasPrefix(project, []byte("# Project: "+filepath.Base(f.root)+"\n")) {
		t.Fatal("root-derived project name changed")
	}
	var manifest struct {
		Files        map[string]string `json:"files"`
		SwarmVersion string            `json:"swarm_version"`
	}
	if err := json.Unmarshal(read(harness.ManifestPath), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SwarmVersion != "" {
		t.Fatal("unexpected version pin")
	}
	for rel, digest := range manifest.Files {
		actual := fmt.Sprintf("%x", sha256.Sum256(read(rel)))
		if rel == ".swarm/validation/required-checks.yaml" {
			if actual == digest {
				t.Fatal("policy overwrite unexpectedly refreshed manifest")
			}
		} else if actual != digest {
			t.Fatalf("managed digest changed: %s", rel)
		}
	}
	if got := gitfixture.Run(t, f.root, "log", "-1", "--format=%an <%ae> %cn <%ce>"); got != "Fixture <fixture@example.invalid> Fixture <fixture@example.invalid>\n" {
		t.Fatalf("commit identity changed: %s", got)
	}
}

func TestCLIFixtureMockControlsPreserveSeedEligibility(t *testing.T) {
	root := cliNamedRoot(t, "008")
	before, eligible, err := cliFixtureInputKey(root)
	if err != nil || !eligible {
		t.Fatalf("empty root ineligible: %v", err)
	}
	t.Setenv("SWARM_MOCK_PLAN_APPEND", "process value")
	after, eligible, err := cliFixtureInputKey(root)
	if err != nil || !eligible || before != after {
		t.Fatalf("mock control changed generation key: %v", err)
	}
	if value, _ := newFixtureEnv(t).lookup()("SWARM_MOCK_PLAN_APPEND"); value != "process value" {
		t.Fatal("process mock control hidden")
	}
	t.Setenv("GIT_TEMPLATE_DIR", root)
	if _, eligible, err := cliFixtureInputKey(root); err != nil || eligible {
		t.Fatalf("custom Git templates eligible: %v", err)
	}
}
