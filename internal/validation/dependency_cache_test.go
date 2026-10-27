package validation

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/sandbox"
)

// goModuleValidationRepo commits gomod and a policy whose check prints
// GOMODCACHE, against a host whose own module cache is hostCache.
func goModuleValidationRepo(t *testing.T, gomod string) (repo, base, hostCache string) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH")
	}
	hostCache = filepath.Join(t.TempDir(), "host-gomodcache")
	if err := os.MkdirAll(filepath.Join(hostCache, "example.com", "private@v1.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOMODCACHE", hostCache)
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOFLAGS", "")
	repo = setupGitRepoForValidation(t)
	commitRepoChange(t, repo, "go.mod", gomod)
	policy := "required_checks:\n  - name: env\n    command: 'printf \"GOMODCACHE=%s\\n\" \"$GOMODCACHE\"'\n    timeout: 1m\nprotected_paths:\n  - .github/workflows/**\n"
	return repo, commitPolicy(t, repo, policy), hostCache
}

func TestValidateUsesValidatorDependencyCache(t *testing.T) {
	repo, base, hostCache := goModuleValidationRepo(t, "module example.com/v\n\ngo 1.21\n")
	for _, tc := range []struct {
		mode   sandbox.DependencyCacheMode
		shared bool
	}{{"", false}, {sandbox.PerRepository, false}, {sandbox.SharedHostCaches, true}} {
		cacheRoot := useCacheRoot(t)
		report, err := Validate(context.Background(), repo, nil, ValidateOptions{CIMode: true, BaseSHA: base, DependencyPolicy: sandbox.DependencyPolicy{Mode: tc.mode}})
		if err != nil || !report.Passed {
			t.Fatalf("mode %q: validate = %+v, %v", tc.mode, report, err)
		}
		got := checkEnvOutput(t, report.CheckOutcomes[0].Output, "GOMODCACHE")
		if tc.shared {
			if got != hostCache {
				t.Fatalf("shared mode GOMODCACHE = %q, want the host cache", got)
			}
			continue
		}
		want := filepath.Join(cacheRoot, "swarm", string(sandbox.ValidatorDependencyCache))
		if !strings.HasPrefix(got, want+string(filepath.Separator)) || filepath.Base(got) != "go" {
			t.Fatalf("mode %q: GOMODCACHE = %q, want the validator cache under %s", tc.mode, got, want)
		}
	}
}

func TestValidatePrefetchFailureStopsBeforeChecks(t *testing.T) {
	repo, base, _ := goModuleValidationRepo(t, "module example.com/v\n\ngo 1.21\n\nrequire example.com/missing v1.0.0\n")
	useCacheRoot(t)
	var started []string
	report, err := Validate(context.Background(), repo, nil, ValidateOptions{CIMode: true, BaseSHA: base, OnProgress: recordEvents(&started)})
	if err == nil || report != nil || !strings.Contains(err.Error(), "example.com/missing@v1.0.0") {
		t.Fatalf("validate = %+v, %v; want a preflight error naming the module", report, err)
	}
	if len(started) != 0 {
		t.Fatalf("checks ran after a failed prefetch: %v", started)
	}
}

func TestCleanCheckEnvUsesDependencyCaches(t *testing.T) {
	hostCache := filepath.Join(t.TempDir(), "host-gomodcache")
	if err := os.MkdirAll(hostCache, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOMODCACHE", hostCache)
	deps := sandbox.Dependencies{Mode: sandbox.PerRepository, Caches: []sandbox.DependencyCache{{Ecosystem: "go", EnvKey: "GOMODCACHE", Dir: "/cache/validate-deps/key/go"}}}
	env := cleanCheckEnv("/scratch/home", nil, deps)
	var values []string
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "GOMODCACHE="); ok {
			values = append(values, value)
		}
	}
	if !slices.Equal(values, []string{"/cache/validate-deps/key/go"}) {
		t.Fatalf("GOMODCACHE entries = %q", values)
	}
	if env := cleanCheckEnv("/scratch/home", nil, sandbox.Dependencies{Mode: sandbox.SharedHostCaches}); !slices.Contains(env, "GOMODCACHE="+hostCache) {
		t.Fatalf("shared mode env = %q", env)
	}
}

// The validator prefetch applies the repository's private Go module policy:
// the preflight error names the repository the policy was resolved for.
func TestValidatorDependenciesUseRepositoryPolicy(t *testing.T) {
	repo, base, _ := goModuleValidationRepo(t, "module example.com/v\n\ngo 1.21\n\nrequire example.com/priv/lib v1.0.0\n")
	useCacheRoot(t)
	t.Setenv("GOPRIVATE", "example.com/priv")
	t.Setenv("GONOPROXY", "")
	for _, patterns := range [][]string{nil, {"example.com/other"}} {
		policy := sandbox.DependencyPolicy{Mode: sandbox.PerRepository, PrivateGo: sandbox.PrivateGo{Repository: "acme/a", Patterns: patterns}}
		var started []string
		report, err := Validate(context.Background(), repo, nil, ValidateOptions{CIMode: true, BaseSHA: base, OnProgress: recordEvents(&started), DependencyPolicy: policy})
		if err == nil || report != nil || !strings.Contains(err.Error(), "example.com/priv/lib@v1.0.0") || !strings.Contains(err.Error(), `private_go_modules["acme/a"]`) {
			t.Fatalf("patterns %q: validate = %+v, %v; want the private module preflight error for acme/a", patterns, report, err)
		}
		if len(started) != 0 {
			t.Fatalf("checks ran after a failed preflight: %v", started)
		}
	}
}

func TestCleanCheckEnvHasNoPrefetchCredentials(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "/tmp/ssh-agent.sock")
	t.Setenv("NETRC", "/home/user/.netrc")
	t.Setenv("GOAUTH", "netrc")
	deps := sandbox.Dependencies{Mode: sandbox.PerRepository, Caches: []sandbox.DependencyCache{{Ecosystem: "go", EnvKey: "GOMODCACHE", Dir: "/cache/validate-deps/key/go"}}}
	for _, entry := range cleanCheckEnv("/scratch/home", nil, deps) {
		key, _, _ := strings.Cut(entry, "=")
		if key == "SSH_AUTH_SOCK" || key == "NETRC" || key == "GOAUTH" {
			t.Errorf("check environment passes %s", entry)
		}
	}
}
