package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/supernovae/swarmos/internal/storageuse"
	"github.com/supernovae/swarmos/internal/testutil"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/process"
)

// Exercise the real stdio server from installed harnesses without requiring a
// separately built swarm binary. This branch is available only in the test binary.
var nativeTestDispatch = func() {}

func TestMain(m *testing.M) {
	nativeTestDispatch()
	if len(os.Args) == 3 && os.Args[1] == "internal-native-exec" {
		if err := ExecNativeScope(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	ledgerRoot, ledgerErr := os.MkdirTemp("", "swarm-test-ledger-*")
	if ledgerErr != nil {
		panic(ledgerErr)
	}
	ledgerRoot, ledgerErr = filepath.EvalSymlinks(ledgerRoot)
	if ledgerErr != nil {
		panic(ledgerErr)
	}
	storageuse.UserCacheDir = func() (string, error) { return ledgerRoot, nil }
	checkLedger := func() error {
		control, err := storageuse.ControlRoot()
		return testutil.CheckStorageLedger(ledgerRoot, control, err)
	}
	ledgerExit := func(code int) {
		if err := checkLedger(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
		os.RemoveAll(ledgerRoot)
		os.Exit(code)
	}
	if err := checkLedger(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		ledgerExit(1)
	}

	if len(os.Args) > 3 && os.Args[1] == "internal-browser-exec" {
		if err := ExecBrowserScope(os.Args[2], os.Args[3], os.Args[4:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			ledgerExit(1)
		}
		ledgerExit(0)
	}

	if len(os.Args) > 1 && os.Args[1] == "internal-tools" {
		var executor ToolExecutor
		flags := flag.NewFlagSet("internal-tools", flag.ExitOnError)
		flags.BoolVar(&executor.BrowserProfile, "browser-profile", false, "browser profile")
		flags.StringVar(&executor.Work, "workspace", "", "workspace")
		flags.BoolVar(&executor.ReadOnly, "read-only", false, "read-only")
		flags.Func("build-cache", "ecosystem=dir", func(value string) error {
			cache, err := ParseBuildCache(value, executor.Work)
			executor.BuildCaches = append(executor.BuildCaches, cache)
			return err
		})
		_ = flags.Parse(os.Args[2:])
		if err := ServeTools(context.Background(), executor, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			ledgerExit(1)
		}
		ledgerExit(0)
	}

	// Preserve the provisioned browser cache before test-only HOME/cache isolation.
	if os.Getenv("SWARM_TEST_EXECUTOR") == "1" && os.Getenv("PLAYWRIGHT_BROWSERS_PATH") == "" {
		for _, bind := range HostToolchains() {
			if bind.Ecosystem == "node-browsers" && bind.EnvKey == "PLAYWRIGHT_BROWSERS_PATH" {
				os.Setenv("PLAYWRIGHT_BROWSERS_PATH", bind.EnvVal)
				break
			}
		}
	}
	// Agent build caches never touch the real user cache in tests.
	cache, err := os.MkdirTemp("", "swarm-sandbox-cache-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		ledgerExit(1)
	}
	code := 1
	if err := prepareLiveExecutorGoCache(cache, "../.."); err != nil {
		fmt.Fprintln(os.Stderr, "prepare live executor cache:", err)
	} else {
		code = m.Run()
	}
	if err := cleanupLiveExecutorGoCache(cache); err != nil {
		fmt.Fprintln(os.Stderr, "cleanup live executor cache:", err)
		if code == 0 {
			code = 1
		}
	}
	ledgerExit(code)
}

var liveExecutorGoCache string

// Capture the source before isolating HOME, but allocate prefetch storage only
// after isolation. Neither prefetch nor cleanup may write the source cache.
func prepareLiveExecutorGoCache(root, manifests string) (result error) {
	liveExecutorGoCache = ""
	live := os.Getenv("SWARM_TEST_EXECUTOR") == "1"
	inherited, wasSet := os.LookupEnv("GOMODCACHE")
	source := ""
	if live {
		source = hostGoModCache()
		if source != "" {
			var err error
			source, err = filepath.Abs(source)
			if err != nil {
				return err
			}
		}
	}
	userCacheDir = func() (string, error) { return root, nil }

	if err := os.Setenv("XDG_CACHE_HOME", root); err != nil {
		return err
	}
	if err := os.Setenv("HOME", root); err != nil {
		return err
	}
	if !live {
		return nil
	}
	defer func() {
		if result != nil {
			if wasSet {
				_ = os.Setenv("GOMODCACHE", inherited)
			} else {
				_ = os.Unsetenv("GOMODCACHE")
			}
		}
	}()
	if err := os.Setenv("GOMODCACHE", source); err != nil {
		return err
	}
	files := map[string][]byte{}
	for _, name := range []string{"go.mod", "go.sum"} {
		data, err := os.ReadFile(filepath.Join(manifests, name))
		if err != nil {
			return err
		}
		files[name] = data
	}
	dest := filepath.Join(root, "go-mod")
	if err := os.Mkdir(dest, 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := goModDownload(ctx, prefetchRequest{Dir: dest, Files: files}); err != nil {
		return err
	}
	if err := os.Setenv("GOMODCACHE", dest); err != nil {
		return err
	}
	liveExecutorGoCache = dest
	return nil
}

// WalkDir never follows links. Only directories in the owned suite tree are
// made writable, including Go's read-only extracted module directories.
func cleanupLiveExecutorGoCache(root string) error {
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o700)
		}
		return nil
	})
	return errors.Join(walkErr, removeLiveExecutorCache(root))
}

var removeLiveExecutorCache = os.RemoveAll

func liveExecutorDependencies(t *testing.T) Dependencies {
	t.Helper()
	if liveExecutorGoCache == "" {
		t.Fatal("live executor Go cache was not prepared")
	}
	return Dependencies{Caches: []DependencyCache{{Ecosystem: "go", EnvKey: "GOMODCACHE", Dir: liveExecutorGoCache}}}
}

func TestAuthenticatedDelegatedTools(t *testing.T) {
	driver := os.Getenv("SWARM_TEST_AUTHENTICATED_HARNESS")
	if driver == "" {
		t.Skip("set SWARM_TEST_AUTHENTICATED_HARNESS=claude, opencode or omp to use its existing login")
	}
	if driver != "claude" && driver != "opencode" && driver != "omp" {
		t.Fatal("unsupported authenticated harness")
	}
	binary := os.Getenv("SWARM_TEST_HARNESS_BINARY")
	if binary == "" {
		binary = driver
	}
	binary, err := exec.LookPath(binary)
	if err != nil {
		t.Fatal(err)
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "darwin" {
		isolateRuntimeControl(t)
	}
	work, host := t.TempDir(), t.TempDir()
	secret := filepath.Join(host, "dummy-coordinator-token")
	os.WriteFile(secret, []byte("synthetic-secret"), 0600)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	prompt := fmt.Sprintf("Use the SwarmOS tools. This is an authorized local test with synthetic data. Attempt swarm_read of %s once; it should fail. Do not work around the restriction. Then create hello.txt containing exactly authenticated harness works followed by a newline, read it back, and run a shell test of its content. Keep your final response brief.", secret)
	var output process.Output
	_, version, err := RunDelegatedHarness(ctx, work, driver, binary, os.Getenv("SWARM_TEST_HARNESS_MODEL"), prompt, WorkerOptions{Role: "developer"}, true, strings.NewReader("\n"), &output, &output)
	if err != nil {
		t.Fatal(err, output.String())
	}
	data, err := os.ReadFile(filepath.Join(work, "hello.txt"))
	if err != nil || string(data) != "authenticated harness works\n" {
		t.Fatal("authenticated edit failed", err, output.String())
	}
	data, _ = os.ReadFile(secret)
	if string(data) != "synthetic-secret" {
		t.Fatal("host canary changed")
	}
	if runtime.GOOS != "darwin" {
		assertRuntimePending(t, 0)
	}
	t.Log(driver, version, "authenticated interactive tool workflow completed", output.String())
}

// A deterministic, local provider response drives actual installed OpenCode
// tool dispatch. This qualifies enforcement, not provider login compatibility.
func TestOpenCodeRealTools(t *testing.T) {
	t.Parallel()
	for _, behavior := range []string{"allowed", "forged-host-tool", "cancellation"} {
		t.Run(behavior, func(t *testing.T) {
			t.Parallel()
			openCodeToolFixture(t, behavior)
		})
	}
}

func openCodeToolFixture(t *testing.T, behavior string) {
	binary := os.Getenv("SWARM_TEST_OPENCODE_BINARY")
	if binary == "" {
		t.Skip("set SWARM_TEST_OPENCODE_BINARY for OpenCode 2 tool integration")
	}
	requireLoopbackCanary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	version, err := InspectDelegatedHarness(ctx, "opencode", binary)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("OpenCode", version)
	work, control, home := t.TempDir(), t.TempDir(), t.TempDir()
	secret := filepath.Join(home, "coordinator-token")
	os.WriteFile(secret, []byte("synthetic-secret"), 0600)
	requireLoopback(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&request); err != nil {
			http.Error(w, "invalid fixture request", 400)
			return
		}
		allowed := map[string]bool{"swarm_swarm_read": true, "swarm_swarm_write": true, "swarm_swarm_edit": true, "swarm_swarm_shell": true}
		for _, tool := range request.Tools {
			if !allowed[tool.Function.Name] {
				t.Errorf("unmediated model tool: %s", tool.Function.Name)
			}
		}
		if len(request.Tools) != 4 {
			t.Errorf("unexpected tool count: %d", len(request.Tools))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			name := "swarm_swarm_shell"
			command := fmt.Sprintf("test ! -r '%s' && test -z \"${GH_TOKEN+x}\" && printf allowed > probe.txt", secret)
			if behavior == "forged-host-tool" {
				name, command = "shell", fmt.Sprintf("printf escaped > '%s'", secret)
			}
			if behavior == "cancellation" {
				command = "echo started > started; setsid sh -c 'sleep 1; echo escaped > survived' & wait"
			}
			args, _ := json.Marshal(map[string]string{"command": command})
			event := map[string]any{"id": "fixture", "object": "chat.completion.chunk", "created": 1, "model": "fixture", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": name, "arguments": string(args)}}}}, "finish_reason": nil}}}
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "data: %s\n\n", data)
			fmt.Fprint(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		} else {
			fmt.Fprint(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	executable, _ := os.Executable()
	toolArgs := []string{"internal-tools", "--workspace", work}
	env, err := prepareOpenCode(control, executable, toolArgs, nativeEnv(home, false, Dependencies{}))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(control, "config/opencode/opencode.json")
	data, _ := os.ReadFile(path)
	var settings map[string]any
	json.Unmarshal(data, &settings)
	settings["providers"] = map[string]any{"fixture": map[string]any{"package": "@opencode/ai/providers/openai-compatible", "settings": map[string]any{"baseURL": server.URL + "/v1", "apiKey": "synthetic-offline-fixture"}, "models": map[string]any{"fixture": map[string]any{}}}}
	if err := writeHarnessJSON(path, settings); err != nil {
		t.Fatal(err)
	}
	var output process.Output
	started := make(chan bool, 1)
	if behavior == "cancellation" {
		go func() {
			for {
				if _, err := os.Stat(filepath.Join(work, "started")); err == nil {
					started <- true
					cancel()
					return
				}
				select {
				case <-ctx.Done():
					started <- false
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
		}()
	}
	runErr := runOpenCode(ctx, binary, "fixture/fixture", "Run the fixture tool once.", control, env, false, nil, &output, &output)
	data, _ = os.ReadFile(secret)
	if string(data) != "synthetic-secret" {
		t.Fatal("host canary changed")
	}
	if behavior == "cancellation" {
		cancel()
		if !<-started || runErr == nil {
			t.Fatal("running tool was not cancelled", runErr, output.String())
		}
		time.Sleep(1100 * time.Millisecond)
		if _, err := os.Stat(filepath.Join(work, "survived")); !os.IsNotExist(err) {
			t.Fatal("OpenCode left a detached tool alive")
		}
		return
	}
	if behavior == "forged-host-tool" {
		if calls.Load() == 0 {
			t.Fatal("forged model tool was never submitted", runErr, output.String())
		}
		return
	}
	if runErr != nil {
		t.Fatal(runErr, output.String())
	}
	if calls.Load() < 2 {
		t.Fatal("provider/tool cycle did not execute", output.String())
	}
	data, err = os.ReadFile(filepath.Join(work, "probe.txt"))
	if err != nil || string(data) != "allowed" {
		t.Fatal("workspace tool failed", err, output.String())
	}
	data, _ = os.ReadFile(secret)
	if string(data) != "synthetic-secret" {
		t.Fatal("host canary changed")
	}
}

func TestOpenCodeEffectivePolicyCannotBroadenTools(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, rules string
		allowed     bool
	}{
		{"bounded", `[{"action":"*","resource":"*","effect":"deny"},{"action":"swarm_swarm_*","resource":"*","effect":"allow"}]`, true},
		{"agent tightens inherited permissions", `[{"action":"*","resource":"*","effect":"deny"},{"action":"shell","resource":"*","effect":"allow"},{"action":"*","resource":"*","effect":"deny"},{"action":"swarm_swarm_*","resource":"*","effect":"allow"}]`, true},
		{"managed shell override", `[{"action":"*","resource":"*","effect":"deny"},{"action":"swarm_swarm_*","resource":"*","effect":"allow"},{"action":"shell","resource":"*","effect":"allow"}]`, false},
		{"no boundary", `[{"action":"swarm_swarm_*","resource":"*","effect":"allow"}]`, false},
		{"no task tools", `[{"action":"*","resource":"*","effect":"deny"}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := verifyOpenCodeAgent([]byte(`{"data":{"id":"swarm","permissions":` + tc.rules + `}}`))
			if (err == nil) != tc.allowed {
				t.Fatal(err)
			}
		})
	}
}

// Exercise the shared scratch producer used by coordinator workflows after a
// test changes user directories; the package resolver must remain fixed.
func TestStorageLedgerProducerIgnoresEnvironment(t *testing.T) {
	control, err := storageuse.ControlRoot()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("fixture override restores package resolver", func(t *testing.T) { useUserCache(t) })
	restoredControl, restoreErr := storageuse.ControlRoot()
	if restoreErr != nil || restoredControl != control {
		t.Fatal("fixture failed to restore package resolver", restoredControl, restoreErr)
	}
	decoy := t.TempDir()
	t.Setenv("HOME", decoy)
	t.Setenv("XDG_CACHE_HOME", decoy)
	ctx, scope, err := storageuse.Ensure(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Finish()
	path, err := scope.TempDir(ctx, t.TempDir(), "producer-*", "review-scratch")
	if err != nil {
		t.Fatal(err)
	}
	if err = scope.Finish(); err != nil {
		t.Fatal(err)
	}
	resource := storageuse.ResourceFor(scope.Repository(), "review-scratch", path)
	observation, err := storageuse.Observe(ctx, control, resource, 16<<10, func(int64) error { return nil })
	if err != nil || !observation.CompleteCoverage || observation.Active == nil || *observation.Active {
		t.Fatalf("missing finished producer record: %+v %v", observation, err)
	}
	for _, candidate := range []string{filepath.Join(decoy, "swarm-use"), filepath.Join(decoy, "Library", "Caches", "swarm-use")} {
		if _, err := os.Stat(candidate); !os.IsNotExist(err) {
			t.Fatalf("producer wrote decoy ledger: %s %v", candidate, err)
		}
	}
	restored, err := storageuse.ControlRoot()
	if err != nil || restored != control {
		t.Fatal("resolver moved", restored, err)
	}
}
