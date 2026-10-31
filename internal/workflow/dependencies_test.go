package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/supernovae/swarmos/internal/storageuse"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/registry"
	"github.com/supernovae/swarmos/internal/sandbox"
	"github.com/supernovae/swarmos/internal/tracker"
)

// dependencyRecorder records the dependency caches each agent run receives.
type dependencyRecorder struct {
	agent.Runner
	run func(agent.RunRequest)
}

func (r dependencyRecorder) Run(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
	r.run(req)
	return r.Runner.Run(ctx, req)
}

func approvedDependencyTask(t *testing.T) (*Engine, string, int) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	if out, err := gitfixture.Command(t, "", "init", "-b", "main", tmp).CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v (%s)", err, out)
	}
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: tmp}); err != nil {
		t.Fatal(err)
	}
	configureValidation(t, tmp)
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "initialize harness"}} {
		if out, err := gitfixture.Command(t, "", append([]string{"-C", tmp}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	lt := tracker.NewLocalTracker(tmp).WithUser("local-dev", "OWNER")
	wu := &contract.WorkUnit{Objective: "Bump a dependency", Constraints: []string{"Stdlib only"}, Acceptance: []string{"Builds"}, Workflow: "feature", Gate: contract.GateAgent}
	iss, err := lt.CreateIssue(ctx, "Bump a dependency", contract.FormatIssueMarkdown(wu), []string{"swarm:ready"})
	if err != nil {
		t.Fatal(err)
	}
	eng, err := NewEngineWithOptions(tmp, lt, EngineOptions{NodeProfilePath: filepath.Join(t.TempDir(), "node.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.LeaseManager.ApproveTask(ctx, iss.Number, "local-dev"); err != nil {
		t.Fatal(err)
	}
	return eng, tmp, iss.Number
}

// stubAgentDependencies changes a package-global hook; callers must remain serial.
func stubAgentDependencies(t *testing.T, stub func(context.Context, contract.ExecutionConfig, string, string, sandbox.DependencyPolicy) (sandbox.Dependencies, error)) {
	t.Helper()
	previous := prepareAgentDependencies
	prepareAgentDependencies = stub
	t.Cleanup(func() { prepareAgentDependencies = previous })
}

func TestPreflightStopsBeforeClaimWhenDependencyCacheFails(t *testing.T) {
	eng, root, issue := approvedDependencyTask(t)
	ctx := context.Background()
	stubAgentDependencies(t, func(context.Context, contract.ExecutionConfig, string, string, sandbox.DependencyPolicy) (sandbox.Dependencies, error) {
		return sandbox.Dependencies{}, errors.New("prefetch go modules for base abc: example.com/missing@v1.0.0: not found")
	})
	ran := false
	eng.NewRunner = func(name string, cfg *contract.SwarmConfig, local bool) (agent.Runner, error) {
		r, err := agent.NewConfiguredRunner(name, cfg, local)
		return dependencyRecorder{Runner: r, run: func(agent.RunRequest) { ran = true }}, err
	}
	_, err := eng.ExecuteWork(ctx, WorkOptions{RepoRoot: root, IssueNumber: issue, AgentName: "mock", Phase: "plan", Headless: true})
	if err == nil || !strings.Contains(err.Error(), "dependency cache preflight") || !strings.Contains(err.Error(), "example.com/missing@v1.0.0") {
		t.Fatalf("ExecuteWork error = %v, want a dependency preflight error naming the module", err)
	}
	if ran {
		t.Fatal("an agent started after the dependency cache failed")
	}
	st, err := eng.LeaseManager.GetCurrentState(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	if st.Lease.ClaimID != "" {
		t.Fatalf("preflight failure acquired claim %q", st.Lease.ClaimID)
	}
	if _, err := eng.Preflight(ctx, WorkOptions{RepoRoot: root, IssueNumber: issue, AgentName: "mock", Phase: "plan", InspectOnly: true}); err != nil {
		t.Fatalf("inspection must not prefetch: %v", err)
	}
}

func TestAgentsStartWithPrefetchedDependencies(t *testing.T) {
	eng, root, issue := approvedDependencyTask(t)
	cache := filepath.Join(t.TempDir(), "agent-deps", "go")
	var bases []string
	stubAgentDependencies(t, func(_ context.Context, _ contract.ExecutionConfig, repo, base string, policy sandbox.DependencyPolicy) (sandbox.Dependencies, error) {
		bases = append(bases, base)
		if policy.Mode != sandbox.PerRepository || len(policy.PrivateGo.Patterns) != 0 {
			t.Errorf("policy = %+v, want the per-repository default", policy)
		}
		// Stands in for the host prefetch: the module appears only now.
		if err := os.MkdirAll(filepath.Join(cache, "example.com", "needed@v1.0.0"), 0o700); err != nil {
			return sandbox.Dependencies{}, err
		}
		return sandbox.Dependencies{Mode: policy.Mode, Caches: []sandbox.DependencyCache{{Ecosystem: "go", EnvKey: "GOMODCACHE", Dir: cache}}}, nil
	})
	var seen []sandbox.Dependencies
	eng.NewRunner = func(name string, cfg *contract.SwarmConfig, local bool) (agent.Runner, error) {
		r, err := agent.NewConfiguredRunner(name, cfg, local)
		return dependencyRecorder{Runner: r, run: func(req agent.RunRequest) {
			if _, err := os.Stat(filepath.Join(cache, "example.com", "needed@v1.0.0")); err != nil {
				t.Errorf("%s started before its module was prefetched: %v", req.Role, err)
			}
			seen = append(seen, req.Dependencies)
		}}, err
	}
	res, err := eng.ExecuteWork(context.Background(), WorkOptions{RepoRoot: root, IssueNumber: issue, AgentName: "mock", Phase: "plan", Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) == 0 {
		t.Fatal("no agent ran")
	}
	for _, deps := range seen {
		if len(deps.Caches) != 1 || deps.Caches[0].Dir != cache {
			t.Fatalf("agent dependencies = %+v", deps)
		}
	}
	for _, base := range bases {
		if base != res.BaseSHA {
			t.Fatalf("prefetched base %s, want the approved base %s", base, res.BaseSHA)
		}
	}
}

func TestPrepareAgentDependenciesOnlyForNativeIsolation(t *testing.T) {
	previous := userCacheDir
	userCacheDir = func() (string, error) { return "", errors.New("must not be read") }
	t.Cleanup(func() { userCacheDir = previous })
	for _, execution := range []contract.ExecutionConfig{
		{Mode: contract.ExecutionTrustedLocal},
		{Mode: contract.ExecutionIsolated, Backend: contract.BackendOCI},
	} {
		deps, err := prepareAgentDependencies(context.Background(), execution, t.TempDir(), "abc", sandbox.DependencyPolicy{Mode: sandbox.PerRepository})
		if err != nil || len(deps.Caches) != 0 {
			t.Fatalf("%+v: deps = %+v, %v", execution, deps, err)
		}
	}
}

func TestAgentDependenciesUseRepositoryPolicy(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	if out, err := gitfixture.Command(t, "", "init", "-q", "-b", "main", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v (%s)", err, out)
	}
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: repo}); err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(t.TempDir(), "repos.yaml")
	if _, err := registry.New(registryPath).Add("acme/a", repo, "local"); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(t.TempDir(), "node.yaml")
	if err := os.WriteFile(profile, []byte("node_id: n\nprivate_go_modules:\n  acme/a: [example.com/priv]\n  acme/b: [example.com/other]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := EngineOptions{NodeProfilePath: profile, RegistryPath: registryPath}
	lt := tracker.NewLocalTracker(repo).WithUser("local-dev", "OWNER")
	eng, err := NewEngineWithOptions(repo, lt, opts)
	if err != nil {
		t.Fatal(err)
	}
	want := sandbox.DependencyPolicy{Mode: sandbox.PerRepository, PrivateGo: sandbox.PrivateGo{Repository: "acme/a", Patterns: []string{"example.com/priv"}}}
	for name, e := range map[string]*Engine{"NewEngineWithOptions": eng, "NewEngineWithManagerOptions": NewEngineWithManagerOptions(repo, lt, nil, eng.Config, false, "", opts)} {
		var got sandbox.DependencyPolicy
		stubAgentDependencies(t, func(_ context.Context, _ contract.ExecutionConfig, _, _ string, policy sandbox.DependencyPolicy) (sandbox.Dependencies, error) {
			got = policy
			return sandbox.Dependencies{Mode: policy.Mode}, nil
		})
		if _, err := e.AgentDependencies(ctx, e.Config, repo, "abc"); err != nil {
			t.Fatal(err)
		}
		if got.Mode != want.Mode || got.PrivateGo.Repository != want.PrivateGo.Repository || strings.Join(got.PrivateGo.Patterns, ",") != "example.com/priv" {
			t.Fatalf("%s: agent prefetch policy = %+v, want %+v", name, got, want)
		}
	}
}

// The enclosing engine owns preflight admission through the last role, without
// placing completion authority in the worker-visible Dependencies value.
func TestEngineDependencyScopeLivesThroughRoles(t *testing.T) {
	rootHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CACHE_HOME", rootHome)
	if runtime.GOOS == "darwin" {
		t.Setenv("HOME", rootHome)
	}
	eng, root, issue := approvedDependencyTask(t)
	cache := filepath.Join(t.TempDir(), "cache")
	var resource storageuse.Resource
	stubAgentDependencies(t, func(ctx context.Context, _ contract.ExecutionConfig, _, _ string, policy sandbox.DependencyPolicy) (sandbox.Dependencies, error) {
		scope := storageuse.FromContext(ctx)
		if scope == nil {
			t.Fatal("preflight has no enclosing owner")
		}
		resource = storageuse.ResourceFor(scope.Repository(), "agent-deps", cache)
		if err := scope.Acquire(ctx, resource); err != nil {
			return sandbox.Dependencies{}, err
		}
		return sandbox.Dependencies{Mode: policy.Mode}, nil
	})
	checked := false
	eng.NewRunner = func(name string, cfg *contract.SwarmConfig, local bool) (agent.Runner, error) {
		runner, err := agent.NewConfiguredRunner(name, cfg, local)
		return dependencyRecorder{Runner: runner, run: func(agent.RunRequest) {
			control, _ := storageuse.ControlRoot()
			m, err := storageuse.Open(control)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if guard, err := m.AcquireMaintenance(context.Background(), resource); !errors.Is(err, storageuse.ErrUnsafe) {
				if guard != nil {
					guard.Close()
				}
				t.Fatalf("preflight scope ended before role: %v", err)
			}
			checked = true
		}}, err
	}
	if _, err := eng.ExecuteWork(context.Background(), WorkOptions{RepoRoot: root, IssueNumber: issue, AgentName: "mock", Phase: "plan", Headless: true}); err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("no role checked lifetime")
	}
	control, _ := storageuse.ControlRoot()
	records, err := filepath.Glob(filepath.Join(control, "*.json"))
	if err != nil || len(records) == 0 {
		t.Fatalf("no evidence: %v", err)
	}
	for _, path := range records {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var r struct{ Uses map[string]bool }
		if err := json.Unmarshal(data, &r); err != nil {
			t.Fatal(err)
		}
		for _, done := range r.Uses {
			if !done {
				t.Fatalf("ordinary engine completion left pending record %s", filepath.Base(path))
			}
		}
	}
}
