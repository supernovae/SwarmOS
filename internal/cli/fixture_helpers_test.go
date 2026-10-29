package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

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
	observe  cliFixtureInputProvider // Immutable after construction; nil uses real observations.
	mu       sync.Mutex
	selected bool
	key      string
	root     string
	err      error
}

var pristineCLIFixture cliFixtureSeed

// Stages preserve the original eligibility short circuit. Providers are owned by
// one seed and installed before publication, rather than changing process state.
type cliFixtureInputProvider struct {
	root     func(string) (bool, error)
	template func() string
	config   func(int) error
	identity func(string) (string, []string)
}
type cliFixtureInput struct {
	empty    bool
	err      error
	template string
	config   [4]error
	basename string
	env      []string
}

func realCLIFixtureInputProvider() cliFixtureInputProvider {
	return cliFixtureInputProvider{
		root:     func(root string) (bool, error) { e, err := os.ReadDir(root); return len(e) == 0, err },
		template: func() string { return os.Getenv("GIT_TEMPLATE_DIR") },
		config: func(i int) error {
			paths := []string{filepath.Join(os.Getenv("HOME"), ".gitconfig"), filepath.Join(os.Getenv("HOME"), ".config", "git", "config"), filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "git", "config"), "/etc/gitconfig"}
			_, err := os.Lstat(paths[i])
			return err
		},
		identity: func(root string) (string, []string) { return filepath.Base(root), os.Environ() },
	}
}
func observeCLIFixtureInput(root string, p cliFixtureInputProvider) cliFixtureInput {
	var o cliFixtureInput
	o.empty, o.err = p.root(root)
	if o.err != nil || !o.empty {
		return o
	}
	o.template = p.template()
	if o.template != "" {
		return o
	}
	for i := range o.config {
		o.config[i] = p.config(i)
		if !os.IsNotExist(o.config[i]) {
			return o
		}
	}
	o.basename, o.env = p.identity(root)
	o.env = append([]string(nil), o.env...)
	return o
}
func decideCLIFixtureInput(o cliFixtureInput) (string, bool, error) {
	if o.err != nil {
		return "", false, o.err
	}
	if !o.empty || o.template != "" {
		return "", false, nil
	}
	for _, err := range o.config {
		if !os.IsNotExist(err) {
			return "", false, nil
		}
	}
	var env []string
	for _, value := range o.env {
		name, _, _ := strings.Cut(value, "=")
		if !strings.HasPrefix(name, "SWARM_MOCK_") {
			env = append(env, value)
		}
	}
	sort.Strings(env)
	return o.basename + "\x00" + strings.Join(env, "\x00"), true, nil
}
func cliFixtureInputKey(root string) (string, bool, error) {
	return decideCLIFixtureInput(observeCLIFixtureInput(root, realCLIFixtureInputProvider()))
}
func pristineCLIInputProvider() cliFixtureInputProvider {
	return cliFixtureInputProvider{
		root:     func(root string) (bool, error) { e, err := os.ReadDir(root); return len(e) == 0, err },
		template: func() string { return os.Getenv("GIT_TEMPLATE_DIR") },
		config:   func(int) error { return os.ErrNotExist },
		identity: func(root string) (string, []string) { return filepath.Base(root), os.Environ() },
	}
}

// Seed invariants use fixed eligibility while retaining real roots and copies.
func deterministicCLIInputProvider() cliFixtureInputProvider {
	p := pristineCLIInputProvider()
	p.template = func() string { return "" }
	p.identity = func(root string) (string, []string) { return filepath.Base(root), []string{"FIXTURE=pristine"} }
	return p
}

func (s *cliFixtureSeed) populate(root string, initialize func(string) error) error {
	p := s.observe
	if p.root == nil {
		p = realCLIFixtureInputProvider()
	}
	key, eligible, err := decideCLIFixtureInput(observeCLIFixtureInput(root, p))
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
	seed := cliFixtureSeed{observe: deterministicCLIInputProvider()}
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
	seed := cliFixtureSeed{observe: deterministicCLIInputProvider()}
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
			seed := cliFixtureSeed{observe: deterministicCLIInputProvider()}
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
	provider := pristineCLIInputProvider()
	provider.template = func() string { return "" }
	before, eligible, err := decideCLIFixtureInput(observeCLIFixtureInput(root, provider))
	if err != nil || !eligible {
		t.Fatalf("empty root ineligible: %v", err)
	}
	t.Setenv("SWARM_MOCK_PLAN_APPEND", "process value")
	after, eligible, err := decideCLIFixtureInput(observeCLIFixtureInput(root, provider))
	if err != nil || !eligible || before != after {
		t.Fatalf("mock control changed generation key: %v", err)
	}
	if value, _ := newFixtureEnv(t).lookup()("SWARM_MOCK_PLAN_APPEND"); value != "process value" {
		t.Fatal("process mock control hidden")
	}
	t.Setenv("GIT_TEMPLATE_DIR", root)
	provider.template = func() string { return os.Getenv("GIT_TEMPLATE_DIR") }
	if _, eligible, err := decideCLIFixtureInput(observeCLIFixtureInput(root, provider)); err != nil || eligible {
		t.Fatalf("custom Git templates eligible: %v", err)
	}
}

// These observations are scheduling hints, not reservations. A failed or
// unsupported observation leaves testing's original parallelism unchanged.
type cliTestParallelLimit struct {
	max, current uint64
	unlimited    bool
}
type cliTestParallelObservation struct {
	valid          bool
	memory, nproc  uint64
	nprocUnlimited bool
	ancestors      []struct{ memory, pids cliTestParallelLimit }
}
type cliTestParallelDecision struct {
	Original int    `json:"original"`
	Final    int    `json:"final"`
	Reason   string `json:"reason"`
}

func decideCLITestParallel(d int, explicit bool, goos string, o cliTestParallelObservation) cliTestParallelDecision {
	r := cliTestParallelDecision{d, d, "unchanged"}
	if explicit {
		r.Reason = "explicit"
		return r
	}
	if goos != "linux" || d <= 1 || d == 16 {
		return r
	}
	if d > 16 {
		r.Final = 16
		r.Reason = "capped"
		return r
	}
	r.Reason = "fallback"
	if !o.valid || o.memory < 4294967296 || (!o.nprocUnlimited && o.nproc < 4096) {
		return r
	}
	for _, a := range o.ancestors {
		for i, l := range []cliTestParallelLimit{a.memory, a.pids} {
			if !l.unlimited && (l.current > l.max || (i == 0 && l.max-l.current < 4294967296) || (i == 1 && l.max-l.current < 1024)) {
				return r
			}
		}
	}
	r.Final = 16
	r.Reason = "admitted"
	return r
}
func applyCLITestParallel(f *flag.FlagSet, goos string, provider func() cliTestParallelObservation) (cliTestParallelDecision, error) {
	if !f.Parsed() {
		return cliTestParallelDecision{}, errors.New("test flags not parsed")
	}
	v := f.Lookup("test.parallel")
	if v == nil {
		return cliTestParallelDecision{}, errors.New("missing test.parallel")
	}
	d, err := strconv.Atoi(v.Value.String())
	if err != nil {
		return cliTestParallelDecision{}, err
	}
	explicit := false
	f.Visit(func(v *flag.Flag) {
		if v.Name == "test.parallel" {
			explicit = true
		}
	})
	var o cliTestParallelObservation
	if !explicit && goos == "linux" && d > 1 && d < 16 {
		o = provider()
	}
	r := decideCLITestParallel(d, explicit, goos, o)
	if r.Final != d {
		err = v.Value.Set(strconv.Itoa(r.Final))
	}
	return r, err
}

// The provider interface bounds reads before allocating their contents. The
// production reader uses limit+1 and requires EOF; fixtures obey the same cap.
type cliParallelFiles struct {
	read func(string, int) (string, error)
	link func(string) (string, error)
	stat func(string) (fs.FileInfo, error)
}

func realCLIParallelFiles() cliParallelFiles {
	return cliParallelFiles{
		read: func(path string, limit int) (string, error) {
			f, err := os.Open(path)
			if err != nil {
				return "", err
			}
			defer f.Close()
			b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
			if err != nil {
				return "", err
			}
			if len(b) > limit {
				return "", errors.New("text limit")
			}
			return string(b), nil
		}, link: os.Readlink, stat: os.Lstat,
	}
}

type cliParallelReader struct {
	files                 cliParallelFiles
	reads, bytes, scalars int
}

func cliParallelLines(s string, cap int) bool {
	if strings.ContainsRune(s, 0) {
		return false
	}
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if len(lines) > cap {
		return false
	}
	for _, line := range lines {
		if len(line) > 4096 {
			return false
		}
	}
	return true
}
func (r *cliParallelReader) text(path string, cap, records int) (string, error) {
	if len(path) > 4096 || r.reads >= 300 {
		return "", errors.New("read budget")
	}
	r.reads++
	s, err := r.files.read(path, cap)
	if err != nil {
		return "", err
	}
	r.bytes += len(s)
	if len(s) > cap || r.bytes > 512*1024 || !cliParallelLines(s, records) {
		return "", errors.New("text budget")
	}
	return s, nil
}
func (r *cliParallelReader) namespace() (string, error) {
	s, err := r.files.link("/proc/self/ns/cgroup")
	if err != nil {
		return "", err
	}
	if len(s) > 128 || s != "cgroup:[4026531835]" {
		return "", errors.New("unsupported namespace")
	}
	return s, nil
}
func cliParallelPath(p string) bool {
	if len(p) == 0 || len(p) > 4096 || !path.IsAbs(p) || path.Clean(p) != p {
		return false
	}
	for _, c := range p {
		if unicode.IsControl(c) || c == '\\' {
			return false
		}
	}
	return len(strings.Split(strings.TrimPrefix(p, "/"), "/")) <= 64
}
func cliParallelUnescape(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		if i+3 >= len(s) {
			return "", errors.New("mount escape")
		}
		c, ok := map[string]byte{"040": ' ', "011": '\t', "012": '\n', "134": '\\'}[s[i+1:i+4]]
		if !ok {
			return "", errors.New("mount escape")
		}
		b.WriteByte(c)
		i += 3
	}
	v := b.String()
	if !cliParallelPath(v) {
		return "", errors.New("mount path")
	}
	return v, nil
}
func cliParallelMembership(s string) (string, error) {
	s = strings.TrimSuffix(s, "\n")
	if !strings.HasPrefix(s, "0::") || strings.ContainsAny(s, "\n\r") {
		return "", errors.New("membership")
	}
	p := strings.TrimPrefix(s, "0::")
	if !cliParallelPath(p) || strings.Contains(p, ":") {
		return "", errors.New("membership path")
	}
	return p, nil
}
func cliParallelMount(s string) (string, error) {
	var mount string
	var points []string
	for _, line := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
		fields := strings.Fields(line)
		sep := -1
		for i, f := range fields {
			if f == "-" {
				if sep != -1 {
					return "", errors.New("mount separator")
				}
				sep = i
			}
		}
		if sep < 6 || len(fields) != sep+4 {
			return "", errors.New("mount fields")
		}
		for _, idx := range []int{0, 1} {
			if _, err := cliParallelUint(fields[idx]); err != nil {
				return "", err
			}
		}
		dev := strings.Split(fields[2], ":")
		if len(dev) != 2 {
			return "", errors.New("mount device")
		}
		for _, n := range dev {
			if _, err := cliParallelUint(n); err != nil {
				return "", err
			}
		}
		root, err := cliParallelUnescape(fields[3])
		if err != nil {
			return "", err
		}
		point, err := cliParallelUnescape(fields[4])
		if err != nil {
			return "", err
		}
		points = append(points, point)
		if fields[sep+1] == "cgroup" {
			return "", errors.New("hybrid mount")
		}
		if fields[sep+1] == "cgroup2" {
			if mount != "" || root != "/" {
				return "", errors.New("ambiguous cgroup mount")
			}
			mount = point
		}
	}
	if mount == "" {
		return "", errors.New("missing cgroup mount")
	}
	count := 0
	for _, p := range points {
		if p == mount {
			count++
		} else if strings.HasPrefix(p, mount+"/") || mount == "/" {
			return "", errors.New("nested mount")
		}
	}
	if count != 1 {
		return "", errors.New("overmount")
	}
	return mount, nil
}
func cliParallelUint(s string) (uint64, error) {
	if s == "" {
		return 0, errors.New("empty scalar")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("nondecimal scalar")
		}
	}
	return strconv.ParseUint(s, 10, 64)
}
func cliParallelScalar(s string, allowMax bool) (uint64, bool, error) {
	s = strings.TrimSuffix(s, "\n")
	if allowMax && s == "max" {
		return 0, true, nil
	}
	n, err := cliParallelUint(s)
	return n, false, err
}
func cliParallelMemory(s string) (uint64, error) {
	seen := false
	var result uint64
	for _, line := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok || key == "" || strings.IndexFunc(key, unicode.IsSpace) >= 0 {
			return 0, errors.New("meminfo line")
		}
		f := strings.Fields(rest)
		if len(f) < 1 || len(f) > 2 {
			return 0, errors.New("meminfo fields")
		}
		n, err := cliParallelUint(f[0])
		if err != nil {
			return 0, err
		}
		if len(f) == 2 && f[1] != "kB" {
			return 0, errors.New("meminfo units")
		}
		if key == "MemAvailable" {
			if seen || len(f) != 2 || n > ^uint64(0)/1024 {
				return 0, errors.New("MemAvailable")
			}
			seen = true
			result = n * 1024
		}
	}
	if !seen {
		return 0, errors.New("missing MemAvailable")
	}
	return result, nil
}
func cliParallelNproc(s string) (uint64, bool, error) {
	seen := false
	var soft uint64
	var unlimited bool
	for i, line := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
		f := strings.Fields(line)
		if i == 0 && strings.Join(f, " ") == "Limit Soft Limit Hard Limit Units" {
			continue
		}
		if len(f) < 3 {
			return 0, false, errors.New("limits line")
		}
		// Other Linux limit names and optional units vary; the two limit columns
		// are always decimal or unlimited. Validate them before ignoring the row.
		end := len(f)
		if _, err := cliParallelUint(f[end-1]); err != nil && f[end-1] != "unlimited" {
			end--
		}
		if end < 3 {
			return 0, false, errors.New("limits columns")
		}
		for _, v := range f[end-2 : end] {
			if v != "unlimited" {
				if _, err := cliParallelUint(v); err != nil {
					return 0, false, err
				}
			}
		}
		if len(f) >= 2 && f[0] == "Max" && f[1] == "processes" {
			if seen || len(f) != 5 || f[0] != "Max" || f[1] != "processes" || f[4] != "processes" {
				return 0, false, errors.New("NPROC row")
			}
			seen = true
			unlimited = f[2] == "unlimited"
			if !unlimited {
				var err error
				soft, err = cliParallelUint(f[2])
				if err != nil {
					return 0, false, err
				}
			}
		}
	}
	if !seen {
		return 0, false, errors.New("missing NPROC")
	}
	return soft, unlimited, nil
}
func (r *cliParallelReader) regular(path string) error {
	if len(path) > 4096 {
		return errors.New("resource path budget")
	}
	info, err := r.files.stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("nonregular resource")
	}
	return nil
}
func (r *cliParallelReader) limit(dir, name string) (cliTestParallelLimit, error) {
	var l cliTestParallelLimit
	for i, suffix := range []string{".max", ".current"} {
		if r.scalars >= 256 {
			return l, errors.New("scalar budget")
		}
		r.scalars++
		p := path.Join(dir, name+suffix)
		if err := r.regular(p); err != nil {
			return l, err
		}
		text, err := r.text(p, 128, 1)
		if err != nil {
			return l, err
		}
		n, u, err := cliParallelScalar(text, i == 0)
		if err != nil {
			return l, err
		}
		if i == 0 {
			l.max, l.unlimited = n, u
		} else {
			l.current = n
		}
	}
	if !l.unlimited && l.current > l.max {
		return l, errors.New("current exceeds max")
	}
	return l, nil
}
func observeCLITestParallel(files cliParallelFiles) cliTestParallelObservation {
	o, err := readCLITestParallel(files)
	if err != nil {
		return cliTestParallelObservation{}
	}
	return o
}
func readCLITestParallel(files cliParallelFiles) (cliTestParallelObservation, error) {
	var o cliTestParallelObservation
	r := cliParallelReader{files: files}
	ns, err := r.namespace()
	if err != nil {
		return o, err
	}
	membership, err := r.text("/proc/self/cgroup", 8192, 16)
	if err != nil {
		return o, err
	}
	member, err := cliParallelMembership(membership)
	if err != nil {
		return o, err
	}
	mounts, err := r.text("/proc/self/mountinfo", 256*1024, 2048)
	if err != nil {
		return o, err
	}
	mount, err := cliParallelMount(mounts)
	if err != nil {
		return o, err
	}
	leaf := path.Join(mount, strings.TrimPrefix(member, "/"))
	rel, err := filepath.Rel(mount, leaf)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return o, errors.New("containment")
	}
	if !cliParallelPath(leaf) {
		return o, errors.New("leaf path")
	}
	dirs := []string{mount}
	if rel != "." {
		p := mount
		for _, c := range strings.Split(rel, string(filepath.Separator)) {
			p = path.Join(p, c)
			dirs = append(dirs, p)
		}
	}
	if len(dirs) > 64 {
		return o, errors.New("ancestor budget")
	}
	for _, p := range dirs {
		info, err := files.stat(p)
		if err != nil {
			return o, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return o, errors.New("non-directory ancestor")
		}
	}
	controllersPath := path.Join(mount, "cgroup.controllers")
	if err := r.regular(controllersPath); err != nil {
		return o, err
	}
	controllers, err := r.text(controllersPath, 4096, 1)
	if err != nil {
		return o, err
	}
	mem, pids := 0, 0
	for _, v := range strings.Fields(controllers) {
		if v == "memory" {
			mem++
		}
		if v == "pids" {
			pids++
		}
	}
	if mem != 1 || pids != 1 {
		return o, errors.New("controllers")
	}
	text, err := r.text("/proc/meminfo", 64*1024, 512)
	if err != nil {
		return o, err
	}
	o.memory, err = cliParallelMemory(text)
	if err != nil {
		return o, err
	}
	text, err = r.text("/proc/self/limits", 16*1024, 128)
	if err != nil {
		return o, err
	}
	o.nproc, o.nprocUnlimited, err = cliParallelNproc(text)
	if err != nil {
		return o, err
	}
	for i := len(dirs) - 1; i > 0; i-- {
		m, err := r.limit(dirs[i], "memory")
		if err != nil {
			return o, err
		}
		p, err := r.limit(dirs[i], "pids")
		if err != nil {
			return o, err
		}
		o.ancestors = append(o.ancestors, struct{ memory, pids cliTestParallelLimit }{m, p})
	}
	end, err := r.text("/proc/self/cgroup", 8192, 16)
	if err != nil || end != membership {
		return o, errors.New("membership changed")
	}
	end, err = r.namespace()
	if err != nil || end != ns {
		return o, errors.New("namespace changed")
	}
	end, err = r.text("/proc/self/mountinfo", 256*1024, 2048)
	if err != nil || end != mounts {
		return o, errors.New("mounts changed")
	}
	o.valid = true
	return o, nil
}

func TestCLITestParallelDecision(t *testing.T) {
	good := cliTestParallelObservation{valid: true, memory: 4294967296, nproc: 4096}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, d := range []int{-1, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 32, int(^uint(0) >> 1)} {
			for _, explicit := range []bool{false, true} {
				want := d
				if !explicit && goos == "linux" && d > 1 {
					want = 16
				}
				if got := decideCLITestParallel(d, explicit, goos, good); got.Final != want {
					t.Fatalf("decision %s %d %v: %+v", goos, d, explicit, got)
				}
			}
		}
	}
	for _, o := range []cliTestParallelObservation{{}, {valid: true, memory: 4294967295, nproc: 4096}, {valid: true, memory: 4294967296, nproc: 4095}} {
		for d := 2; d < 16; d++ {
			if decideCLITestParallel(d, false, "linux", o).Final != d {
				t.Fatal("invalid observation admitted")
			}
		}
	}
	for _, memory := range []bool{false, true} {
		for _, headroom := range []uint64{0, 1023, 1024, 4294967295, 4294967296} {
			o := good
			a := struct{ memory, pids cliTestParallelLimit }{cliTestParallelLimit{unlimited: true}, cliTestParallelLimit{unlimited: true}}
			threshold := uint64(1024)
			if memory {
				a.memory = cliTestParallelLimit{max: headroom}
				threshold = 4294967296
			} else {
				a.pids = cliTestParallelLimit{max: headroom}
			}
			o.ancestors = append(o.ancestors, a)
			for d := 2; d < 16; d++ {
				want := d
				if headroom >= threshold {
					want = 16
				}
				if decideCLITestParallel(d, false, "linux", o).Final != want {
					t.Fatal("threshold decision")
				}
			}
		}
	}
	good.nproc = 0
	good.nprocUnlimited = true
	if decideCLITestParallel(2, false, "linux", good).Final != 16 {
		t.Fatal("unlimited NPROC rejected")
	}
}
func TestCLITestParallelFlags(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		for _, d := range []int{0, 1, 2, 4, 8, 15, 16, 17, 32} {
			for _, args := range [][]string{nil, {"-test.parallel=1"}, {"-test.parallel", "2"}, {"--test.parallel=4"}, {"-test.parallel=8"}, {"-test.parallel=16"}, {"-test.parallel=32"}, {"-test.parallel=" + strconv.Itoa(d)}, {"-test.parallel=1", "-test.parallel=8"}, {"-test.parallel=8", "-test.parallel=1"}, {"-test.parallel=8", "-test.parallel=8"}, {"-test.parallel=8", "-test.parallel=" + strconv.Itoa(d)}, {"-test.parallel=0"}, {"-test.parallel=-1"}} {
				f := flag.NewFlagSet("fixture", flag.ContinueOnError)
				var help bytes.Buffer
				f.SetOutput(&help)
				value := f.Int("test.parallel", d, "parallel fixture")
				other := f.String("other", "kept", "unrelated")
				if err := f.Parse(append(append([]string(nil), args...), "arg")); err != nil {
					t.Fatal(err)
				}
				f.PrintDefaults()
				beforeHelp := help.String()
				help.Reset()
				old := *value
				explicit := len(args) > 0
				calls := 0
				provider := func() cliTestParallelObservation {
					calls++
					if explicit || goos != "linux" || d <= 1 || d >= 16 {
						t.Fatal("unexpected probe")
					}
					return cliTestParallelObservation{valid: true, memory: 4294967296, nproc: 4096}
				}
				r, err := applyCLITestParallel(f, goos, provider)
				if err != nil {
					t.Fatal(err)
				}
				want := old
				if !explicit && goos == "linux" && old > 1 {
					want = 16
				}
				if *value != want || r.Final != want || *other != "kept" || !reflect.DeepEqual(f.Args(), []string{"arg"}) {
					t.Fatal("flag application changed unrelated state")
				}
				r2, err := applyCLITestParallel(f, goos, provider)
				if err != nil || r2.Final != want || calls > 1 {
					t.Fatal("application not idempotent")
				}
				visited := false
				f.Visit(func(v *flag.Flag) {
					if v.Name == "test.parallel" {
						visited = true
					}
				})
				if visited != explicit {
					t.Fatal("implicit update marked explicit")
				}
				f.PrintDefaults()
				if help.String() != beforeHelp || f.Lookup("test.parallel").DefValue != strconv.Itoa(d) {
					t.Fatal("help/default changed")
				}
			}
		}
	}
	for _, args := range [][]string{{"-test.parallel=bad"}, {"-test.parallel=18446744073709551616"}, {"-test.parallel=bad", "-test.parallel=1"}} {
		f := flag.NewFlagSet("fixture", flag.ContinueOnError)
		f.SetOutput(io.Discard)
		f.Int("test.parallel", 2, "")
		if f.Parse(args) == nil {
			t.Fatal("malformed flag accepted")
		}
	}
	// Invalid observations remain at D on repeated application.
	for d := 2; d < 16; d++ {
		f := flag.NewFlagSet("fixture", flag.ContinueOnError)
		v := f.Int("test.parallel", d, "")
		_ = f.Parse(nil)
		for range 2 {
			if _, err := applyCLITestParallel(f, "linux", func() cliTestParallelObservation { return cliTestParallelObservation{} }); err != nil || *v != d {
				t.Fatal("fallback changed D")
			}
		}
	}
}

type cliParallelFixtureInfo struct {
	directory bool
	mode      fs.FileMode
}

func (i cliParallelFixtureInfo) Name() string { return "fixture" }
func (i cliParallelFixtureInfo) Size() int64  { return 0 }
func (i cliParallelFixtureInfo) Mode() fs.FileMode {
	if i.directory {
		return fs.ModeDir | 0700
	}
	return i.mode | 0600
}
func (i cliParallelFixtureInfo) ModTime() time.Time { return time.Time{} }
func (i cliParallelFixtureInfo) IsDir() bool        { return i.directory }
func (i cliParallelFixtureInfo) Sys() any           { return nil }
func cliParallelFixtureTexts() map[string]string {
	m := map[string]string{
		"/proc/self/cgroup":      "0::/parent/middle/leaf\n",
		"/proc/self/mountinfo":   "29 1 0:26 / /cg rw,nosuid shared:1 - cgroup2 cgroup rw\n",
		"/proc/meminfo":          "MemTotal: 9000000 kB\nMemAvailable: 4194304 kB\n",
		"/proc/self/limits":      "Limit                     Soft Limit           Hard Limit           Units\nMax processes             4096                 unlimited            processes\n",
		"/cg/cgroup.controllers": "memory pids\n",
	}
	for _, dir := range []string{"/cg/parent", "/cg/parent/middle", "/cg/parent/middle/leaf"} {
		m[dir+"/memory.max"] = "4294967296\n"
		m[dir+"/memory.current"] = "0\n"
		m[dir+"/pids.max"] = "1024\n"
		m[dir+"/pids.current"] = "0\n"
	}
	return m
}
func cliParallelFixtureFiles(texts map[string]string) cliParallelFiles {
	frozen := make(map[string]string, len(texts))
	for k, v := range texts {
		frozen[k] = v
	}
	return cliParallelFiles{
		read: func(p string, cap int) (string, error) {
			s, ok := frozen[p]
			if !ok {
				return "", os.ErrNotExist
			}
			if len(s) > cap {
				return "", errors.New("fixture read limit")
			}
			return s, nil
		},
		link: func(p string) (string, error) {
			if p != "/proc/self/ns/cgroup" {
				return "", os.ErrPermission
			}
			return "cgroup:[4026531835]", nil
		},
		stat: func(p string) (fs.FileInfo, error) {
			if p == "/cg" || p == "/cg/parent" || p == "/cg/parent/middle" || p == "/cg/parent/middle/leaf" {
				return cliParallelFixtureInfo{directory: true}, nil
			}
			if _, ok := frozen[p]; ok {
				return cliParallelFixtureInfo{}, nil
			}
			return nil, os.ErrNotExist
		},
	}
}
func TestCLITestParallelComposedReader(t *testing.T) {
	for d := 2; d < 16; d++ {
		f := cliParallelFixtureFiles(cliParallelFixtureTexts())
		links := 0
		pid1 := 0
		f.link = func(p string) (string, error) {
			if p == "/proc/1/ns/cgroup" {
				pid1++
				return "", os.ErrPermission
			}
			if p != "/proc/self/ns/cgroup" {
				t.Fatal("unexpected link")
			}
			links++
			return "cgroup:[4026531835]", nil
		}
		o, err := readCLITestParallel(f)
		if err != nil || !o.valid || len(o.ancestors) != 3 {
			t.Fatal("qualifying composition failed", err)
		}
		flags := flag.NewFlagSet("fixture", flag.ContinueOnError)
		v := flags.Int("test.parallel", d, "")
		_ = flags.Parse(nil)
		r, err := applyCLITestParallel(flags, "linux", func() cliTestParallelObservation { return observeCLITestParallel(f) })
		if err != nil || r.Reason != "admitted" || *v != 16 || pid1 != 0 || links != 4 {
			t.Fatal("reader/decision composition failed")
		}
	}
	// All three finite ancestors independently constrain the decision.
	for _, dir := range []string{"/cg/parent", "/cg/parent/middle", "/cg/parent/middle/leaf"} {
		for _, tc := range []struct {
			file, value string
			valid       bool
		}{
			{"memory.max", "4294967295\n", true}, {"memory.current", "1\n", true},
			{"pids.max", "1023\n", true}, {"pids.current", "1\n", true},
			{"memory.current", "4294967297\n", false}, {"pids.current", "1025\n", false},
			{"memory.current", "max\n", false}, {"pids.current", "max\n", false},
		} {
			m := cliParallelFixtureTexts()
			m[dir+"/"+tc.file] = tc.value
			o := observeCLITestParallel(cliParallelFixtureFiles(m))
			if o.valid != tc.valid {
				t.Fatal("observation validity", dir, tc.file)
			}
			for d := 2; d < 16; d++ {
				if decideCLITestParallel(d, false, "linux", o).Final != d {
					t.Fatal("tight ancestor admitted")
				}
			}
		}
	}
	for _, tc := range []struct{ path, value string }{
		{"/proc/self/cgroup", "0::/\n0::/other\n"}, {"/proc/self/cgroup", "1:memory:/x\n0::/x\n"},
		{"/proc/self/cgroup", "0::relative\n"}, {"/proc/self/cgroup", "0::/a/../b\n"}, {"/proc/self/cgroup", "0::/a//b\n"},
		{"/proc/self/cgroup", "0::/a/\n"}, {"/proc/self/cgroup", "0::/a:b\n"},
		{"/proc/self/mountinfo", "29 1 0:26 /sub /cg rw - cgroup2 cgroup rw\n"},
		{"/proc/self/mountinfo", "29 1 0:26 / /cg rw - cgroup cgroup rw\n"},
		{"/proc/self/mountinfo", "29 1 0:26 / /cg rw cgroup2 cgroup rw\n"},
		{"/proc/self/mountinfo", "29 1 0:26 / /cg rw - cgroup2 cgroup rw\n30 1 0:27 / /cg/nested rw - tmpfs tmpfs rw\n"},
		{"/proc/self/mountinfo", "29 1 0:26 / /cg rw - cgroup2 cgroup rw\n30 1 0:27 / /other rw - cgroup2 cgroup rw\n"},
		{"/cg/cgroup.controllers", "memory\n"}, {"/cg/cgroup.controllers", "pids\n"}, {"/cg/cgroup.controllers", "memory memory pids\n"},
		{"/proc/meminfo", "MemAvailable: 4194303 kB\n"}, {"/proc/self/limits", "Max processes 4095 999999 processes\n"},
	} {
		m := cliParallelFixtureTexts()
		m[tc.path] = tc.value
		o := observeCLITestParallel(cliParallelFixtureFiles(m))
		for d := 2; d < 16; d++ {
			if decideCLITestParallel(d, false, "linux", o).Final != d {
				t.Fatal("unsupported observation admitted", tc.path)
			}
		}
	}
	// Failure of any read, including every scalar, must fail closed.
	for path := range cliParallelFixtureTexts() {
		f := cliParallelFixtureFiles(cliParallelFixtureTexts())
		read := f.read
		f.read = func(p string, n int) (string, error) {
			if p == path {
				return "", os.ErrPermission
			}
			return read(p, n)
		}
		if observeCLITestParallel(f).valid {
			t.Fatal("read error accepted", path)
		}
	}
	for _, value := range []string{"", "cgroup:[123]", strings.Repeat("x", 129)} {
		f := cliParallelFixtureFiles(cliParallelFixtureTexts())
		f.link = func(string) (string, error) { return value, nil }
		if observeCLITestParallel(f).valid {
			t.Fatal("unsupported namespace accepted")
		}
	}
	for _, completion := range []bool{false, true} {
		f := cliParallelFixtureFiles(cliParallelFixtureTexts())
		calls := 0
		f.link = func(string) (string, error) {
			calls++
			if !completion || calls == 2 {
				return "", os.ErrPermission
			}
			return "cgroup:[4026531835]", nil
		}
		if observeCLITestParallel(f).valid {
			t.Fatal("namespace read error accepted")
		}
	}
	for _, target := range []string{"/proc/self/cgroup", "/proc/self/mountinfo", "/proc/self/ns/cgroup"} {
		f := cliParallelFixtureFiles(cliParallelFixtureTexts())
		read, link := f.read, f.link
		calls := 0
		if target == "/proc/self/ns/cgroup" {
			f.link = func(p string) (string, error) {
				calls++
				if calls == 2 {
					return "cgroup:[123]", nil
				}
				return link(p)
			}
		} else {
			f.read = func(p string, n int) (string, error) {
				if p == target {
					calls++
					if calls == 2 {
						return "changed", nil
					}
				}
				return read(p, n)
			}
		}
		if observeCLITestParallel(f).valid {
			t.Fatal("mapping change accepted")
		}
	}
	for _, path := range []string{"/cg", "/cg/parent", "/cg/parent/middle", "/cg/parent/middle/leaf", "/cg/parent/memory.max", "/cg/cgroup.controllers"} {
		for _, mode := range []fs.FileMode{fs.ModeSymlink, fs.ModeNamedPipe} {
			f := cliParallelFixtureFiles(cliParallelFixtureTexts())
			stat := f.stat
			f.stat = func(p string) (fs.FileInfo, error) {
				if p == path {
					return cliParallelFixtureInfo{mode: mode}, nil
				}
				return stat(p)
			}
			if observeCLITestParallel(f).valid {
				t.Fatal("unsafe path accepted", path)
			}
		}
	}
	// The genuine root has no max interfaces, but still requires controllers.
	m := cliParallelFixtureTexts()
	m["/proc/self/cgroup"] = "0::/\n"
	o := observeCLITestParallel(cliParallelFixtureFiles(m))
	if !o.valid || len(o.ancestors) != 0 || decideCLITestParallel(2, false, "linux", o).Final != 16 {
		t.Fatal("root membership rejected")
	}
	for _, name := range []string{"memory", "pids"} {
		m := cliParallelFixtureTexts()
		for p := range m {
			if strings.HasSuffix(p, "/"+name+".max") {
				m[p] = "max\n"
			}
		}
		if !observeCLITestParallel(cliParallelFixtureFiles(m)).valid {
			t.Fatal("unlimited ancestor rejected")
		}
		m["/cg/parent/"+name+".current"] = "max\n"
		if observeCLITestParallel(cliParallelFixtureFiles(m)).valid {
			t.Fatal("unlimited current accepted")
		}
	}
}

func TestCLITestParallelParsersAndBounds(t *testing.T) {
	for _, s := range []string{"", " ", "-1", "+1", "0x10", "1.5", "18446744073709551616", "1 2", "1\n2", "1\n\n", "MAX", "max\nextra", "12\r\n"} {
		if _, _, err := cliParallelScalar(s, true); err == nil {
			t.Fatalf("bad scalar accepted %q", s)
		}
	}
	for _, s := range []string{"0", "0\n", "18446744073709551615\n"} {
		if _, _, err := cliParallelScalar(s, false); err != nil {
			t.Fatal("valid scalar rejected", err)
		}
	}
	if _, u, err := cliParallelScalar("max\n", true); err != nil || !u {
		t.Fatal("max rejected")
	}
	if _, _, err := cliParallelScalar("max", false); err == nil {
		t.Fatal("current=max accepted")
	}
	for _, s := range []string{"", "MemFree: 999999999 kB\n", "MemAvailable: 4194304 MB\n", "MemAvailable: 1 kB\nMemAvailable: 1 kB\n", "MemAvailable: 4194304 kB\n MemAvailable: 1 kB\n", "MemAvailable: 18014398509481984 kB\n", "MemAvailable: +1 kB\n", "MemAvailable: 1\n", "bad\n"} {
		if _, err := cliParallelMemory(s); err == nil {
			t.Fatal("bad meminfo accepted")
		}
	}
	for _, s := range []string{"", "Max processes 4096 unlimited bytes\n", "Max processes bad unlimited processes\n", "Max processes 4096 bad processes\n", "Max processes 4096 unlimited processes\nMax processes 4096 unlimited processes\n", "Max processes 4096 unlimited processes\n Max   processes 4096 unlimited processes\n", "Max processes +1 unlimited processes\n", "bad\n"} {
		if _, _, err := cliParallelNproc(s); err == nil {
			t.Fatal("bad limits accepted", s)
		}
	}
	for _, s := range []string{"Max processes 4096 unlimited processes\n", "Max processes unlimited unlimited processes\n", "Max processes 4096 8192 processes\n"} {
		if _, _, err := cliParallelNproc(s); err != nil {
			t.Fatal("valid limits rejected", err)
		}
	}
	for _, s := range []string{"/cg\\040space", "/cg"} {
		if _, err := cliParallelUnescape(s); err != nil {
			t.Fatal("valid mount escape rejected")
		}
	}
	for _, s := range []string{"relative", "/a\\011b", "/a\\012b", "/a\\134b", "/a\\041b", "/a\\0", "/a/../b", "/a//b", "/a/", "/a\x00b"} {
		if _, err := cliParallelUnescape(s); err == nil {
			t.Fatal("bad mount escape/path accepted")
		}
	}
	// Exercise limit+1 reads against real files, including a valid prefix which
	// would parse if truncation were silently accepted.
	for _, cap := range []int{128, 4096, 8192, 16384, 65536, 262144} {
		p := filepath.Join(t.TempDir(), "bounded")
		for _, n := range []int{cap, cap + 1} {
			if err := os.WriteFile(p, bytes.Repeat([]byte{'1'}, n), 0600); err != nil {
				t.Fatal(err)
			}
			text, err := realCLIParallelFiles().read(p, cap)
			if n == cap && (err != nil || len(text) != cap) {
				t.Fatal("exact read bound failed")
			}
			if n > cap && err == nil {
				t.Fatal("oversized read accepted")
			}
		}
	}
	for _, tc := range []struct{ cap, records int }{{128, 1}, {4096, 1}, {8192, 16}, {16384, 128}, {65536, 512}, {262144, 2048}} {
		r := cliParallelReader{files: cliParallelFiles{read: func(string, int) (string, error) { return strings.Repeat("x\n", tc.records), nil }}}
		if _, err := r.text("/fixture", tc.cap, tc.records); err != nil {
			t.Fatal("exact record bound rejected", err)
		}
		r.files.read = func(string, int) (string, error) { return strings.Repeat("x\n", tc.records+1), nil }
		if _, err := r.text("/fixture", tc.cap, tc.records); err == nil {
			t.Fatal("record exhaustion accepted")
		}
	}
	r := cliParallelReader{files: cliParallelFiles{read: func(string, int) (string, error) { return strings.Repeat("x", 4096), nil }}}
	if _, err := r.text("/fixture", 4096, 1); err != nil {
		t.Fatal(err)
	}
	r.files.read = func(string, int) (string, error) { return strings.Repeat("x", 4097), nil }
	if _, err := r.text("/fixture", 8192, 1); err == nil {
		t.Fatal("long line accepted")
	}
	r.files.read = func(string, int) (string, error) { return "", nil }
	r.reads = 299
	if _, err := r.text("/fixture", 128, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := r.text("/fixture", 128, 1); err == nil {
		t.Fatal("read budget exceeded")
	}
	r.reads = 0
	r.bytes = 512*1024 - 1
	r.files.read = func(string, int) (string, error) { return "x", nil }
	if _, err := r.text("/fixture", 128, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := r.text("/fixture", 128, 1); err == nil {
		t.Fatal("aggregate byte budget exceeded")
	}
	if cliParallelPath("/"+strings.Repeat("a/", 64)+"a") || cliParallelPath("/"+strings.Repeat("a", 4096)) {
		t.Fatal("path bounds exceeded")
	}
	// Traversal includes the root; 64 non-root components exceed 64 dirs.
	m := cliParallelFixtureTexts()
	m["/proc/self/cgroup"] = "0::/" + strings.TrimSuffix(strings.Repeat("a/", 64), "/") + "\n"
	if observeCLITestParallel(cliParallelFixtureFiles(m)).valid {
		t.Fatal("ancestor budget exceeded")
	}
	// Missing resource files at a non-root directory never mean unlimited.
	m = cliParallelFixtureTexts()
	delete(m, "/cg/parent/memory.max")
	if observeCLITestParallel(cliParallelFixtureFiles(m)).valid {
		t.Fatal("missing nonroot limit accepted")
	}
}
func TestCLIFixtureInputGuards(t *testing.T) {
	sentinel := errors.New("root read failed")
	for _, tc := range []struct {
		name       string
		empty      bool
		rootErr    error
		template   string
		probe      int
		probeErr   error
		wantStages int
		eligible   bool
	}{
		{"eligible", true, nil, "", -1, nil, 7, true},
		{"nonempty", false, nil, "", -1, nil, 1, false},
		{"root error", true, sentinel, "", -1, nil, 1, false},
		{"template", true, nil, "custom", -1, nil, 2, false},
		{"config0", true, nil, "", 0, nil, 3, false}, {"config1", true, nil, "", 1, nil, 4, false}, {"config2", true, nil, "", 2, nil, 5, false}, {"config3", true, nil, "", 3, nil, 6, false},
		{"config0 error", true, nil, "", 0, os.ErrPermission, 3, false}, {"config1 error", true, nil, "", 1, os.ErrPermission, 4, false}, {"config2 error", true, nil, "", 2, os.ErrPermission, 5, false}, {"config3 error", true, nil, "", 3, os.ErrPermission, 6, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stages := []string{}
			p := cliFixtureInputProvider{
				root:     func(string) (bool, error) { stages = append(stages, "root"); return tc.empty, tc.rootErr },
				template: func() string { stages = append(stages, "template"); return tc.template },
				config: func(i int) error {
					stages = append(stages, strconv.Itoa(i))
					if i == tc.probe {
						return tc.probeErr
					}
					return os.ErrNotExist
				},
				identity: func(string) (string, []string) {
					stages = append(stages, "identity")
					return "008", []string{"Z=2", "SWARM_MOCK_PLAN_APPEND=ignored", "A=1"}
				},
			}
			key, eligible, err := decideCLIFixtureInput(observeCLIFixtureInput("root", p))
			if eligible != tc.eligible || !errors.Is(err, tc.rootErr) || len(stages) != tc.wantStages {
				t.Fatal("guard semantics/order changed", stages)
			}
			wantOrder := []string{"root", "template", "0", "1", "2", "3", "identity"}
			if !reflect.DeepEqual(stages, wantOrder[:tc.wantStages]) {
				t.Fatal("stage ordering changed")
			}
			if eligible && key != "008\x00A=1\x00Z=2" {
				t.Fatal("key bytes changed")
			}
			if !eligible {
				stages = nil
				seed := cliFixtureSeed{observe: p}
				calls := 0
				err := seed.populate("requested", func(root string) error {
					calls++
					if root != "requested" {
						t.Fatal("fallback initialized another root")
					}
					return nil
				})
				if !errors.Is(err, tc.rootErr) || seed.selected || seed.root != "" {
					t.Fatal("ineligible seed published")
				}
				want := 1
				if tc.rootErr != nil {
					want = 0
				}
				if calls != want {
					t.Fatal("fallback initializer count")
				}
			}
		})
	}
	base := cliFixtureInput{empty: true, basename: "008", env: []string{"A=1"}, config: [4]error{os.ErrNotExist, os.ErrNotExist, os.ErrNotExist, os.ErrNotExist}}
	key, _, _ := decideCLIFixtureInput(base)
	for _, tc := range []struct {
		name  string
		env   []string
		equal bool
	}{{"008", []string{"A=1", "SWARM_MOCK_X=changed"}, true}, {"other", []string{"A=1"}, false}, {"008", []string{"A=2"}, false}} {
		o := base
		o.basename = tc.name
		o.env = tc.env
		k, eligible, err := decideCLIFixtureInput(o)
		if err != nil || !eligible || (k == key) != tc.equal {
			t.Fatal("key partition changed")
		}
	}
	// The observation owns its environment snapshot.
	env := []string{"A=1"}
	p := deterministicCLIInputProvider()
	p.root = func(string) (bool, error) { return true, nil }
	p.identity = func(string) (string, []string) { return "008", env }
	o := observeCLIFixtureInput("unused", p)
	env[0] = "A=changed"
	k, _, _ := decideCLIFixtureInput(o)
	if k != key {
		t.Fatal("input retained mutable environment")
	}
}
func TestCLIFixtureDefaultObservation(t *testing.T) {
	seed := cliFixtureSeed{}
	for range 2 {
		root := cliNamedRoot(t, "008")
		_, eligible, err := cliFixtureInputKey(root)
		if err != nil {
			t.Fatal(err)
		}
		builtAt := ""
		if err := seed.populate(root, func(p string) error { builtAt = p; return initializeCLIFixture(t, p) }); err != nil {
			t.Fatal(err)
		}
		if eligible {
			if !seed.selected || seed.root == "" || (builtAt != "" && builtAt != seed.root) {
				t.Fatal("default eligible branch not used")
			}
		} else if builtAt != root || seed.selected || seed.root != "" {
			t.Fatal("default fallback branch not used")
		}
		original := cliNamedRoot(t, "008")
		if err := initializeCLIFixture(t, original); err != nil {
			t.Fatal(err)
		}
		assertCLIEquivalent(t, root, original)
	}
}

func TestCLITestParallelReaderEdges(t *testing.T) {
	// A legal escaped mountpoint maps membership beneath it component by
	// component; use a translated immutable fixture filesystem.
	for _, point := range []string{"/cg space", "/" + strings.Repeat("m", 80)} {
		m := cliParallelFixtureTexts()
		escaped := strings.ReplaceAll(point, " ", `\040`)
		m["/proc/self/mountinfo"] = "29 1 0:26 / " + escaped + " rw - cgroup2 cgroup rw\n"
		base := cliParallelFixtureFiles(m)
		read, stat := base.read, base.stat
		translate := func(p string) string {
			if p == point {
				return "/cg"
			}
			if strings.HasPrefix(p, point+"/") {
				return "/cg" + strings.TrimPrefix(p, point)
			}
			return p
		}
		base.read = func(p string, n int) (string, error) { return read(translate(p), n) }
		base.stat = func(p string) (fs.FileInfo, error) { return stat(translate(p)) }
		if !observeCLITestParallel(base).valid {
			t.Fatal("escaped/translated mountpoint rejected")
		}
	}
	for _, caseName := range []string{"read error", "changed", "oversized"} {
		f := cliParallelFixtureFiles(cliParallelFixtureTexts())
		calls := 0
		f.link = func(string) (string, error) {
			calls++
			if calls == 1 {
				return "cgroup:[4026531835]", nil
			}
			switch caseName {
			case "read error":
				return "", os.ErrPermission
			case "changed":
				return "cgroup:[123]", nil
			default:
				return strings.Repeat("x", 129), nil
			}
		}
		o := observeCLITestParallel(f)
		for d := 2; d < 16; d++ {
			if decideCLITestParallel(d, false, "linux", o).Final != d {
				t.Fatal("bad completion namespace admitted")
			}
		}
	}
	// Every supported text source rejects oversized and truncated-valid-prefix
	// content through the composed reader, rather than accepting a prefix.
	for path := range cliParallelFixtureTexts() {
		for _, extra := range []string{"\nextra", strings.Repeat("x", 262145)} {
			m := cliParallelFixtureTexts()
			m[path] += extra
			if observeCLITestParallel(cliParallelFixtureFiles(m)).valid {
				t.Fatal("malformed/oversized input accepted", path)
			}
		}
	}
	// Equality yields zero headroom, whereas excess current invalidates the
	// entire observation, independently for memory and PIDs.
	for _, name := range []string{"memory", "pids"} {
		m := cliParallelFixtureTexts()
		m["/cg/parent/"+name+".current"] = m["/cg/parent/"+name+".max"]
		o := observeCLITestParallel(cliParallelFixtureFiles(m))
		if !o.valid {
			t.Fatal("max=current should be a valid exhausted observation")
		}
		for d := 2; d < 16; d++ {
			if decideCLITestParallel(d, false, "linux", o).Final != d {
				t.Fatal("exhausted resource admitted")
			}
		}
	}
	// Exactly 64 directories, including the genuine root, are supported.
	member := "/" + strings.TrimSuffix(strings.Repeat("a/", 63), "/")
	m := cliParallelFixtureTexts()
	m["/proc/self/cgroup"] = "0::" + member + "\n"
	dirs := map[string]bool{"/cg": true}
	dir := "/cg"
	for range 63 {
		dir += "/a"
		dirs[dir] = true
		for _, name := range []string{"memory", "pids"} {
			m[dir+"/"+name+".max"] = "max\n"
			m[dir+"/"+name+".current"] = "0\n"
		}
	}
	f := cliParallelFixtureFiles(m)
	stat := f.stat
	f.stat = func(p string) (fs.FileInfo, error) {
		if dirs[p] {
			return cliParallelFixtureInfo{directory: true}, nil
		}
		return stat(p)
	}
	o, err := readCLITestParallel(f)
	if err != nil || !o.valid || len(o.ancestors) != 63 {
		t.Fatal("exact ancestor bound rejected", err)
	}
	// All records are owned: concurrent decision consumers do not mutate them.
	original := append([]struct{ memory, pids cliTestParallelLimit }(nil), o.ancestors...)
	var wg sync.WaitGroup
	for d := 2; d < 16; d++ {
		wg.Add(1)
		go func(d int) {
			defer wg.Done()
			if decideCLITestParallel(d, false, "linux", o).Final != 16 {
				t.Error("frozen observation rejected")
			}
		}(d)
	}
	wg.Wait()
	if !reflect.DeepEqual(original, o.ancestors) {
		t.Fatal("decision mutated observation")
	}
	r := cliParallelReader{files: cliParallelFixtureFiles(cliParallelFixtureTexts()), scalars: 256}
	if _, err := r.limit("/cg/parent", "memory"); err == nil {
		t.Fatal("scalar budget exceeded")
	}
	for _, target := range []string{"/proc/self/cgroup", "/proc/self/mountinfo"} {
		f := cliParallelFixtureFiles(cliParallelFixtureTexts())
		read := f.read
		calls := 0
		f.read = func(p string, n int) (string, error) {
			if p == target {
				calls++
				if calls == 2 {
					return "", os.ErrPermission
				}
			}
			return read(p, n)
		}
		if observeCLITestParallel(f).valid {
			t.Fatal("completion read failure accepted")
		}
	}
}

func TestCLIFixtureDifferentKeyFallback(t *testing.T) {
	seed := cliFixtureSeed{observe: deterministicCLIInputProvider()}
	first := cliNamedRoot(t, "008")
	if err := seed.populate(first, func(root string) error { return initializeCLIFixture(t, root) }); err != nil {
		t.Fatal(err)
	}
	selected, source := seed.key, seed.root
	other := cliNamedRoot(t, "other")
	calls := 0
	if err := seed.populate(other, func(root string) error {
		calls++
		if root != other {
			t.Fatal("different key initialized seed instead of requested root")
		}
		return initializeCLIFixture(t, root)
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || seed.key != selected || seed.root != source {
		t.Fatal("different key changed seed publication")
	}
	original := cliNamedRoot(t, "other")
	if err := initializeCLIFixture(t, original); err != nil {
		t.Fatal(err)
	}
	assertCLIEquivalent(t, other, original)
}
