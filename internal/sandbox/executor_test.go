package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/storageuse"
)

func TestDelegatedValidationWithoutCodex(t *testing.T) {
	if os.Getenv("SWARM_TEST_EXECUTOR") != "1" {
		t.Skip("set SWARM_TEST_EXECUTOR=1 for actual delegated validation")
	}
	isolateRuntimeControl(t)
	cfg := &contract.SwarmConfig{DefaultAgent: "worker", Agents: map[string]contract.AgentConfig{"worker": {Harness: "omp", Binary: "not-used-by-validation"}}}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "source"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := RunConfiguredNativeValidation(ctx, dir, cfg, "test \"$(cat source)\" = original; echo changed > source", nil, Dependencies{}, false, &out, &out); err != nil {
		t.Fatal(err, out.String())
	}
	var failed strings.Builder
	if err := RunConfiguredNativeValidation(ctx, dir, cfg, "printf '%s\\n' '--- FAIL: TestNative (0.00s)'; exit 7", nil, Dependencies{}, false, &failed, &failed); err == nil || !strings.Contains(failed.String(), "--- FAIL: TestNative") {
		t.Fatalf("native failed output: %q, %v", failed.String(), err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "source"))
	if err != nil || string(data) != "original" {
		t.Fatal("validation changed coordinator workspace", err, string(data))
	}
	assertRuntimePending(t, 0)
}

func TestMCPToolCancellation(t *testing.T) {
	t.Parallel()
	if os.Getenv("SWARM_TEST_EXECUTOR") != "1" {
		t.Skip("set SWARM_TEST_EXECUTOR=1 for actual tool cancellation")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	work := t.TempDir()
	requests, input := io.Pipe()
	output, responses := io.Pipe()
	defer input.Close()
	defer output.Close()
	done := make(chan error, 1)
	go func() { done <- ServeTools(ctx, ToolExecutor{Work: work}, requests, responses) }()
	encoder := json.NewEncoder(input)
	if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "swarm_shell", "arguments": map[string]string{"command": "echo ready > ready; sleep 2; echo escaped > survived"}}}); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(work, "ready")); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("tool did not start")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": 1}}); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Result ToolResult `json:"result"`
	}
	if err := json.NewDecoder(output).Decode(&response); err != nil || !response.Result.IsError {
		t.Fatal("cancelled tool did not fail", err, response)
	}
	input.Close()
	<-done
	if _, err := os.Stat(filepath.Join(work, "survived")); !os.IsNotExist(err) {
		t.Fatal("cancelled tool continued")
	}
}

func TestToolExecutorBoundary(t *testing.T) {
	if os.Getenv("SWARM_TEST_EXECUTOR") != "1" {
		t.Skip("set SWARM_TEST_EXECUTOR=1 for real Bubblewrap tests")
	}
	isolateRuntimeControl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := CheckToolExecutor(ctx); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	host := t.TempDir()
	secret := filepath.Join(host, "coordinator-token")
	os.WriteFile(secret, []byte("dummy-credential"), 0600)
	os.MkdirAll(filepath.Join(work, ".swarm/active"), 0700)
	os.Symlink(secret, filepath.Join(work, "escape"))
	e := ToolExecutor{Work: work}
	call := func(name string, args map[string]string, fail bool) ToolResult {
		data, _ := json.Marshal(args)
		result := e.Call(ctx, name, data)
		if result.IsError != fail {
			t.Fatalf("%s: %+v", name, result)
		}
		return result
	}
	call("swarm_read", map[string]string{"path": secret}, true)
	call("swarm_read", map[string]string{"path": "escape"}, true)
	// The same /tmp pathname may be created in the private tmpfs, but it must
	// not touch the host file (checked below).
	call("swarm_write", map[string]string{"path": secret, "content": "stolen"}, false)
	call("swarm_write", map[string]string{"path": "hello.txt", "content": "hello"}, false)
	call("swarm_edit", map[string]string{"path": "hello.txt", "old_text": "hello", "new_text": "world"}, false)
	if result := call("swarm_read", map[string]string{"path": "hello.txt"}, false); result.Content[0].Text != "world" {
		t.Fatal(result)
	}
	e.ReadOnly = true
	call("swarm_write", map[string]string{"path": "hello.txt", "content": "changed"}, true)
	call("swarm_write", map[string]string{"path": ".swarm/active/REVIEW.md", "content": "review"}, false)
	data, _ := os.ReadFile(secret)
	if string(data) != "dummy-credential" {
		t.Fatal("host credential changed")
	}
	// Detached process groups must also end when the PID namespace exits.
	e.ReadOnly = false
	short, stop := context.WithTimeout(ctx, 300*time.Millisecond)
	defer stop()
	var output strings.Builder
	err := e.Run(short, `echo started > started; setsid sh -c 'sleep 1; echo survived > survived' & wait`, &output, &output)
	if err == nil || short.Err() == nil {
		t.Fatal("cancellation failed", err)
	}
	if _, err := os.Stat(filepath.Join(work, "started")); err != nil {
		t.Fatal("command never started", err, output.String())
	}
	time.Sleep(time.Second)
	if _, err := os.Stat(filepath.Join(work, "survived")); !os.IsNotExist(err) {
		t.Fatal("detached child survived")
	}
	t.Run("toolchain mounts exclude go/src", func(t *testing.T) {
		bwrap, err := exec.LookPath("bwrap")
		if err != nil {
			t.Skip("bwrap is not installed")
		}
		goBinary, err := exec.LookPath("go")
		if err != nil {
			t.Skip("go is not installed")
		}
		goroot, err := exec.Command(goBinary, "env", "GOROOT").Output()
		if err != nil {
			t.Fatal(err)
		}
		home := resolvedTempDir(t)
		goBin := filepath.Join(home, "go", "bin")
		secret := filepath.Join(home, "go", "src", "private", "secret.txt")
		for _, dir := range []string{goBin, filepath.Dir(secret)} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(secret, []byte("dummy-secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", home)
		t.Setenv("PATH", strings.Join([]string{goBin, filepath.Dir(goBinary), filepath.Dir(bwrap)}, string(os.PathListSeparator)))
		t.Setenv("GOROOT", strings.TrimSpace(string(goroot)))
		var out strings.Builder
		if err := e.Run(ctx, `test -d "$1" && test ! -r "$2" && go version`, &out, &out, goBin, secret); err != nil {
			t.Fatal(err, out.String())
		}
		if !strings.Contains(out.String(), "go version") {
			t.Fatalf("go did not run: %s", out.String())
		}
		if data, _ := os.ReadFile(secret); string(data) != "dummy-secret" {
			t.Fatal("host secret changed")
		}
	})
	assertRuntimePending(t, 0)
}

func TestOMPRealToolRegistration(t *testing.T) {
	t.Parallel()
	binary := os.Getenv("SWARM_TEST_OMP_BINARY")
	if binary == "" {
		t.Skip("set SWARM_TEST_OMP_BINARY for real RPC interface qualification")
	}
	home := t.TempDir()
	control := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	version, err := InspectDelegatedHarness(ctx, "omp", binary)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("OMP", version)
	var output strings.Builder
	env := append(nativeEnv(home, false, Dependencies{}), "OPENAI_API_KEY=synthetic-offline-fixture")
	if err := runOMP(ctx, binary, "openai/gpt-4.1", "", control, env, ToolExecutor{Work: t.TempDir()}, false, nil, &output, &output); err != nil {
		t.Fatal(err, output.String())
	}
}

func TestMCPDoesNotExposeCoordinatorOperations(t *testing.T) {
	t.Parallel()
	input := strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"approve\"}\n")
	var output strings.Builder
	if err := ServeTools(context.Background(), ToolExecutor{}, input, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "swarm_shell") || !strings.Contains(output.String(), "-32601") {
		t.Fatal(output.String())
	}
	result := (ToolExecutor{}).Call(context.Background(), "publish", json.RawMessage(`{}`))
	if !result.IsError {
		t.Fatal("unregistered tool accepted")
	}
}

func TestToolServerFollowsCoordinatorCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	requests, input := io.Pipe()
	defer input.Close()
	done := make(chan error, 1)
	go func() { done <- ServeTools(ctx, ToolExecutor{}, requests, io.Discard) }()
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("tool server ignored coordinator cancellation")
	}
}

func requireExecutor(t *testing.T) {
	t.Helper()
	if os.Getenv("SWARM_TEST_EXECUTOR") != "1" {
		t.Skip("set SWARM_TEST_EXECUTOR=1 for real Bubblewrap tests")
	}
}

func TestBubblewrapAgentBuildCacheReuse(t *testing.T) {
	requireExecutor(t)
	isolateRuntimeControl(t)
	useUserCache(t)
	repo := cacheTestRepo(t)
	prep, complete := fixturePreparation(t, repo)
	caches, err := agentBuildCaches(prep, repo, resolvedTempDir(t), WorkerOptions{Role: "developer"})
	if err != nil {
		t.Fatal(err)
	}
	work := resolvedTempDir(t)
	writeWorkerFixture(t, work, "go.mod", "module example.com/reuse\n\ngo 1.21\n")
	writeWorkerFixture(t, work, "main.go", "package main\n\nfunc main() {}\n")
	e := ToolExecutor{Work: work, BuildCaches: caches, Dependencies: liveExecutorDependencies(t)}
	write := `set -eu; for d in "$GOCACHE" "$CARGO_TARGET_DIR" "$CCACHE_DIR"; do printf run1 > "$d/m"; done`
	read := `set -eu; for d in "$GOCACHE" "$CARGO_TARGET_DIR" "$CCACHE_DIR"; do test "$(cat "$d/m")" = run1; done`
	for _, script := range []string{write, read} {
		var out strings.Builder
		if err := runCoordinatorExecutor(t, e, t.Context(), script, &out, &out); err != nil {
			t.Fatal(err, out.String())
		}
	}
	complete()
	assertRuntimePending(t, 0)
	if _, err := exec.LookPath("go"); err != nil {
		return
	}
	prep, complete = fixturePreparation(t, repo)
	reused := prepareFixtureCaches(t, prep, AgentBuildCache, repo, rootCache(t))
	for i := range caches {
		if reused[i].Dir != caches[i].Dir {
			t.Fatal("reprepared cache changed location")
		}
	}
	var outputs []string
	for range 2 {
		var out strings.Builder
		if err := runCoordinatorExecutor(t, e, t.Context(), "go build -x -o /tmp/reuse . 2>&1", &out, &out); err != nil {
			t.Skip("go build unavailable in the sandbox:", err, out.String())
		}
		outputs = append(outputs, out.String())
	}
	if !strings.Contains(outputs[0], "/compile ") {
		t.Fatalf("first build did not compile, so reuse is unproven:\n%s", outputs[0])
	}
	if strings.Contains(outputs[1], "/compile ") {
		t.Fatalf("second build recompiled despite the agent cache:\n%s", outputs[1])
	}
	complete()
	assertRuntimePending(t, 0)
}

func TestBubblewrapValidationBuildCacheIsolation(t *testing.T) {
	requireExecutor(t)
	isolateRuntimeControl(t)
	root := useUserCache(t)
	repo := cacheTestRepo(t)
	prep, complete := fixturePreparation(t, repo)
	agent := prepareFixtureCaches(t, prep, AgentBuildCache, repo, root)
	validator := prepareFixtureCaches(t, prep, ValidatorBuildCache, repo, root)
	for _, c := range agent {
		if err := os.WriteFile(filepath.Join(c.Dir, "agent-marker"), []byte("agent"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &contract.SwarmConfig{DefaultAgent: "worker", Agents: map[string]contract.AgentConfig{"worker": {Harness: "omp", Binary: "not-used-by-validation"}}}
	for i, c := range agent {
		v := validator[i]
		// Warm-up is the only validation run that writes the persistent cache.
		script := fmt.Sprintf(`set -eu; test ! -e %q; test ! -e %q; test "$%s" = %q; printf warm > "$%s/validator-marker"`,
			filepath.Join(c.Dir, "agent-marker"), c.Dir, v.EnvKey, v.Dir, v.EnvKey)
		var out strings.Builder
		if err := RunConfiguredNativeValidation(t.Context(), resolvedTempDir(t), cfg, script, validator, liveExecutorDependencies(t), true, &out, &out); err != nil {
			t.Fatal(err, out.String())
		}
		if _, err := os.Stat(filepath.Join(v.Dir, "validator-marker")); err != nil {
			t.Fatalf("%s: warm-up did not write the validator cache: %v", v.Ecosystem, err)
		}
		// Agents can neither see nor write the validator cache.
		e := ToolExecutor{Work: resolvedTempDir(t), BuildCaches: agent, Dependencies: liveExecutorDependencies(t)}
		out.Reset()
		if err := runCoordinatorExecutor(t, e, t.Context(), `set -eu; test ! -e "$1"; if (printf x > "$1/agent-wrote") 2>/dev/null; then exit 9; fi`, &out, &out, v.Dir); err != nil {
			t.Fatalf("%s: agent reached the validator cache: %v %s", v.Ecosystem, err, out.String())
		}
		if _, err := os.Stat(filepath.Join(v.Dir, "agent-wrote")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: agent wrote the validator cache: %v", v.Ecosystem, err)
		}
	}
	complete()
	assertRuntimePending(t, 0)
}

func TestBubblewrapCandidateWritesDiscarded(t *testing.T) {
	requireExecutor(t)
	isolateRuntimeControl(t)
	if !bubblewrapOverlay() {
		t.Skip("Bubblewrap overlay mounts are unavailable here")
	}
	root := useUserCache(t)
	repo := cacheTestRepo(t)
	prep, complete := fixturePreparation(t, repo)
	validator := prepareFixtureCaches(t, prep, ValidatorBuildCache, repo, root)
	for _, c := range validator {
		if err := os.WriteFile(filepath.Join(c.Dir, "warm-marker"), []byte("warm"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &contract.SwarmConfig{DefaultAgent: "worker", Agents: map[string]contract.AgentConfig{"worker": {Harness: "omp", Binary: "not-used-by-validation"}}}
	script := `set -eu; for d in "$GOCACHE" "$CARGO_TARGET_DIR" "$CCACHE_DIR"; do test "$(cat "$d/warm-marker")" = warm; printf bad > "$d/candidate-marker"; printf bad > "$d/warm-marker"; done`
	var out strings.Builder
	if err := RunConfiguredNativeValidation(t.Context(), resolvedTempDir(t), cfg, script, validator, liveExecutorDependencies(t), false, &out, &out); err != nil {
		t.Fatal(err, out.String())
	}
	for _, c := range validator {
		if data, err := os.ReadFile(filepath.Join(c.Dir, "warm-marker")); err != nil || string(data) != "warm" {
			t.Fatalf("%s: candidate changed the warm cache: %q, %v", c.Ecosystem, data, err)
		}
		if _, err := os.Stat(filepath.Join(c.Dir, "candidate-marker")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: candidate write persisted: %v", c.Ecosystem, err)
		}
	}
	complete()
	assertRuntimePending(t, 0)
}

func TestGoConcurrencyExecutor(t *testing.T) {
	goConcurrencyHostileEnv(t)
	for _, readOnly := range []bool{false, true} {
		for _, warmup := range []bool{false, true} {
			t.Run(fmt.Sprintf("readonly=%v/warmup=%v", readOnly, warmup), func(t *testing.T) {
				cache := t.TempDir()
				caches := []BuildCache{{Ecosystem: "go", EnvKey: "GOCACHE", Dir: cache, Overlay: !warmup}}
				flags, err := executorFlags(t.TempDir(), readOnly, caches, Dependencies{}, "echo unchanged", []string{"argument"})
				if err != nil {
					t.Fatal(err)
				}
				var env []string
				for i, flag := range flags {
					if flag == "--setenv" {
						env = append(env, flags[i+1]+"="+flags[i+2])
					}
				}
				goConcurrencyAssertEnv(t, env, cache)
				joined := strings.Join(flags, "\n")
				for _, exact := range []string{"--clearenv", "--size\n1073741824\n--tmpfs\n/tmp", "--as=8589934592", "--cpu=300", "--nproc=1024", "--nofile=4096", "--fsize=268435456", "--core=0"} {
					if !strings.Contains("\n"+joined+"\n", "\n"+exact+"\n") {
						t.Fatalf("missing resource/isolation setting %q", exact)
					}
				}
				if !strings.HasSuffix(joined, "/bin/sh\n-c\necho unchanged\nswarm-tool\nargument") {
					t.Fatal("command arguments changed")
				}
				goConcurrencyReplay(t, env)
			})
		}
	}
}

func TestGoConcurrencyBoundaries(t *testing.T) {
	for _, harness := range []string{"omp", "codex"} {
		t.Run(harness, func(t *testing.T) {
			binary := os.Getenv("SWARM_TEST_CODEX_BINARY")
			if harness == "codex" && binary == "" {
				t.Skip("set SWARM_TEST_CODEX_BINARY for real Codex boundary")
			}
			if harness == "omp" && os.Getenv("SWARM_TEST_EXECUTOR") != "1" {
				t.Skip("set SWARM_TEST_EXECUTOR=1 for real Bubblewrap boundary")
			}
			goConcurrencyHostileEnv(t)
			work := goConcurrencyFixture(t)
			cfg := &contract.SwarmConfig{DefaultAgent: "worker", Agents: map[string]contract.AgentConfig{"worker": {Harness: harness, Binary: binary}}}
			cache := t.TempDir()
			caches := []BuildCache{{Ecosystem: "go", EnvKey: "GOCACHE", Dir: cache, Persistent: true}}
			for _, warmup := range []bool{true, false} {
				var out strings.Builder
				err := RunConfiguredNativeValidation(t.Context(), work, cfg, goConcurrencyScript+"\nprintf changed > source\n", caches, Dependencies{}, warmup, &out, &out)
				if err != nil {
					t.Fatal(err, out.String())
				}
				goConcurrencyOutput(t, out.String())
				data, err := os.ReadFile(filepath.Join(work, "source"))
				if err != nil || string(data) != "original" {
					t.Fatal("validation modified candidate", err)
				}
			}
			var out strings.Builder
			if harness == "omp" {
				if err := (ToolExecutor{Work: work, BuildCaches: caches}).Run(t.Context(), goConcurrencyScript, &out, &out); err != nil {
					t.Fatal(err, out.String())
				}
			} else {
				scratch := t.TempDir()
				args := codexPolicy(binary, work, scratch, false, caches, Dependencies{})
				args = append(args, "sandbox", "-C", work, "-P", "swarm", "--", "/bin/sh", "-c", goConcurrencyScript)
				if err := runNativeCommand(t.Context(), binary, scratch, nativeEnv(codexHostHome(scratch), false, Dependencies{}), args, nil, &out, &out); err != nil {
					t.Fatal(err, out.String())
				}
			}
			goConcurrencyOutput(t, out.String())
		})
	}
}

// TestToolExecutorRecoveryBoundary is the maintainer's live teardown check.
// The helper coordinator is killed only after the writer and durable evidence
// are ready. No stand-in launcher supplies the production boundary identity.
func TestToolExecutorRecoveryBoundary(t *testing.T) {
	if os.Getenv("SWARM_TEST_EXECUTOR") != "1" {
		t.Skip("set SWARM_TEST_EXECUTOR=1 for live Bubblewrap recovery")
	}
	if root := os.Getenv("SWARM_TEST_RECOVERY_EXECUTOR_ROOT"); root != "" {
		storageuse.UserCacheDir = func() (string, error) { return root, nil }
		ctx, _, err := storageuse.Ensure(context.Background(), strings.Repeat("a", 32))
		if err != nil {
			t.Fatal(err)
		}
		cache := filepath.Join(root, "cache", "agent-build", strings.Repeat("a", 32), "go")
		scope := storageuse.FromContext(ctx)
		if err := scope.AcquirePrepared(ctx, storageuse.ResourceFor(scope.Repository(), "agent-build", cache)); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(cache, 0700); err != nil {
			t.Fatal(err)
		}
		e := ToolExecutor{Work: filepath.Join(root, "work"), BuildCaches: []BuildCache{{Ecosystem: "go", EnvKey: "GOCACHE", Dir: cache, Persistent: true}}}
		if err := e.Run(ctx, `echo ready > /workspace/ready; while :; do echo running >> "$GOCACHE/writer"; sleep 0.05; done`, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
		return
	}
	root := resolvedTempDir(t)
	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestToolExecutorRecoveryBoundary$", "-test.timeout=30s")
	cmd.Env = append(os.Environ(), "SWARM_TEST_RECOVERY_EXECUTOR_ROOT="+root)
	var diagnostics strings.Builder
	cmd.Stdout = &diagnostics
	cmd.Stderr = &diagnostics
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	control := filepath.Join(root, "swarm-use")
	cache := filepath.Join(root, "cache", "agent-build", strings.Repeat("a", 32), "go")
	resource := storageuse.ResourceFor(strings.Repeat("a", 32), "agent-build", cache)
	var boundary storageuse.ProcessIdentity
	ready := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, readyErr := os.Stat(filepath.Join(work, "ready"))
		records, _ := filepath.Glob(filepath.Join(control, "*.json"))
		sealed := 0
		for _, path := range records {
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var v struct {
				Evidence map[string]struct {
					Consumers map[string]storageuse.ProcessIdentity
				}
			}
			if json.Unmarshal(data, &v) != nil {
				continue
			}
			for _, e := range v.Evidence {
				for _, p := range e.Consumers {
					if p.PID > 0 {
						boundary = p
						sealed++
					}
				}
			}
		}
		if readyErr == nil && sealed == 2 {
			ready = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready || boundary.PID == 0 {
		t.Fatal("no complete durable namespace identity before deadline")
	}
	if _, err := os.Stat(filepath.Join(work, "ready")); err != nil {
		t.Fatal("writer not ready", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	manager, err := storageuse.Open(control, cache)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	deadline = time.Now().Add(10 * time.Second)
	recovered := false
	for time.Now().Before(deadline) {
		changed, err := manager.Recover(context.Background(), resource, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if changed {
			recovered = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !recovered {
		t.Fatal("killed coordinator/boundary did not become recoverable")
	}
	if current, err := storageuse.CaptureProcess(boundary.PID); err == nil && current == boundary {
		t.Fatal("recorded boundary survived recovery")
	}
	first, err := os.ReadFile(filepath.Join(cache, "writer"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	last, err := os.ReadFile(filepath.Join(cache, "writer"))
	if err != nil || string(first) != string(last) {
		t.Fatal("storage writer survived teardown", err)
	}
	observation, err := storageuse.Observe(context.Background(), control, resource, 16384, func(int64) error { return nil })
	if err != nil || observation.Active == nil || *observation.Active {
		t.Fatalf("recovery: %+v %v", observation, err)
	}
}

func TestToolCapabilityPrototype(t *testing.T) {
	for _, driver := range []string{"codex", "claude", "opencode", "omp"} {
		for _, delegated := range []bool{false, true} {
			if !delegated && driver != "codex" {
				continue
			}
			for _, role := range []string{"developer", "planner", "reviewer"} {
				for _, browser := range []bool{false, true} {
					for _, verified := range []bool{false, true} {
						r := ToolCapabilities(driver, role, "approved_task", delegated, true, verified, browser)
						raw, err := json.Marshal(r)
						if err != nil {
							t.Fatal(err)
						}
						if len(r.Entries) != 3 || !strings.Contains(FormatToolCapabilities(r), r.Entries[0].Transport) {
							t.Fatal(string(raw))
						}
						if !verified && r.Entries[0].Usable {
							t.Fatal("unprobed usable")
						}
						if role != "developer" && strings.Contains(strings.Join(r.Entries[0].Operations, ","), "write") {
							t.Fatal("read-only write promise")
						}
						b := InspectCapabilityEntry(r.Entries[1], func() error { return errors.New("TOKEN /home/private-user/secret") })
						raw, _ = json.Marshal(b)
						if strings.Contains(string(raw), "TOKEN") || strings.Contains(string(raw), "/home/") || b.Runtime != "not_probed" || b.Usable {
							t.Fatal(string(raw))
						}
					}
				}
			}
		}
	}
}

func TestToolCapabilityAssemblyPanicAndTimeout(t *testing.T) {
	e := ToolCapabilities("codex", "developer", "approved_task", true, true, true, true).Entries[1]
	e = InspectCapabilityEntry(e, func() error { panic("TOKEN /home/private-user/secret") })
	if e.Runtime != "not_probed" || e.Usable {
		t.Fatal(e)
	}
	release := make(chan struct{})
	defer close(release)
	start := time.Now()
	e = InspectCapabilityEntry(e, func() error { <-release; return nil })
	if e.Runtime != "not_probed" || e.Usable || time.Since(start) > 3*time.Second {
		t.Fatal(e)
	}
}

func TestToolCapabilityPreparationAndLaunch(t *testing.T) {
	deps := Dependencies{Caches: []DependencyCache{{Ecosystem: "go", Dir: "/home/private-user/TOKEN"}, {Ecosystem: "TOKEN", Dir: "/secret"}}}
	r := LaunchToolCapabilities("omp", "developer", true, false, true, deps)
	if !r.Entries[0].ReadOnly || r.Entries[0].Limits.CommandSeconds != toolCommandSeconds {
		t.Fatal(r)
	}
	if strings.Contains(strings.Join(r.Entries[0].Operations, ","), "write") {
		t.Fatal("read-only launch promised writes")
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "TOKEN") || strings.Contains(string(b), "/home/") || r.Entries[2].Runtime != "available" || r.Entries[2].Usable {
		t.Fatal(string(b))
	}
	r.Entries[2] = InspectCapabilityEntry(r.Entries[2], func() error { return errors.New("failure") })
	r = PreparedCapabilities(r, deps)
	if r.Entries[2].Runtime != "not_probed" {
		t.Fatal("preparation overwrote report assembly failure")
	}
}

func TestToolCapabilitySharedFields(t *testing.T) {
	for _, tc := range []struct{ driver, tool, transport string }{
		{"codex", "mcp__swarm.swarm_read", "owned_mcp"},
		{"claude", "mcp__swarm__swarm_read", "owned_mcp"},
		{"opencode", "swarm_swarm_read", "owned_mcp"},
		{"omp", "swarm_read", "host_rpc"},
	} {
		r := ToolCapabilities(tc.driver, "developer", "approved_task", true, true, true, false)
		e := r.Entries[0]
		if e.Tools[0] != tc.tool || e.Transport != tc.transport || e.Limits.ArgumentBytes != toolArgumentBytes || e.Limits.ReadEditBytes != toolReadEditBytes || e.Limits.CommandSeconds != toolCommandSeconds {
			t.Fatal(e)
		}
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		var decoded ToolCapabilityReport
		if err := json.Unmarshal(b, &decoded); err != nil {
			t.Fatal(err)
		}
		if FormatToolCapabilities(decoded) != FormatToolCapabilities(r) {
			t.Fatal("text and JSON disagree")
		}
		if r.Entries[1].Declared || r.Entries[1].Authorized || r.Entries[1].Usable {
			t.Fatal("optional browser enabled")
		}
	}
	r := ToolCapabilities("codex", "developer", "approved_task", false, true, true, false)
	if r.Entries[0].Limits.ArgumentBytes != 0 || r.Entries[0].Limits.CommandSeconds != 0 {
		t.Fatal("executor limits attributed to native tools")
	}
}

func TestToolCapabilityNativeBrowserServerTools(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	policy, err := codexBrowserPolicy([]string{"mcp_servers={}"}, executable, t.TempDir(), ToolExecutor{Work: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, enabled, ok := strings.Cut(policy[0], "enabled_tools=")
	if !ok {
		t.Fatal("missing enabled tools")
	}
	enabled, _, ok = strings.Cut(enabled, "]")
	if !ok {
		t.Fatal("invalid enabled tools")
	}
	var names []string
	if err := json.Unmarshal([]byte(enabled+"]"), &names); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if err := ServeTools(t.Context(), ToolExecutor{}, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n"), &output); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Result struct {
			Tools []ToolDefinition `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(output.String()), &response); err != nil {
		t.Fatal(err)
	}
	var expected []string
	for _, definition := range response.Result.Tools {
		for _, name := range names {
			if definition.Name == name {
				if !strings.Contains(policy[0], name+`={approval_mode="approve"}`) {
					t.Fatalf("tool %s lacks approval", name)
				}
				expected = append(expected, "mcp__swarm."+name)
			}
		}
	}
	if len(expected) == 0 || len(expected) != len(names) || len(expected) != len(response.Result.Tools) {
		t.Fatal("enabled server definitions differ", names, response.Result.Tools)
	}
	for _, role := range []string{"developer", "tester", "planner", "reviewer"} {
		t.Run(role, func(t *testing.T) {
			report := ToolCapabilities("codex", role, "approved_task", false, true, true, true)
			native, mcp := report.Entries[0], report.Entries[1]
			if strings.Join(mcp.Tools, ",") != strings.Join(expected, ",") {
				t.Fatalf("report tools %v; enabled server tools %v", mcp.Tools, expected)
			}
			if native.Limits != (ToolCapabilityLimits{}) || mcp.Limits != (ToolCapabilityLimits{ArgumentBytes: toolArgumentBytes, ReadEditBytes: toolReadEditBytes, CommandSeconds: toolCommandSeconds}) {
				t.Fatal("native and MCP limits", native.Limits, mcp.Limits)
			}
			ro := role == "planner" || role == "reviewer"
			for _, operation := range []string{"write", "edit"} {
				has := false
				for _, actual := range mcp.Operations {
					has = has || actual == operation
				}
				if has == ro {
					t.Fatal("incorrect writable operation", mcp)
				}
			}
			if ro && (!mcp.ReadOnly || mcp.Authorized || mcp.Usable || mcp.Reason != "role_denied" || !strings.Contains(strings.Join(mcp.Restrictions, ","), "write/edit denied")) {
				t.Fatal("read-only MCP permission", mcp)
			}
		})
	}
}
