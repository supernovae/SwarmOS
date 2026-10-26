package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"gopkg.in/yaml.v3"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "internal-tools" {
		if err := NewRootCmd().Execute(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	// Point every user directory at one temporary root before any test runs, so
	// parallel tests write nothing under the real home, config, state, app-data
	// or cache directories. That includes the persistent Go build cache that host
	// validation creates under XDG_CACHE_HOME, claim records under the user
	// config directory, agent diagnostics under XDG_STATE_HOME and the global Git
	// configuration that fixture Git commands read from HOME.
	root, err := os.MkdirTemp("", "swarm-test-home-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "TestMain:", err)
		os.Exit(1)
	}
	for _, dir := range []struct{ name, sub string }{
		{"HOME", "home"},
		{"XDG_CONFIG_HOME", "config"},
		{"XDG_STATE_HOME", "state"},
		{"XDG_CACHE_HOME", "cache"},
		{"APPDATA", "appdata"},
	} {
		path := filepath.Join(root, dir.sub)
		if err := os.Mkdir(path, 0o700); err != nil {
			fmt.Fprintln(os.Stderr, "TestMain:", err)
			os.RemoveAll(root)
			os.Exit(1)
		}
		if err := os.Setenv(dir.name, path); err != nil {
			fmt.Fprintln(os.Stderr, "TestMain:", err)
			os.RemoveAll(root)
			os.Exit(1)
		}
	}
	code := m.Run()
	os.RemoveAll(root)
	os.Exit(code)
}

// This opt-in test uses the operator's existing Codex login and model quota.
// Coordination and Draft PR creation stay in a disposable local fixture.
func TestNativeCodexAuthenticatedWorkUnit(t *testing.T) {
	if os.Getenv("SWARM_TEST_CODEX_AUTHENTICATED") != "1" {
		t.Skip("set SWARM_TEST_CODEX_AUTHENTICATED=1 to use your Codex login")
	}
	authenticatedWorkUnit(t, "codex")
}

func TestMixedHarnessAuthenticatedWorkUnit(t *testing.T) {
	if os.Getenv("SWARM_TEST_MIXED_AUTHENTICATED") != "1" {
		t.Skip("set SWARM_TEST_MIXED_AUTHENTICATED=1 to use all four existing harness logins")
	}
	authenticatedWorkUnit(t, "mixed")
}

func TestDelegatedAuthenticatedWorkUnit(t *testing.T) {
	driver := os.Getenv("SWARM_TEST_WORKFLOW_HARNESS")
	if driver == "" {
		t.Skip("set SWARM_TEST_WORKFLOW_HARNESS=claude, opencode or omp for all-role authenticated qualification")
	}
	if driver != "claude" && driver != "opencode" && driver != "omp" {
		t.Fatal("unsupported harness")
	}
	authenticatedWorkUnit(t, driver)
}

func authenticatedWorkUnit(t *testing.T, routing string) {
	t.Setenv("SWARM_EXECUTION_MODE", "isolated")
	t.Setenv("SWARM_USER", "native-worker")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-b", "main")
	git("config", "user.name", "Native Integration")
	git("config", "user.email", "native@example.invalid")
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	if routing != "codex" {
		cfg, err := harness.LoadConfig(root)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Roles = map[string]string{"planner": "claude", "reviewer": "codex", "developer": "opencode", "tester": "omp"}
		if routing != "mixed" {
			cfg.DefaultAgent = routing
			for role := range cfg.Roles {
				cfg.Roles[role] = routing
			}
		}
		for name, ac := range cfg.Agents {
			if value := os.Getenv("SWARM_TEST_" + strings.ToUpper(name) + "_BINARY"); value != "" {
				ac.Binary = value
			}
			if value := os.Getenv("SWARM_TEST_" + strings.ToUpper(name) + "_MODEL"); value != "" {
				ac.Model = value
			}
			cfg.Agents[name] = ac
		}
		data, err := yaml.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".swarm/config.yaml"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	policy := "required_checks:\n  - name: greeting\n    command: test \"$(cat hello.txt)\" = 'hello from SwarmOS'\n    timeout: 30s\nprotected_paths: ['.swarm/**', '.github/**']\n"
	if err := os.WriteFile(filepath.Join(root, ".swarm/validation/required-checks.yaml"), []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "approved native workflow fixture")
	tr := tracker.NewLocalTracker(root)
	cfg, err := harness.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	manager := lease.NewManager(tr, cfg.Security)
	if err := manager.ConfigureStore(root); err != nil {
		t.Fatal(err)
	}
	f := &cliFixture{root: root, tracker: tr, manager: manager}
	f.must(t, "repo", "doctor")
	f.must(t, "issue", "create", "--title", "Native greeting", "--objective", "Create hello.txt containing exactly hello from SwarmOS followed by a newline.", "--constraint", "Keep repository policy unchanged. Make only this small text-file change.", "--acceptance", "The greeting required check passes.", "--gate", "human")
	issues, err := tr.ListIssues(context.Background(), tracker.IssueFilter{State: "open"})
	if err != nil || len(issues) != 1 {
		t.Fatal("missing fixture issue", err)
	}
	number := issues[0].Number
	n := strconv.Itoa(number)
	f.must(t, "issue", "approve", n)
	// Exercise the same streamed interactive phase used by --headless=false.
	// Empty input accepts the completed phase without widening tool permissions.
	input, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if _, err := input.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	oldStdin := os.Stdin
	os.Stdin = input
	t.Cleanup(func() { os.Stdin = oldStdin })
	f.must(t, "issue", "work", n, "--phase", "plan", "--headless=false")
	if _, err := f.run("issue", "submit", n); err == nil {
		t.Fatal("submission bypassed human review")
	}
	f.must(t, "issue", "review-plan", n)
	if _, err := f.run("issue", "work", n, "--phase", "implement"); err == nil {
		t.Fatal("agent review satisfied the human gate")
	}
	t.Setenv("SWARM_USER", "native-maintainer")
	f.must(t, "issue", "review-plan", n, "--approve", "--notes", "Verified the bounded greeting plan")
	t.Setenv("SWARM_USER", "native-worker")
	f.must(t, "issue", "work", n, "--headless")
	f.must(t, "issue", "submit", n)
	state := f.state(t, number)
	if state.PRNumber == 0 || state.Lease.Status != "completed" {
		t.Fatalf("incomplete native workflow: %+v", state)
	}
	pr, err := tr.FindDraftPR(context.Background(), state.Branch, "main")
	if err != nil || pr == nil || pr.Number != state.PRNumber {
		t.Fatal("missing local Draft PR", err)
	}
	for _, artifact := range state.Artifacts {
		if artifact.Type == "design" || artifact.Type == "test-plan" || artifact.Type == "implementation" || artifact.Type == "validation" {
			want := "codex-native"
			if artifact.Metadata["harness"] != "codex" {
				want = "harness-tools-bwrap"
			}
			if artifact.Metadata["sandbox"] != want {
				t.Fatalf("missing native provenance: %+v", artifact)
			}
		}
	}
	t.Log("Authenticated planner (interactive), reviewer, human gate, developer, tester, isolated validation and local Draft PR completed; routing:", routing)
}
