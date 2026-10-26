package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/node"
	"github.com/supernovae/swarmos/internal/registry"
	"github.com/supernovae/swarmos/internal/workspace"
)

// TestRootDefaultEnvironment is serial: it checks that a root command without
// an injected lookup reads the real process environment.
func TestRootDefaultEnvironment(t *testing.T) {
	f := newCLIFixture(t)
	t.Setenv("SWARM_BACKEND", "local")
	t.Setenv("SWARM_REPO", "acme/process-default")
	t.Setenv("SWARM_USER", "process-user")
	t.Setenv("SWARM_REGISTRY_PATH", filepath.Join(t.TempDir(), "repos.yaml"))
	t.Setenv("SWARM_NODE_CONFIG", filepath.Join(t.TempDir(), "node.yaml"))
	cmd := NewRootCmd()
	for name, want := range map[string]string{"backend": "local", "repo": "acme/process-default"} {
		if got := cmd.PersistentFlags().Lookup(name).DefValue; got != want {
			t.Fatalf("--%s default = %q, want %q", name, got, want)
		}
	}
	opts := &RootOptions{RepoRoot: f.root, Backend: "local"}
	if got, want := opts.registryPath(), registry.DefaultRegistryPath(); got != want {
		t.Fatalf("registry path = %q, want default %q", got, want)
	}
	if got, want := opts.nodeConfigPath(), node.DefaultNodeConfigPath(); got != want {
		t.Fatalf("node profile path = %q, want default %q", got, want)
	}
	tr, err := opts.resolveTracker(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if user, _ := tr.CurrentUser(context.Background()); user != "process-user" {
		t.Fatalf("local tracker user = %q, want process-user", user)
	}

	// Execution mode is read when doctor runs, not when the command is built.
	doctor := NewRootCmd()
	t.Setenv("SWARM_EXECUTION_MODE", "process-mode")
	var out bytes.Buffer
	doctor.SetOut(&out)
	doctor.SetErr(&out)
	doctor.SetArgs([]string{"--root", f.root, "--backend", "local", "repo", "doctor", "--json"})
	_ = doctor.Execute()
	if !strings.Contains(out.String(), `execution mode \"process-mode\"`) {
		t.Fatalf("doctor did not read the process execution mode:\n%s", out.String())
	}
}

// TestRootInjectedEnvironment is serial so the process can hold conflicting
// values that the injected lookup must override.
func TestRootInjectedEnvironment(t *testing.T) {
	t.Setenv("SWARM_BACKEND", "github")
	t.Setenv("SWARM_REPO", "acme/process")
	t.Setenv("SWARM_USER", "process-user")
	t.Setenv("SWARM_REGISTRY_PATH", "/process/repos.yaml")
	t.Setenv("SWARM_NODE_CONFIG", "/process/node.yaml")
	t.Setenv("SWARM_REPOS_DIR", "/process/repos")
	lookup := func(vars map[string]string) func(string) (string, bool) {
		return func(name string) (string, bool) {
			value, ok := vars[name]
			return value, ok
		}
	}
	home := filepath.Join(string(filepath.Separator), "injected", "home")
	for _, tc := range []struct {
		name                      string
		vars                      map[string]string
		args                      []string
		backend, repo, user       string
		registry, node, cloneBase string
	}{
		{
			name:    "injected values",
			vars:    map[string]string{"SWARM_BACKEND": "local", "SWARM_REPO": "acme/injected", "SWARM_USER": "injected-user", "SWARM_REGISTRY_PATH": "/injected/repos.yaml", "SWARM_NODE_CONFIG": "/injected/node.yaml", "SWARM_REPOS_DIR": "/injected/repos"},
			backend: "local", repo: "acme/injected", user: "injected-user", registry: "/injected/repos.yaml", node: "/injected/node.yaml", cloneBase: "/injected/repos",
		},
		{
			name:    "explicit flags win",
			vars:    map[string]string{"SWARM_BACKEND": "github", "SWARM_REPO": "acme/injected"},
			args:    []string{"--backend", "local", "--repo", "acme/flag"},
			backend: "local", repo: "acme/flag", user: "local-dev",
			registry: filepath.Join(".", ".swarm", "repos.yaml"), node: filepath.Join(".", ".swarm", "node.yaml"), cloneBase: filepath.Join(".", ".swarm", "repos"),
		},
		{
			name:     "empty values fall back like unset ones",
			vars:     map[string]string{"SWARM_BACKEND": "", "SWARM_REPO": "", "SWARM_USER": "", "USER": "injected-login", "SWARM_REGISTRY_PATH": "", "SWARM_NODE_CONFIG": "", "SWARM_REPOS_DIR": "", "HOME": home, "USERPROFILE": home, "home": home},
			user:     "injected-login",
			registry: filepath.Join(home, ".config", "swarm", "repos.yaml"), node: filepath.Join(home, ".config", "swarm", "node.yaml"), cloneBase: filepath.Join(home, ".swarm", "repos"),
		},
		{
			name:     "unset values ignore the process",
			vars:     map[string]string{},
			user:     "local-dev",
			registry: filepath.Join(".", ".swarm", "repos.yaml"), node: filepath.Join(".", ".swarm", "node.yaml"), cloneBase: filepath.Join(".", ".swarm", "repos"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := &RootOptions{lookupEnv: lookup(tc.vars)}
			cmd := newRootCmdWithOptions(opts)
			if err := cmd.PersistentFlags().Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			for name, got := range map[string][2]string{
				"backend": {opts.Backend, tc.backend}, "repo": {opts.Repo, tc.repo}, "user": {opts.localUser(), tc.user},
				"registry": {opts.registryPath(), tc.registry}, "node": {opts.nodeConfigPath(), tc.node}, "clone base": {opts.cloneBaseDir(), tc.cloneBase},
			} {
				if got[0] != got[1] {
					t.Errorf("%s = %q, want %q", name, got[0], got[1])
				}
			}
		})
	}
}

// TestCLIParallelFixtureIsolation runs fixtures concurrently; each must see
// only its own identity, registry and node profile.
func TestCLIParallelFixtureIsolation(t *testing.T) {
	t.Parallel()
	for _, user := range []string{"alice", "bob"} {
		t.Run(user, func(t *testing.T) {
			t.Parallel()
			f := newCLIFixture(t)
			f.setenv("SWARM_USER", user)
			number := f.create(t, "agent")
			f.must(t, "repo", "add", f.root, "--json")
			f.must(t, "node", "init", "--id", user+"-node")
			// Switching identity here must not reach the other fixture.
			f.setenv("SWARM_USER", user+"-maintainer")
			f.must(t, "issue", "approve", strconv.Itoa(number))
			f.setenv("SWARM_USER", user)

			issue, err := f.tracker.GetIssue(context.Background(), number)
			if err != nil {
				t.Fatal(err)
			}
			if issue.Author != user {
				t.Fatalf("issue author = %q, want %q", issue.Author, user)
			}
			if approver := f.state(t, number).ApprovedBy; approver != user+"-maintainer" {
				t.Fatalf("approved by %q, want %q", approver, user+"-maintainer")
			}
			var listed struct {
				Repositories []registry.RepoEntry `json:"repositories"`
			}
			if err := json.Unmarshal([]byte(f.must(t, "repo", "list", "--json")), &listed); err != nil {
				t.Fatal(err)
			}
			if len(listed.Repositories) != 1 || listed.Repositories[0].Path != mustEvalSymlinks(t, f.root) && listed.Repositories[0].Path != f.root {
				t.Fatalf("registry shared between fixtures: %+v", listed.Repositories)
			}
			profile, err := node.LoadOrDetectProfile(f.options(nil).nodeConfigPath())
			if err != nil || profile.NodeID != user+"-node" {
				t.Fatalf("node profile = %+v, %v", profile, err)
			}
		})
	}
}

// TestCLIResolverRunnerIsolation runs PR work in concurrent fixtures; neither
// fixture's runner hook may serve the other fixture's commands.
func TestCLIResolverRunnerIsolation(t *testing.T) {
	t.Parallel()
	type fixtureRun struct {
		root string
		mu   sync.Mutex
		seen []string
	}
	runs := map[string]*fixtureRun{"first": {}, "second": {}}
	t.Run("group", func(t *testing.T) {
		for name, run := range runs {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				f, _, number := submittedPRFixture(t, "agent")
				run.root = f.root
				f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf '"+name+"\\n' > runner-"+name+".txt")
				lookup := f.env.lookup()
				f.resolverRunner = func(agentName string, cfg *contract.SwarmConfig, local bool) (agent.Runner, error) {
					runner, err := agent.NewConfiguredRunnerWithOptions(agentName, cfg, local, agent.RunnerOptions{LookupEnv: lookup})
					return recordingRunner{Runner: runner, record: func(path string) {
						run.mu.Lock()
						defer run.mu.Unlock()
						run.seen = append(run.seen, path)
					}}, err
				}
				f.must(t, "pr", "work", strconv.Itoa(number), "--agent", "mock", "--progress=false")
			})
		}
	})
	for name, run := range runs {
		own := false
		for _, path := range run.seen {
			own = own || strings.HasPrefix(path, run.root)
			for other, o := range runs {
				if other != name && strings.HasPrefix(path, o.root) {
					t.Fatalf("%s runner hook served %s's worktree %s", name, other, path)
				}
			}
		}
		if !own {
			t.Fatalf("%s runner hook never served its own PR worktree: %q", name, run.seen)
		}
	}
}

type recordingRunner struct {
	agent.Runner
	record func(worktree string)
}

func (r recordingRunner) Run(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
	r.record(req.WorktreePath)
	return r.Runner.Run(ctx, req)
}

// TestCLIInjectedValidationExecutionMode supplies execution mode only through
// the fixture lookup. These commands run no agent, so an unsupported mode can
// only be rejected by validation's own mode resolution.
func TestCLIInjectedValidationExecutionMode(t *testing.T) {
	t.Parallel()
	const invalid = "injected-invalid-mode"
	expectRejected := func(t *testing.T, f *cliFixture, args ...string) {
		t.Helper()
		f.setenv("SWARM_EXECUTION_MODE", invalid)
		defer f.setenv("SWARM_EXECUTION_MODE", contract.ExecutionTrustedLocal)
		out, err := f.run(args...)
		if err == nil || !strings.Contains(out+err.Error(), `execution mode "`+invalid+`"`) {
			t.Fatalf("swarm %v ran validation without the injected mode: %v\n%s", args, err, out)
		}
	}
	t.Run("issue", func(t *testing.T) {
		t.Parallel()
		f := newCLIFixture(t)
		number := f.create(t, "agent")
		n := strconv.Itoa(number)
		f.must(t, "issue", "approve", n)
		f.must(t, "issue", "work", n, "--agent", "mock", "--headless", "--progress=false")
		expectRejected(t, f, "issue", "validate", n)
		f.must(t, "issue", "validate", n)
		expectRejected(t, f, "issue", "submit", n)
		f.must(t, "issue", "submit", n)
	})
	t.Run("interactive stage", func(t *testing.T) {
		t.Parallel()
		f := newCLIFixture(t)
		cfgPath := filepath.Join(f.root, ".swarm", "config.yaml")
		cfg, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfgPath, []byte(strings.Replace(string(cfg), "plan_mode: auto", "plan_mode: interactive", 1)), 0o600); err != nil {
			t.Fatal(err)
		}
		prGit(t, f.root, "add", ".swarm/config.yaml")
		prGit(t, f.root, "commit", "-m", "Approve interactive plan policy")
		number := f.create(t, "human")
		n := strconv.Itoa(number)
		f.must(t, "issue", "approve", n)
		f.must(t, "issue", "claim", n, "--agent", "mock")
		tree := workspace.WorktreePath(f.root, number)
		for name, body := range map[string]string{".swarm/active/PLAN.md": "# Plan\n", ".swarm/active/TEST_PLAN.md": "# Test plan\n"} {
			if err := os.WriteFile(filepath.Join(tree, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		f.must(t, "issue", "stage", n, "--phase", "plan")
		f.setenv("SWARM_USER", "independent-maintainer")
		f.must(t, "issue", "review-plan", n, "--approve", "--notes", "Checked plan")
		f.setenv("SWARM_USER", "alice")
		if err := os.WriteFile(filepath.Join(tree, "staged.txt"), []byte("implementation\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		expectRejected(t, f, "issue", "stage", n, "--phase", "implement")
		f.must(t, "issue", "stage", n, "--phase", "implement")
		f.must(t, "issue", "submit", n)
	})
	t.Run("pr", func(t *testing.T) {
		t.Parallel()
		f, _, number := submittedPRFixture(t, "agent")
		p := strconv.Itoa(number)
		f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'PR update\\n' > pr-update.txt")
		f.must(t, "pr", "work", p, "--agent", "mock", "--progress=false")
		expectRejected(t, f, "pr", "submit", p, "--progress=false")
		expectRejected(t, f, "pr", "validate", p)
		f.must(t, "pr", "validate", p)
		f.must(t, "pr", "submit", p, "--progress=false")
	})
}

// TestCLIFixtureMockControlsStayOutOfChildProcesses checks that a mock
// developer command set on a fixture never appears in its own environment.
func TestCLIFixtureMockControlsStayOutOfChildProcesses(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	dump := filepath.Join(t.TempDir(), "env.txt")
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "env > "+strconv.Quote(dump)+"; printf 'change\\n' > change.txt")
	n := strconv.Itoa(f.create(t, "agent"))
	f.must(t, "issue", "approve", n)
	f.must(t, "issue", "work", n, "--agent", "mock", "--headless", "--progress=false")
	env, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("fixture developer command did not run: %v", err)
	}
	for _, leaked := range []string{"SWARM_MOCK_", "SWARM_EXECUTION_MODE", "SWARM_USER=alice"} {
		if strings.Contains(string(env), leaked) {
			t.Fatalf("fixture control %s reached the developer command environment:\n%s", leaked, env)
		}
	}
	if _, ok := os.LookupEnv("SWARM_MOCK_DEVELOPER_CMD"); ok {
		t.Fatal("fixture control changed the process environment")
	}
}
