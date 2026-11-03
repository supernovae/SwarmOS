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
	"runtime"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/process"
	"github.com/supernovae/swarmos/internal/storageuse"
)

func isolateRuntimeControl(t *testing.T) {
	t.Helper()
	root := resolvedTempDir(t)
	previous := storageuse.UserCacheDir
	storageuse.UserCacheDir = func() (string, error) { return root, nil }
	t.Cleanup(func() { storageuse.UserCacheDir = previous })
	t.Setenv("XDG_CACHE_HOME", root)
	if runtime.GOOS == "darwin" {
		t.Setenv("HOME", resolvedTempDir(t))
	}
}

func assertRuntimePending(t *testing.T, want int) {
	t.Helper()
	root, err := storageuse.ControlRoot()
	if err != nil {
		t.Fatal(err)
	}
	records, err := filepath.Glob(filepath.Join(root, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 {
		t.Fatal("no durable registration")
	}
	pending := 0
	var details strings.Builder
	for _, path := range records {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var r pendingFixtureRecord
		if err = json.Unmarshal(data, &r); err != nil {
			t.Fatal(err)
		}
		n, detail := pendingFixtureDetails(r)
		pending += n
		details.WriteString(detail)
	}
	if pending != want {
		t.Fatalf("pending records=%d, want %d (incomplete use tokens)\n%s", pending, want, details.String())
	}
}

func TestNativeWorkspaceDurableScopeThroughImport(t *testing.T) {
	isolateRuntimeControl(t)
	source := t.TempDir()
	var held storageuse.Resource
	err := withNativeWorkspace(context.Background(), source, WorkerOptions{}, func(ctx context.Context, work, scratch string) error {
		scope := storageuse.FromContext(ctx)
		held = storageuse.ResourceFor(scope.Repository(), "native-scratch", filepath.Dir(scratch))
		root, _ := storageuse.ControlRoot()
		manager, err := storageuse.Open(root)
		if err != nil {
			return err
		}
		defer manager.Close()
		if guard, err := manager.AcquireMaintenance(ctx, held); !errors.Is(err, storageuse.ErrUnsafe) {
			if guard != nil {
				guard.Close()
			}
			t.Fatalf("maintenance during reader: %v", err)
		}
		assertRuntimePending(t, 1)
		return os.WriteFile(filepath.Join(work, "result"), []byte("imported"), 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(source, "result"))
	if err != nil || string(data) != "imported" {
		t.Fatalf("import %q %v", data, err)
	}
	assertRuntimePending(t, 0)
	root, _ := storageuse.ControlRoot()
	manager, err := storageuse.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	guard, err := manager.AcquireMaintenance(context.Background(), held)
	if err != nil {
		t.Fatal(err)
	}
	guard.Close()
}

func TestNativeCommandVerifiedFailureCompletesScope(t *testing.T) {
	for _, kind := range []string{"success", "nonzero", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			isolateRuntimeControl(t)
			ctx, scope, err := storageuse.Ensure(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			dir, err := scope.TempDir(ctx, "", "swarm-native-test-*", "native-scratch")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			script := "exit 0"
			if kind == "nonzero" {
				script = "exit 7"
			}
			if kind == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			err = runNativeCommand(ctx, "/bin/sh", dir, []string{"PATH=/usr/bin:/bin"}, []string{"-c", script}, nil, io.Discard, io.Discard)
			if (kind == "success") != (err == nil) {
				t.Fatalf("command outcome %v", err)
			}
			if err := scope.Finish(); err != nil {
				t.Fatal(err)
			}
			assertRuntimePending(t, 0)
		})
	}
}

func TestNativeWorkspaceUncertainCompletionRetainsScratch(t *testing.T) {
	for _, panics := range []bool{false, true} {
		t.Run(map[bool]string{false: "unverified-stop", true: "panic"}[panics], func(t *testing.T) {
			isolateRuntimeControl(t)
			var root string
			func() {
				defer func() {
					if p := recover(); p != nil && !panics {
						t.Fatalf("unexpected panic %v", p)
					}
				}()
				err := withNativeWorkspace(context.Background(), t.TempDir(), WorkerOptions{}, func(ctx context.Context, work, scratch string) error {
					root = filepath.Dir(scratch)
					if panics {
						panic("interrupted coordinator")
					}
					storageuse.FromContext(ctx).Retain()
					return errors.New("unverified stop")
				})
				if err == nil {
					t.Fatal("missing failure")
				}
			}()
			defer os.RemoveAll(root)
			assertRuntimePending(t, 1)
			if _, err := os.Stat(root); err != nil {
				t.Fatalf("lost recovery scratch: %v", err)
			}
		})
	}
}

func TestHarnessInspectionOrdinaryCompletionHasNoPendingUses(t *testing.T) {
	for _, tc := range []struct{ driver, version, help string }{
		{"codex", "codex-cli 0.166.0", "--ignore-user-config --ignore-rules --skip-git-repo-check"},
		{"claude", "2.1.311 (Claude Code)", "--restricted --strict-mcp-config --tools --settings --permission-mode"},
		{"omp", "omp/18.12.6", "--mode --no-tools --no-extensions --no-rules --config"},
		{"opencode", "opencode v2.1.2", "--standalone --server --continue --model --agent --format --title"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			isolateRuntimeControl(t)
			binary := filepath.Join(t.TempDir(), "harness")
			script := "#!/bin/sh\ncase \"$1\" in\n--version) printf '%s\\n' '" + tc.version + "';;\n*) printf '%s\\n' '" + tc.help + "';;\nesac\n"
			if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			if tc.driver == "codex" {
				if _, err := inspectNativeCodexInterface(context.Background(), binary); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, _, err := inspectDelegatedHarness(context.Background(), tc.driver, binary, nil); err != nil {
					t.Fatal(err)
				}
			}
			assertRuntimePending(t, 0)
		})
	}
}

func TestNativeWorkspaceRejectsControlStorageInsideWorkspace(t *testing.T) {
	source := t.TempDir()
	previous := storageuse.UserCacheDir
	storageuse.UserCacheDir = func() (string, error) { return filepath.Join(source, "cache"), nil }
	t.Cleanup(func() { storageuse.UserCacheDir = previous })
	t.Setenv("XDG_CACHE_HOME", filepath.Join(source, "cache"))
	if runtime.GOOS == "darwin" {
		t.Setenv("HOME", source)
	}
	ran := false
	err := withNativeWorkspace(context.Background(), source, WorkerOptions{}, func(context.Context, string, string) error { ran = true; return nil })
	if err == nil || ran {
		t.Fatalf("overlapping control storage admitted: ran=%v err=%v", ran, err)
	}
	root, _ := storageuse.ControlRoot()
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("control storage created inside workspace: %v", err)
	}
}

// cancelNativeCommandWriter cancels only after the command has started and
// produced output, so storage registration cannot short-circuit the stop test.
type cancelNativeCommandWriter struct {
	cancel context.CancelFunc
}

func (w cancelNativeCommandWriter) Write(p []byte) (int, error) {
	w.cancel()
	return len(p), nil
}

func TestNativeStopFailureRetainsUse(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "command-success", true: "cancellation"}[canceled], func(t *testing.T) {
			isolateRuntimeControl(t)
			ctx, scope, err := storageuse.Ensure(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			dir, err := scope.TempDir(ctx, "", "swarm-native-stop-test-*", "native-scratch")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			var stdout io.Writer = io.Discard
			if canceled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				stdout = cancelNativeCommandWriter{cancel: cancel}
			}
			failure := errors.New("injected stop verification failure")
			err = runNativeCommandWithStop(ctx, "/bin/sh", dir, []string{"PATH=/usr/bin:/bin"}, []string{"-c", "printf ready"}, nil, stdout, io.Discard, func(*exec.Cmd) error { return failure })
			if canceled && !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatalf("command did not trigger cancellation: %v", ctx.Err())
			}
			if !errors.Is(err, failure) {
				t.Fatalf("stop failure hidden: %v", err)
			}
			if err := scope.Finish(); err != nil {
				t.Fatal(err)
			}
			assertRuntimePending(t, 1)
		})
	}
}

func TestDelegatedProtocolOrdinaryCompletionHasNoPendingUses(t *testing.T) {
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, driver := range []string{"omp", "opencode"} {
		t.Run(driver, func(t *testing.T) {
			isolateRuntimeControl(t)
			script := "#!/bin/sh\nexport GORACE=\"${GORACE:+$GORACE }atexit_sleep_ms=0\"\nexec '" + strings.ReplaceAll(testBinary, "'", "'\"'\"'") + "' -test.run=TestGuidanceProtocolHelper -- --swarm-guidance-fixture " + driver + " \"$@\"\n"
			binary := guidanceFixture(t, script)
			err := withNativeWorkspace(t.Context(), t.TempDir(), WorkerOptions{}, func(ctx context.Context, work, scratch string) error {
				control := filepath.Join(filepath.Dir(scratch), "control")
				if err := os.Mkdir(control, 0700); err != nil {
					return err
				}
				env := nativeEnv(control, false, Dependencies{})
				if driver == "omp" {
					return runOMP(ctx, binary, "", "initial task", control, env, ToolExecutor{Work: work}, true, strings.NewReader("\n"), io.Discard, io.Discard)
				}
				return runOpenCode(ctx, binary, "", "initial task", control, env, true, strings.NewReader("\n"), io.Discard, io.Discard)
			})
			if err != nil {
				t.Fatal(err)
			}
			assertRuntimePending(t, 0)
		})
	}
}

func runCoordinatorExecutor(t *testing.T, executor ToolExecutor, ctx context.Context, script string, stdout, stderr io.Writer, args ...string) (resultErr error) {
	t.Helper()
	ctx, owner, err := storageuse.Ensure(ctx, executor.Work)
	if err != nil {
		return err
	}
	if owner != nil {
		defer owner.Complete(&resultErr)
	}
	return executor.Run(ctx, script, stdout, stderr, args...)
}

func TestToolExecutorWorkerCreatesNoCoordinatorControl(t *testing.T) {
	isolateRuntimeControl(t)
	root, err := storageuse.ControlRoot()
	if err != nil {
		t.Fatal(err)
	}
	workerCache := filepath.Join(t.TempDir(), "swarm", "agent-build", strings.Repeat("a", 32), "go")
	command := exec.Command(os.Args[0], "-test.run=^TestToolExecutorWorkerNoControlHelper$", "--", workerCache)
	command.Env = append(os.Environ(), "SWARM_EXECUTOR_NO_CONTROL_HELPER=1")
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("worker helper: %v %s", err, out)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("worker created coordinator control root: %v", err)
	}
}

func TestToolExecutorWorkerNoControlHelper(t *testing.T) {
	if os.Getenv("SWARM_EXECUTOR_NO_CONTROL_HELPER") != "1" {
		return
	}
	cache := os.Args[len(os.Args)-1]
	executor := ToolExecutor{Work: filepath.Dir(cache), BuildCaches: []BuildCache{{Ecosystem: "go", EnvKey: "GOCACHE", Dir: cache, Persistent: true}}}
	// The worker has no coordinator scope. Admission must remain owned by the
	// outer run, even when runtime setup fails before a Bubblewrap command starts.
	_ = executor.Run(context.Background(), "exit 0", io.Discard, io.Discard)
}

func TestBoundaryEvidenceOutsideConsumerIsPermanent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux evidence")
	}
	isolateRuntimeControl(t)
	ctx, owner, err := storageuse.Ensure(context.Background(), strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Finish()
	scope := storageuse.FromContext(ctx)
	path := filepath.Join(resolvedTempDir(t), "agent-build", strings.Repeat("a", 32), "go")
	resource := storageuse.ResourceFor(scope.Repository(), "agent-build", path)
	if err := scope.AcquirePrepared(ctx, resource); err != nil {
		t.Fatal(err)
	}
	if err := runLegacyNativeCommand(ctx, "/bin/sh", t.TempDir(), []string{"PATH=/usr/bin:/bin"}, []string{"-c", "exit 0"}, nil, io.Discard, io.Discard, process.StopVerified); err != nil {
		t.Fatal(err)
	}
	// A later contained admission must inherit the earlier unclassified contamination.
	if err := scope.AcquirePrepared(ctx, resource); err != nil {
		t.Fatal(err)
	}
	reservation, err := scope.ReserveBoundary(ctx, []string{path})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := storageuse.CaptureProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.Seal(ctx, identity); err != nil {
		t.Fatal(err)
	}
	root, _ := storageuse.ControlRoot()
	records, _ := filepath.Glob(filepath.Join(root, "*.json"))
	if len(records) != 1 {
		t.Fatal(records)
	}
	data, err := os.ReadFile(records[0])
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Evidence map[string]struct{ Outside bool }
	}
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Evidence) != 2 {
		t.Fatalf("missing evidence: %s", data)
	}
	for _, e := range v.Evidence {
		if !e.Outside {
			t.Fatal("later boundary erased native consumer")
		}
	}
	scope.Retain()
}

func TestBoundaryEvidenceRejectsLauncherPID(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux proc evidence")
	}
	if _, err := executorBoundaryIdentity(os.Getpid(), os.Getppid()); err == nil {
		t.Fatal("host process accepted as namespace init")
	}
}

// Preparation owners must never be passed into runtime calls.
func fixturePreparation(t *testing.T, repo string) (context.Context, func()) {
	t.Helper()
	ctx, owner, err := storageuse.Ensure(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	complete := func() {
		var err error
		owner.Complete(&err)
		if err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(complete)
	return ctx, complete
}
func rootCache(t *testing.T) string {
	t.Helper()
	root, err := userCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	return root
}
func prepareFixtureCaches(t *testing.T, ctx context.Context, owner BuildCacheOwner, repo, root string) []BuildCache {
	t.Helper()
	caches, err := PrepareBuildCaches(ctx, owner, repo, root, resolvedTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(caches) != len(cachedEcosystems()) {
		t.Fatalf("caches = %+v, want one per cached ecosystem", caches)
	}
	return caches
}

type pendingFixtureRecord struct {
	Resource         storageuse.Resource
	Uses             map[string]bool
	CompleteCoverage bool
	Evidence         json.RawMessage
}

func pendingFixtureDetails(r pendingFixtureRecord) (int, string) {
	n := 0
	for _, done := range r.Uses {
		if !done {
			n++
		}
	}
	if n == 0 {
		return 0, ""
	}
	return n, fmt.Sprintf("class=%s location=%s total=%d incomplete=%d coverage=%v evidence=%s\n", r.Resource.Class, r.Resource.Location, len(r.Uses), n, r.CompleteCoverage, r.Evidence)
}
func TestRuntimePendingDiagnostics(t *testing.T) {
	n, detail := pendingFixtureDetails(pendingFixtureRecord{Resource: storageuse.Resource{Class: "agent-build", Location: "/cache/go"}, Uses: map[string]bool{"a": false, "b": false, "c": true}, Evidence: json.RawMessage(`{"boundary":"closed"}`)})
	if n != 2 {
		t.Fatal(n)
	}
	for _, want := range []string{"class=agent-build", "location=/cache/go", "total=3", "incomplete=2", "boundary"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("missing %q: %s", want, detail)
		}
	}
}
func TestLiveFixtureStorageScopeOwnership(t *testing.T) {
	for _, sets := range []int{1, 2} {
		t.Run(fmt.Sprint(sets), func(t *testing.T) {
			root := useUserCache(t)
			repo := cacheTestRepo(t)
			ctx, complete := fixturePreparation(t, repo)
			for i := 0; i < sets; i++ {
				owner := AgentBuildCache
				if i == 1 {
					owner = ValidatorBuildCache
				}
				caches := prepareFixtureCaches(t, ctx, owner, repo, root)
				for _, c := range caches {
					if !strings.HasPrefix(c.Dir, root) {
						t.Fatal(c.Dir)
					}
				}
			}
			want := 3
			if sets == 2 {
				want = 7
			}
			assertRuntimePending(t, want)
			control, err := storageuse.ControlRoot()
			if err != nil {
				t.Fatal(err)
			}
			paths, err := filepath.Glob(filepath.Join(control, "*.json"))
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range paths {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var r pendingFixtureRecord
				if err = json.Unmarshal(data, &r); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(r.Resource.Class, "build") || !strings.HasPrefix(r.Resource.Location, root) {
					t.Fatalf("unexpected preparation resource: %+v", r.Resource)
				}
			}
			complete()
			assertRuntimePending(t, 0)
		})
	}
}

func TestConfiguredValidationStorageOwnership(t *testing.T) {
	requireExecutor(t)
	root := useUserCache(t)
	repo := cacheTestRepo(t)
	prep, complete := fixturePreparation(t, repo)
	caches := prepareFixtureCaches(t, prep, ValidatorBuildCache, repo, root)
	cfg := &contract.SwarmConfig{DefaultAgent: "worker", Agents: map[string]contract.AgentConfig{"worker": {Harness: "omp", Binary: "not-used-by-validation"}}}
	for _, warmup := range []bool{true, false} {
		if !warmup && !bubblewrapOverlay() {
			t.Skip("Bubblewrap overlay mounts unavailable")
		}
		var out strings.Builder
		if err := RunConfiguredNativeValidation(t.Context(), resolvedTempDir(t), cfg, "true", caches, liveExecutorDependencies(t), warmup, &out, &out); err != nil {
			t.Fatal(err, out.String())
		}
		// Preparation stays pending; runtime owners must have completed independently.
		assertRuntimePending(t, 4)
	}
	complete()
	assertRuntimePending(t, 0)
}
