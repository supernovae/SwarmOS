package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"github.com/supernovae/swarmos/internal/tracker"
	"gopkg.in/yaml.v3"
)

var cliParallelStartup cliTestParallelDecision

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
	if !flag.Parsed() {
		flag.Parse()
	}
	cliParallelStartup, err = applyCLITestParallel(flag.CommandLine, runtime.GOOS, func() cliTestParallelObservation { return observeCLITestParallel(realCLIParallelFiles()) })
	if err == nil {
		var data []byte
		data, err = json.Marshal(cliParallelStartup)
		if err == nil && len(data) <= 1024 {
			err = os.WriteFile(filepath.Join(root, "parallel-policy.json"), data, 0600)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "TestMain: parallel policy:", err)
		os.RemoveAll(root)
		os.Exit(1)
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
	// Serial: the interactive phase below swaps the process stdin.
	env := newFixtureEnv(t)
	env.set("SWARM_EXECUTION_MODE", "isolated")
	env.set("SWARM_USER", "native-worker")
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := gitfixture.Command(t, "", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
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
	tr := tracker.NewLocalTracker(root).WithUser("native-worker", "")
	cfg, err := harness.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	manager := lease.NewManager(tr, cfg.Security)
	if err := manager.ConfigureStore(root); err != nil {
		t.Fatal(err)
	}
	f := &cliFixture{root: root, tracker: tr, manager: manager, env: env}
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
	f.setenv("SWARM_USER", "native-maintainer")
	f.must(t, "issue", "review-plan", n, "--approve", "--notes", "Verified the bounded greeting plan")
	f.setenv("SWARM_USER", "native-worker")
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

// Child-only work is selected exactly; marker-free execution is a no-op.
func TestCLITestParallelChild(t *testing.T) {
	marker := os.Getenv("SWARM_CLI_PARALLEL_CHILD")
	if marker == "" {
		return
	}
	value, err := strconv.Atoi(flag.Lookup("test.parallel").Value.String())
	if err != nil || value != cliParallelStartup.Final {
		t.Fatal("startup value mismatch")
	}
	root := filepath.Dir(os.Getenv("HOME"))
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "APPDATA"} {
		p := os.Getenv(name)
		info, err := os.Stat(p)
		if err != nil || !info.IsDir() || filepath.Dir(p) != root {
			t.Fatal("isolated directory unavailable")
		}
	}
	data, err := os.ReadFile(filepath.Join(root, "parallel-policy.json"))
	if err != nil || len(data) > 1024 {
		t.Fatal("startup telemetry unavailable")
	}
	var decision cliTestParallelDecision
	if json.Unmarshal(data, &decision) != nil || decision != cliParallelStartup {
		t.Fatal("startup telemetry mismatch")
	}
	report := struct {
		Root     string
		Decision cliTestParallelDecision
	}{root, decision}
	data, err = json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("SWARM_CLI_PARALLEL_REPORT"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if marker == "fail" {
		t.Fatal("requested child failure")
	}
}

type cliParallelOutput struct {
	bytes.Buffer
	overflow bool
}

func (b *cliParallelOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 64*1024 {
		b.overflow = true
		return 0, errors.New("child output budget")
	}
	return b.Buffer.Write(p)
}
func TestCLITestParallelTestMain(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	original, _ := strconv.Atoi(flag.Lookup("test.parallel").DefValue)
	for _, tc := range []struct {
		name string
		args []string
		want int
		fail bool
	}{
		{"implicit", nil, 0, false},
		{"one", []string{"-test.parallel", "1"}, 1, false},
		{"sixteen", []string{"-test.parallel=16"}, 16, false},
		{"double dash", []string{"--test.parallel=4"}, 4, false},
		{"duplicate", []string{"-test.parallel=8", "-test.parallel=2"}, 2, false},
		{"equal default", []string{"-test.parallel=8", "-test.parallel=" + strconv.Itoa(original)}, original, false},
		{"failure", []string{"-test.parallel=16"}, 16, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := filepath.Join(t.TempDir(), "report.json")
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			args := append([]string{"-test.run=^TestCLITestParallelChild$"}, tc.args...)
			cmd := exec.CommandContext(ctx, exe, args...)
			marker := "ok"
			if tc.fail {
				marker = "fail"
			}
			cmd.Env = append(os.Environ(), "SWARM_CLI_PARALLEL_CHILD="+marker, "SWARM_CLI_PARALLEL_REPORT="+report)
			var out cliParallelOutput
			cmd.Stdout = &out
			cmd.Stderr = &out
			err := cmd.Run()
			if (err != nil) != tc.fail || out.overflow || ctx.Err() != nil {
				t.Fatal("child exit/output failure", err)
			}
			data, err := os.ReadFile(report)
			if err != nil {
				t.Fatal(err)
			}
			var r struct {
				Root     string
				Decision cliTestParallelDecision
			}
			if json.Unmarshal(data, &r) != nil {
				t.Fatal("invalid child report")
			}
			if tc.want != 0 && (r.Decision.Final != tc.want || r.Decision.Reason != "explicit") {
				t.Fatal("explicit value changed")
			}
			if tc.want == 0 {
				t.Logf("implicit parallel policy: original=%d final=%d reason=%s", r.Decision.Original, r.Decision.Final, r.Decision.Reason)
				switch r.Decision.Reason {
				case "unchanged", "capped", "admitted", "fallback":
				default:
					t.Fatal("invalid implicit reason")
				}
			}
			if _, err := os.Stat(r.Root); !os.IsNotExist(err) {
				t.Fatal("TestMain root survived child exit")
			}
		})
	}
	for _, args := range [][]string{
		{"-test.parallel=0"}, {"-test.parallel=-1"}, {"-test.parallel=bad"}, {"-test.parallel=999999999999999999999999999999999"},
		{"-test.parallel=bad", "-test.parallel=1"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		cmd := exec.CommandContext(ctx, exe, append([]string{"-test.run=^TestCLITestParallelChild$"}, args...)...)
		var out cliParallelOutput
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()
		cancel()
		if err == nil || out.overflow {
			t.Fatal("invalid flag accepted or output unbounded")
		}
	}
	for _, args := range [][]string{{"-help"}, {"internal-tools", "--help"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		cmd := exec.CommandContext(ctx, exe, args...)
		var out cliParallelOutput
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()
		cancel()
		if err != nil || out.overflow || out.Len() == 0 {
			t.Fatal("help/early dispatch failed", err)
		}
	}
	// Drive both existing isolation tests with the admitted decision and one slot.
	admitted := decideCLITestParallel(2, false, "linux", cliTestParallelObservation{valid: true, memory: 4294967296, nproc: 4096})
	if admitted.Final != 16 {
		t.Fatal("deterministic admission failed")
	}
	for _, n := range []int{1, admitted.Final} {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		cmd := exec.CommandContext(ctx, exe, "-test.run=^(TestCLIParallelFixtureIsolation|TestCLIResolverRunnerIsolation)$", "-test.parallel="+strconv.Itoa(n))
		var out cliParallelOutput
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()
		cancel()
		if err != nil || out.overflow {
			t.Fatal("isolation child failed", err)
		}
	}
}
