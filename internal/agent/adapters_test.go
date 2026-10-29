package agent

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/sandbox"
	"github.com/supernovae/swarmos/internal/testutil"
)

func TestAgentEnvironmentHelper(t *testing.T) {
	if os.Getenv("SWARM_AGENT_ENV_HELPER") != "1" {
		return
	}
	if os.Getenv("GITHUB_TOKEN") != "" {
		os.Exit(23)
	}
	if os.Getenv("MODEL_API_KEY") != "model-secret" {
		os.Exit(24)
	}
	os.Exit(0)
}

// TestAgentLookupEnvironmentHelper is gated by argv, so a missing injected
// environment fails instead of skipping the checks.
func TestAgentLookupEnvironmentHelper(t *testing.T) {
	if !slices.Contains(os.Args, "--swarm-lookup-helper") {
		return
	}
	if os.Getenv("GITHUB_TOKEN") != "" {
		os.Exit(23)
	}
	if os.Getenv("MODEL_API_KEY") != "model-secret" {
		os.Exit(24)
	}
	os.Exit(0)
}

func TestAgentModelHelper(t *testing.T) {
	if os.Getenv("SWARM_AGENT_MODEL_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--model" && i+1 < len(os.Args) && os.Args[i+1] == "approved-model" {
			os.Exit(0)
		}
	}
	os.Exit(25)
}

func TestConfiguredRunnerRejectsUnapprovedOrUnavailableSelections(t *testing.T) {
	cfg := &contract.SwarmConfig{Execution: contract.ExecutionConfig{Mode: contract.ExecutionTrustedLocal}, Agents: map[string]contract.AgentConfig{
		"claude": {Binary: os.Args[0], Roles: []string{"planner"}, Model: "approved-model"},
	}}
	if _, err := NewConfiguredRunner("codex", cfg, false); err == nil {
		t.Fatal("unallowlisted adapter was accepted")
	}
	if _, err := Preflight("claude", cfg, false, "reviewer"); err == nil {
		t.Fatal("disallowed role was accepted")
	}
	if _, err := Preflight("claude", cfg, false, "planner"); err != nil {
		t.Fatalf("configured available planner rejected: %v", err)
	}
	if _, err := NewConfiguredRunner("mock", cfg, false); err == nil {
		t.Fatal("mock was accepted outside local backend")
	}
	if _, err := NewConfiguredRunner("mock", cfg, true); err != nil {
		t.Fatalf("local mock rejected: %v", err)
	}
}

func TestConfiguredRunnerRejectsMissingBinaryBeforeExecution(t *testing.T) {
	cfg := &contract.SwarmConfig{Execution: contract.ExecutionConfig{Mode: contract.ExecutionTrustedLocal}, Agents: map[string]contract.AgentConfig{"claude": {Binary: "definitely-not-a-swarm-agent"}}}
	if _, err := NewConfiguredRunner("claude", cfg, false); err == nil {
		t.Fatal("unavailable configured executable was accepted")
	}
}

func TestConfiguredRunnerRejectsPublishingCredentialForwarding(t *testing.T) {
	cfg := &contract.SwarmConfig{Execution: contract.ExecutionConfig{Mode: contract.ExecutionTrustedLocal}, Agents: map[string]contract.AgentConfig{"claude": {Binary: os.Args[0], Env: []string{"GITHUB_TOKEN"}}}}
	if _, err := NewConfiguredRunner("claude", cfg, false); err == nil {
		t.Fatal("publishing token was accepted as agent authentication")
	}
}

func TestRestrictedEnvRemovesPublishingCredentials(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "publish-secret")
	t.Setenv("SSH_AUTH_SOCK", "/host/agent.sock")
	t.Setenv("MODEL_API_KEY", "model-secret")
	t.Setenv("SWARM_GITHUB_TOKEN_COMMAND", "op read op://vault/github/token")
	env := restrictedEnv([]string{"MODEL_API_KEY"}, t.TempDir())
	seen := map[string]bool{}
	for _, entry := range env {
		seen[entry] = true
	}
	if seen["GITHUB_TOKEN=publish-secret"] {
		t.Fatal("publishing credential reached agent environment")
	}
	if !seen["MODEL_API_KEY=model-secret"] {
		t.Fatal("explicit model credential did not reach agent environment")
	}
	if seen["SSH_AUTH_SOCK=/host/agent.sock"] {
		t.Fatal("host SSH credential socket reached agent environment")
	}
	if seen["SWARM_GITHUB_TOKEN_COMMAND=op read op://vault/github/token"] {
		t.Fatal("coordinator credential command reached agent environment")
	}
}

// mapLookup is an injected environment holding exactly vars.
func mapLookup(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := vars[name]
		return value, ok
	}
}

func TestRestrictedEnvWithLookupKeepsAllowlist(t *testing.T) {
	// Conflicting process values must not reach the agent when a lookup is injected.
	t.Setenv("MODEL_API_KEY", "process-secret")
	t.Setenv("PATH", "/process/bin")
	home := t.TempDir()
	lookup := mapLookup(map[string]string{
		"PATH": "/injected/bin", "MODEL_API_KEY": "model-secret", "EMPTY_MODEL_KEY": "",
		"GITHUB_TOKEN": "publish-secret", "GH_TOKEN": "publish-secret", "SWARM_GITHUB_TOKEN_COMMAND": "op read op://vault/github/token",
		"SSH_AUTH_SOCK": "/host/agent.sock", "GIT_ASKPASS": "/host/askpass", "HOME": "/host/home", "XDG_CONFIG_HOME": "/host/config",
		"UNLISTED": "value", "SWARM_MOCK_DEVELOPER_CMD": "exit 1",
	})
	env := restrictedEnvWithLookup([]string{"MODEL_API_KEY", "EMPTY_MODEL_KEY", "UNSET_MODEL_KEY"}, home, lookup)
	got := map[string]string{}
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		got[name] = value
	}
	for name, want := range map[string]string{"PATH": "/injected/bin", "MODEL_API_KEY": "model-secret", "EMPTY_MODEL_KEY": "", "HOME": home, "XDG_CONFIG_HOME": filepath.Join(home, ".config")} {
		if value, ok := got[name]; !ok || value != want {
			t.Errorf("%s = %q (present %v), want %q", name, value, ok, want)
		}
	}
	for _, name := range []string{"UNSET_MODEL_KEY", "GITHUB_TOKEN", "GH_TOKEN", "SWARM_GITHUB_TOKEN_COMMAND", "SSH_AUTH_SOCK", "GIT_ASKPASS", "UNLISTED", "SWARM_MOCK_DEVELOPER_CMD"} {
		if value, ok := got[name]; ok {
			t.Errorf("%s=%q reached the agent environment", name, value)
		}
	}
}

func TestConfiguredRunnerWithLookupRejectsCredentialNames(t *testing.T) {
	lookup := mapLookup(map[string]string{"SWARM_EXECUTION_MODE": contract.ExecutionTrustedLocal})
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN", "SWARM_GITHUB_TOKEN_COMMAND", "GIT_ASKPASS", "SSH_AUTH_SOCK", "HOME", "XDG_CONFIG_HOME"} {
		t.Run(name, func(t *testing.T) {
			cfg := &contract.SwarmConfig{Agents: map[string]contract.AgentConfig{"claude": {Binary: os.Args[0], Env: []string{name}}}}
			lookup := func(key string) (string, bool) {
				if key == name {
					return "host-credential", true
				}
				return lookup(key)
			}
			if _, err := NewConfiguredRunnerWithOptions("claude", cfg, false, RunnerOptions{LookupEnv: lookup}); err == nil || !strings.Contains(err.Error(), "credential") {
				t.Fatalf("%s accepted as agent authentication: %v", name, err)
			}
			if _, err := PreflightWithOptions("claude", cfg, false, "planner", RunnerOptions{LookupEnv: lookup}); err == nil || !strings.Contains(err.Error(), "credential") {
				t.Fatalf("%s accepted by preflight: %v", name, err)
			}
		})
	}
}

func TestConfiguredRunnerExecutionModeLookup(t *testing.T) {
	cfg := &contract.SwarmConfig{Agents: map[string]contract.AgentConfig{}}
	t.Setenv("SWARM_EXECUTION_MODE", "process-mode")
	if _, err := NewConfiguredRunner("mock", cfg, true); err == nil || !strings.Contains(err.Error(), `"process-mode"`) {
		t.Fatalf("default runner did not read the process environment: %v", err)
	}
	trusted := RunnerOptions{LookupEnv: mapLookup(map[string]string{"SWARM_EXECUTION_MODE": contract.ExecutionTrustedLocal})}
	if _, err := NewConfiguredRunnerWithOptions("mock", cfg, true, trusted); err != nil {
		t.Fatalf("injected trusted-local mode was ignored: %v", err)
	}
	isolated := RunnerOptions{LookupEnv: mapLookup(map[string]string{"SWARM_EXECUTION_MODE": contract.ExecutionIsolated})}
	if _, err := NewConfiguredRunnerWithOptions("mock", cfg, true, isolated); err == nil || !strings.Contains(err.Error(), "trusted-local") {
		t.Fatalf("injected isolated mode admitted the host mock: %v", err)
	}
	if _, err := NewConfiguredRunnerWithOptions("mock", cfg, true, RunnerOptions{LookupEnv: mapLookup(nil)}); err == nil || !strings.Contains(err.Error(), "trusted-local") {
		t.Fatalf("unset injected mode did not default to isolated: %v", err)
	}
}

func TestAdapterProcessUsesLookupWithoutPublishingCredentials(t *testing.T) {
	lookup := mapLookup(map[string]string{
		"PATH": os.Getenv("PATH"), "MODEL_API_KEY": "model-secret", "GITHUB_TOKEN": "publish-secret",
	})
	runner := &CLIAdapter{agentName: "custom", binary: os.Args[0], prefixArgs: []string{"-test.run=^TestAgentLookupEnvironmentHelper$", "--", "--swarm-lookup-helper"}, envAllow: []string{"MODEL_API_KEY"}, lookupEnv: lookup}
	req := RunRequest{Role: "planner", WorktreePath: t.TempDir(), TaskPrompt: "task"}
	if _, args := runner.BuildCommand(req); strings.Contains(strings.Join(args, " "), "secret") {
		t.Fatalf("credential reached argv: %q", args)
	}
	if _, err := runner.Run(t.Context(), req); err != nil {
		t.Fatalf("injected model auth did not reach the agent, or a publishing credential did: %v", err)
	}
}

func TestMockRunnerControlsUseLookup(t *testing.T) {
	worktree := t.TempDir()
	envDump := filepath.Join(t.TempDir(), "env.txt")
	runner := &MockRunner{lookupEnv: mapLookup(map[string]string{
		"SWARM_MOCK_VERDICT":       "CHANGES_REQUESTED",
		"SWARM_MOCK_DEVELOPER_CMD": "env > " + strconv.Quote(envDump),
	})}
	if _, err := runner.Run(t.Context(), RunRequest{Role: "reviewer", WorktreePath: worktree}); err != nil {
		t.Fatal(err)
	}
	review, err := os.ReadFile(filepath.Join(worktree, ".swarm", "active", "REVIEW.md"))
	if err != nil || !strings.Contains(string(review), "VERDICT: CHANGES_REQUESTED") {
		t.Fatalf("injected verdict ignored: %v\n%s", err, review)
	}
	if _, err := runner.Run(t.Context(), RunRequest{Role: "developer", WorktreePath: worktree, TaskPrompt: "implement"}); err != nil {
		t.Fatal(err)
	}
	dump, err := os.ReadFile(envDump)
	if err != nil {
		t.Fatalf("injected developer command did not run: %v", err)
	}
	if strings.Contains(string(dump), "SWARM_MOCK_") {
		t.Fatalf("mock control reached the developer command environment:\n%s", dump)
	}
}

func TestAdapterProcessHidesPublishingCredentials(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "publish-secret")
	t.Setenv("SWARM_AGENT_ENV_HELPER", "1")
	t.Setenv("MODEL_API_KEY", "model-secret")
	runner := &CLIAdapter{agentName: "custom", binary: os.Args[0], prefixArgs: []string{"-test.run=TestAgentEnvironmentHelper", "--"}, envAllow: []string{"MODEL_API_KEY", "SWARM_AGENT_ENV_HELPER"}}
	if _, err := runner.Run(t.Context(), RunRequest{Role: "planner", WorktreePath: t.TempDir()}); err != nil {
		t.Fatalf("agent inherited prohibited credentials or lost explicit model auth: %v", err)
	}
}

func TestConfiguredModelReachesSupportedHarness(t *testing.T) {
	t.Setenv("SWARM_AGENT_MODEL_HELPER", "1")
	cfg := &contract.SwarmConfig{Execution: contract.ExecutionConfig{Mode: contract.ExecutionTrustedLocal}, Agents: map[string]contract.AgentConfig{
		"reviewer": {Binary: os.Args[0], Harness: "claude", Args: []string{"-test.run=TestAgentModelHelper", "--"}, Model: "approved-model"},
	}}
	runner, err := NewConfiguredRunner("reviewer", cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(t.Context(), RunRequest{Role: "reviewer", WorktreePath: t.TempDir(), Mode: "headless"}); err != nil {
		t.Fatalf("configured model did not reach supported harness: %v", err)
	}
}

func TestOpenCodeV2CommandsPreserveOptionalModel(t *testing.T) {
	for _, mode := range []string{"headless", "interactive"} {
		for _, model := range []string{"", "project-provider/project-model"} {
			adapter := &CLIAdapter{driver: "opencode", binary: "opencode"}
			_, args := adapter.BuildCommand(RunRequest{Mode: mode, Model: model, TaskPrompt: "task"})
			want := "run"
			if mode == "interactive" {
				want = "mini"
			}
			if len(args) < 3 || args[0] != want || args[1] != "--standalone" {
				t.Fatal(args)
			}
			found := false
			for i, arg := range args {
				if arg == "--model" {
					found = true
					if i == 0 || i+1 >= len(args) || args[i+1] != model {
						t.Fatal(args)
					}
				}
			}
			if found != (model != "") {
				t.Fatal("model selection changed", args)
			}
		}
	}
}

func TestAgentEffortHelper(t *testing.T) {
	if os.Getenv("SWARM_AGENT_EFFORT_HELPER") != "1" {
		return
	}
	model, effort := false, false
	for i, arg := range os.Args[:len(os.Args)-1] {
		model = model || arg == "--model" && os.Args[i+1] == "approved-model"
		effort = effort || arg == "--effort" && os.Args[i+1] == "high"
	}
	if model && effort {
		os.Exit(0)
	}
	os.Exit(25)
}

func TestEffortArgvMapsOnlySupportedFlag(t *testing.T) {
	for _, tc := range []struct {
		driver, effort string
		extra          []string
	}{
		{"claude", "", nil},
		{"claude", "max", []string{"--effort", "max"}},
		{"codex", "", nil},
		{"codex", "minimal", []string{"-c", `model_reasoning_effort="minimal"`}},
	} {
		for _, mode := range []string{"headless", "interactive"} {
			req := RunRequest{Mode: mode, Model: "m", TaskPrompt: "task"}
			_, base := (&CLIAdapter{driver: tc.driver, binary: tc.driver}).BuildCommand(req)
			_, got := (&CLIAdapter{driver: tc.driver, binary: tc.driver, effort: tc.effort}).BuildCommand(req)
			if want := append(slices.Clone(tc.extra), base...); !slices.Equal(got, want) {
				t.Fatalf("%s %s effort=%q:\n got %q\nwant %q", tc.driver, mode, tc.effort, got, want)
			}
		}
	}
}

func TestConfiguredRunnerRejectsInvalidEffort(t *testing.T) {
	for name, ac := range map[string]contract.AgentConfig{
		"claude": {Binary: os.Args[0], Effort: "minimal"},
		"codex":  {Binary: os.Args[0], Effort: "max"},
		"omp":    {Binary: os.Args[0], Effort: "high"},
	} {
		cfg := &contract.SwarmConfig{Execution: contract.ExecutionConfig{Mode: contract.ExecutionTrustedLocal}, Agents: map[string]contract.AgentConfig{name: ac}}
		if _, err := NewConfiguredRunner(name, cfg, false); err == nil || !strings.Contains(err.Error(), "effort") {
			t.Fatalf("%s accepted effort %q: %v", name, ac.Effort, err)
		}
	}
}

func TestConfiguredEffortReachesHarnessAndProvenance(t *testing.T) {
	t.Setenv("SWARM_AGENT_EFFORT_HELPER", "1")
	cfg := &contract.SwarmConfig{Execution: contract.ExecutionConfig{Mode: contract.ExecutionTrustedLocal}, Agents: map[string]contract.AgentConfig{
		"claude-planner": {Binary: os.Args[0], Harness: "claude", Args: []string{"-test.run=TestAgentEffortHelper", "--"}, Model: "approved-model", Effort: "high"},
	}}
	runner, err := NewConfiguredRunner("claude-planner", cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if att := runner.Provenance("planner", false); att.Model != "approved-model" || att.Effort != "high" {
		t.Fatalf("provenance = %+v", att)
	}
	res, err := runner.Run(t.Context(), RunRequest{Role: "planner", WorktreePath: t.TempDir(), Mode: "headless"})
	if err != nil {
		t.Fatalf("configured effort did not reach harness: %v", err)
	}
	if res.Attestation.Model != "approved-model" || res.Attestation.Effort != "high" {
		t.Fatalf("run attestation = %+v", res.Attestation)
	}
	if meta := res.Attestation.Metadata(nil); meta["effort"] != "high" || meta["model"] != "approved-model" {
		t.Fatalf("metadata = %v", meta)
	}
}

func countedCalls(t *testing.T, dir, kind string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, kind))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(data)
}

func TestInspectionTrustedLocalOpenCodeCallers(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "opencode")
	script := `#!/bin/sh
case "$1 ${2:-}" in
"--version ") printf x >> '` + dir + `/version'; echo "opencode v2.1.2" ;;
"run --help") echo "--standalone --server --continue --model --agent --format --title" ;;
*) printf x >> '` + dir + `/run' ;;
esac
`
	if err := testutil.WriteExecutable(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := &contract.SwarmConfig{Execution: contract.ExecutionConfig{Mode: contract.ExecutionTrustedLocal}, Agents: map[string]contract.AgentConfig{"opencode": {Binary: binary}}}
	lookup := mapLookup(map[string]string{"SWARM_EXECUTION_MODE": contract.ExecutionTrustedLocal})
	command := func(opts RunnerOptions, config *contract.SwarmConfig) {
		t.Helper()
		for _, role := range []string{"planner", "developer"} {
			if _, err := PreflightWithOptions("opencode", config, false, role, opts); err != nil {
				t.Fatal(err)
			}
		}
		runner, err := NewConfiguredRunnerWithOptions("opencode", config, false, opts)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runner.Run(t.Context(), RunRequest{Role: "developer", WorktreePath: t.TempDir(), Mode: "headless"}); err != nil {
			t.Fatal(err)
		}
	}
	session := sandbox.NewInspectionSession()
	command(RunnerOptions{LookupEnv: lookup, Inspections: session}, cfg)
	if got := countedCalls(t, dir, "version"); got != 1 {
		t.Fatalf("shared session inspected %d times, want 1", got)
	}
	if got := countedCalls(t, dir, "run"); got != 1 {
		t.Fatalf("agent ran %d times, want 1", got)
	}
	// Changed approved policy re-inspects in the same session.
	changed := *cfg
	changed.DefaultAgent = "opencode"
	command(RunnerOptions{LookupEnv: lookup, Inspections: session}, &changed)
	if got := countedCalls(t, dir, "version"); got != 2 {
		t.Fatalf("changed policy inspected %d times in total, want 2", got)
	}
	command(RunnerOptions{LookupEnv: lookup}, cfg)
	if got := countedCalls(t, dir, "version"); got != 5 {
		t.Fatalf("nil session inspected %d times in total, want 5", got)
	}
}

func TestInspectionIsolatedCodexPreflightToLaunch(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("native Codex isolation is qualified on Linux")
	}
	// The mandatory native network canary listens on loopback.
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		t.Skipf("host denies loopback sockets for the native network canary: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	listener.Close()
	dir := t.TempDir()
	binary := filepath.Join(dir, "codex")
	script := `#!/bin/sh
for a; do [ "$a" = swarm-canary ] && canary=1; [ "$a" = exec ] && run=1; done
case "$1 ${2:-}" in
"--version ") printf x >> '` + dir + `/version'; echo "codex-cli 0.163.0" ;;
"exec --help") echo "--ignore-user-config --ignore-rules --ephemeral --skip-git-repo-check" ;;
"login status") printf x >> '` + dir + `/auth'; echo "Logged in using an API key" ;;
*) if [ -n "${canary:-}" ]; then
     printf x >> '` + dir + `/canary'
     [ -e '` + dir + `/fail-canary' ] && exit 3
     printf SWARM_NATIVE_BOUNDARY_OK
   elif [ -n "${run:-}" ]; then printf x >> '` + dir + `/run'
   fi ;;
esac
`
	if err := testutil.WriteExecutable(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := &contract.SwarmConfig{Execution: contract.ExecutionConfig{Mode: contract.ExecutionIsolated, Backend: contract.BackendNative}, Agents: map[string]contract.AgentConfig{
		"codex": {Binary: binary, Roles: []string{"planner", "reviewer", "developer"}},
	}}
	lookup := mapLookup(map[string]string{"SWARM_EXECUTION_MODE": contract.ExecutionIsolated})
	opts := RunnerOptions{LookupEnv: lookup, Inspections: sandbox.NewInspectionSession()}
	for _, role := range []string{"planner", "reviewer", "developer"} {
		if _, err := PreflightWithOptions("codex", cfg, false, role, opts); err != nil {
			t.Fatalf("%s: %v", role, err)
		}
	}
	// Role authorization still runs on every preflight.
	if _, err := PreflightWithOptions("codex", cfg, false, "tester", opts); err == nil || !strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("unauthorized role passed preflight: %v", err)
	}
	runner, err := NewConfiguredRunnerWithOptions("codex", cfg, false, opts)
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("task"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(t.Context(), RunRequest{Role: "developer", WorktreePath: work, Mode: "headless"}); err != nil {
		t.Fatal(err)
	}
	for kind, want := range map[string]int{"version": 1, "canary": 4, "auth": 4, "run": 1} {
		if got := countedCalls(t, dir, kind); got != want {
			t.Fatalf("%s ran %d times, want %d", kind, got, want)
		}
	}
	// A failing canary stops preflight even on a cache hit.
	if err := os.WriteFile(filepath.Join(dir, "fail-canary"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := PreflightWithOptions("codex", cfg, false, "developer", opts); err == nil {
		t.Fatal("cached inspection skipped the failing canary")
	}
}

func TestForkReadCredentialNeverAllowedInAgentEnvironment(t *testing.T) {
	for _, name := range []string{"SWARM_GITHUB_FORK_READ_TOKEN", "swarm_github_fork_read_token"} {
		t.Setenv(name, "fork-secret")
		for _, entry := range restrictedEnv([]string{name}, t.TempDir()) {
			if strings.Contains(entry, "fork-secret") {
				t.Fatal("fork credential inherited")
			}
		}
	}
}
