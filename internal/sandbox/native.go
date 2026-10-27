package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/claimpause"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/process"
	"github.com/supernovae/swarmos/internal/safefs"
)

func qualifiedNativeCodexVersion(output string) (string, error) {
	return harnessVersion(output, "codex-cli ", "", [3]int{0, 163, 0})
}

// NativeValidationBinary uses an approved Codex entry; validation needs only its
// sandbox command, not model authentication or a separate provider identity.
func NativeValidationBinary(cfg *contract.SwarmConfig) (string, error) {
	name := cfg.Roles["tester"]
	if name == "" {
		name = cfg.DefaultAgent
	}
	ac, ok := cfg.Agents[name]
	if !ok || (ac.Harness != "codex" && !(ac.Harness == "" && name == "codex")) {
		ac, ok = cfg.Agents["codex"]
		if !ok || (ac.Harness != "" && ac.Harness != "codex") {
			return "", fmt.Errorf("native validation requires an approved Codex agent entry")
		}
	}
	binary, err := exec.LookPath(ac.Binary)
	if err != nil {
		return "", fmt.Errorf("native validation: %w", err)
	}
	return filepath.EvalSymlinks(binary)
}

func nativeEnv(home string, authentication bool, deps Dependencies) []string {
	toolchains := deps.toolchains()
	env := []string{"PATH=" + SandboxPath(toolchains), "HOME=" + home, "GOFLAGS=-buildvcs=false"}
	for _, b := range toolchains {
		if b.EnvKey != "" && b.EnvVal != "" {
			env = append(env, b.EnvKey+"="+b.EnvVal)
		}
	}
	env = append(env, DependencyCacheEnv(deps.Caches)...)
	if authentication {
		// The harness alone opens its normal credential store. Never read or copy it here.
		for _, key := range []string{"CODEX_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
			if value, ok := os.LookupEnv(key); ok {
				env = append(env, key+"="+value)
			}
		}
	}
	return env
}

// codexHostHome resolves a non-/tmp parent HOME for the trusted outer Codex CLI
// process so Codex does not emit a warning refusing to create PATH helper
// aliases when scratch is under /tmp. Inside the sandbox, shell_environment_policy
// still forces HOME and TMPDIR to the isolated scratch directory.
func codexHostHome(fallback string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return home
	}
	return fallback
}

// codexPolicy uses a named native profile rather than workspace-write's broad
// default read access. :minimal supplies Codex's platform runtime files. Each
// build cache is writable; Codex cannot layer a read-only cache, so callers
// pass only caches the run may write. Dependency caches are always read-only.
func codexPolicy(binary, work, scratch string, readOnly bool, caches []BuildCache, deps Dependencies) []string {
	access := "write"
	if readOnly {
		access = "read"
	}
	toolchains := deps.toolchains()
	fs := []string{`":root"="deny"`, `":minimal"="read"`, strconv.Quote(binary) + `="read"`}
	cacheDirs := map[string]bool{}
	for _, c := range caches {
		cacheDirs[c.Dir] = true
	}
	for _, mount := range ToolchainMounts(toolchains, false) {
		if mount != work && mount != scratch && !isSubpath(mount, work) && !isSubpath(mount, scratch) && !cacheDirs[mount] {
			fs = append(fs, strconv.Quote(mount)+`="read"`)
		}
	}
	fs = append(fs, strconv.Quote(work)+"="+strconv.Quote(access), strconv.Quote(scratch)+`="write"`)
	for _, c := range caches {
		fs = append(fs, strconv.Quote(c.Dir)+`="write"`)
	}
	for _, c := range deps.Caches {
		fs = append(fs, strconv.Quote(c.Dir)+`="read"`)
	}
	if readOnly {
		fs = append(fs, strconv.Quote(filepath.Join(work, ".swarm", "active"))+`="write"`)
	}
	envPairs := []string{
		`PATH=` + strconv.Quote(SandboxPath(toolchains)),
		`HOME=` + strconv.Quote(scratch),
		`TMPDIR=` + strconv.Quote(scratch),
		`XDG_CACHE_HOME=` + strconv.Quote(filepath.Join(scratch, ".cache")),
		`XDG_CONFIG_HOME=` + strconv.Quote(filepath.Join(scratch, ".config")),
		`XDG_DATA_HOME=` + strconv.Quote(filepath.Join(scratch, ".local", "share")),
		`GOFLAGS="-buildvcs=false"`,
	}
	for _, b := range toolchains {
		if b.EnvKey != "" && b.EnvVal != "" && !IsBuildCacheKey(b.EnvKey) {
			envPairs = append(envPairs, b.EnvKey+"="+strconv.Quote(b.EnvVal))
		}
	}
	for _, c := range caches {
		envPairs = append(envPairs, c.EnvKey+"="+strconv.Quote(c.Dir))
	}
	for _, c := range deps.Caches {
		envPairs = append(envPairs, c.EnvKey+"="+strconv.Quote(c.Dir))
	}
	config := []string{
		`default_permissions="swarm"`,
		`permissions={swarm={filesystem={` + strings.Join(fs, ",") + `},network={enabled=false}}}`,
		`approval_policy="never"`, `approvals_reviewer="user"`,
		`mcp_servers={}`, `notify=[]`, `web_search="disabled"`,
		`model_provider="openai"`, `model_providers={}`,
		`projects={}`, `profiles={}`,
		`shell_environment_policy={inherit="none",experimental_use_profile=false,set={` + strings.Join(envPairs, ",") + `}}`,
	}
	args := []string{"--no-daemon"}
	for _, setting := range config {
		args = append(args, "-c", setting)
	}
	// These tool surfaces execute outside the native command sandbox or can
	// introduce additional tools. Qualify them separately before enabling them.
	for _, feature := range []string{"hooks", "plugins", "apps", "multi_agent", "browser_use", "computer_use", "image_generation", "memories", "goals", "remote_plugin", "workspace_dependencies", "daemon_auto_start", "shell_snapshot"} {
		args = append(args, "-c", "features."+feature+"=false")
	}
	return args
}

func runNativeCommand(ctx context.Context, binary, dir string, env, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	process.Configure(cmd)
	cmd.Cancel = func() error { return process.StopVerified(cmd) }
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if stopErr := process.StopVerified(cmd); stopErr != nil {
		return fmt.Errorf("terminate native worker: %w", stopErr)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// CheckNativeCodex verifies actual filesystem and loopback restrictions using
// dummy data. No login or provider request is needed for these checks.
func CheckNativeCodex(ctx context.Context, binary string) error {
	_, err := InspectNativeCodex(ctx, binary)
	return err
}

// InspectNativeCodex returns the version whose boundary checks passed.
func InspectNativeCodex(ctx context.Context, binary string) (string, error) {
	if runtime.GOOS != "linux" {
		return "", fmt.Errorf("native Codex isolation is currently qualified on Linux; use a qualified Linux worker (OCI is a separate offline backend)")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	root, err := os.MkdirTemp("", "swarm-native-check-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(root)
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	work, home := filepath.Join(root, "work"), filepath.Join(root, "home")
	for _, dir := range []string{work, home} {
		if err := os.Mkdir(dir, 0700); err != nil {
			return "", err
		}
	}
	var version, versionDiagnostics process.Output
	if err := runNativeCommand(ctx, binary, home, nativeEnv(home, false, Dependencies{}), []string{"--version"}, nil, &version, &versionDiagnostics); err != nil {
		return "", fmt.Errorf("check Codex version: %w", err)
	}
	qualified, err := qualifiedNativeCodexVersion(version.String())
	if err != nil {
		return "", err
	}
	var help process.Output
	if err := runNativeCommand(ctx, binary, home, nativeEnv(home, false, Dependencies{}), []string{"exec", "--help"}, nil, &help, &versionDiagnostics); err != nil {
		return "", fmt.Errorf("inspect Codex execution interface: %w", err)
	}
	for _, flag := range []string{"--ignore-user-config", "--ignore-rules", "--ephemeral", "--skip-git-repo-check"} {
		if !strings.Contains(help.String(), flag) {
			return "", fmt.Errorf("codex %s lacks required execution interface %s", qualified, flag)
		}
	}
	canary := filepath.Join(root, "coordinator-token")
	if err := os.WriteFile(canary, []byte("SWARM_DUMMY_CREDENTIAL"), 0600); err != nil {
		return "", err
	}
	if err := os.Symlink(canary, filepath.Join(work, "outside-link")); err != nil {
		return "", err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("native network canary: %w", err)
	}
	defer listener.Close()
	// Establish reachability outside the tool boundary so ambient runner policy
	// cannot masquerade as enforcement by Codex.
	connection, err := net.DialTimeout("tcp4", listener.Addr().String(), time.Second)
	if err != nil {
		return "", fmt.Errorf("native network canary is unreachable before sandboxing: %w", err)
	}
	connection.Close()
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	// All paths are passed as positional parameters, never interpolated as shell.
	script := `set -eu
test ! -r "$1"
test ! -r outside-link
# Codex may provide a private synthetic /tmp. A write there is harmless;
# the parent verifies that the real host file was never changed.
(printf escape > "$1") 2>/dev/null || true
printf allowed > allowed
test "$(cat allowed)" = allowed
test -x /bin/bash
/bin/bash -c 'exit 0'
if /bin/bash -c 'echo forbidden > /dev/tcp/127.0.0.1/'"$2" 2>/dev/null; then exit 42; fi
printf SWARM_NATIVE_BOUNDARY_OK`
	args := codexPolicy(binary, work, home, false, nil, Dependencies{})
	args = append(args, "sandbox", "-C", work, "-P", "swarm", "--", "/bin/sh", "-c", script, "swarm-canary", canary, port)
	var out, diagnostics process.Output
	if err := runNativeCommand(ctx, binary, home, nativeEnv(codexHostHome(home), false, Dependencies{}), args, nil, &out, &diagnostics); err != nil {
		return "", fmt.Errorf("codex native boundary check failed: %w: %s", err, diagnostics.String())
	}
	data, err := os.ReadFile(canary)
	if err != nil || string(data) != "SWARM_DUMMY_CREDENTIAL" || strings.TrimSpace(out.String()) != "SWARM_NATIVE_BOUNDARY_OK" {
		return "", fmt.Errorf("codex native boundary check did not preserve the outside canary")
	}
	return qualified, nil
}

func CodexAuthentication(ctx context.Context, binary string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var out process.Output
	err = runNativeCommand(ctx, binary, os.TempDir(), nativeEnv(home, true, Dependencies{}), []string{"login", "status"}, nil, &out, &out)
	if err != nil {
		return "", fmt.Errorf("codex login is unavailable; run codex login, then swarm repo doctor")
	}
	if strings.Contains(out.String(), "Logged in using ChatGPT") {
		return "Codex-managed ChatGPT login", nil
	}
	if strings.Contains(out.String(), "Logged in using an API key") {
		return "Codex-managed API key", nil
	}
	return "", fmt.Errorf("codex login status was not recognized; run codex login")
}

// RunNativeCodex leaves provider authentication in the trusted Codex process.
// Model-controlled commands use Codex's native profile, with no host credentials.
func RunNativeCodex(ctx context.Context, dir, binary, model, prompt string, opts WorkerOptions, interactive bool, stdin io.Reader, stdout, stderr io.Writer) ([]string, string, error) {
	preflight := time.Now()
	version, err := InspectNativeCodex(ctx, binary)
	if err != nil {
		return nil, "", err
	}
	if _, err := CodexAuthentication(ctx, binary); err != nil {
		return nil, version, err
	}
	opts.Lifecycle.addPreflight(preflight)
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, version, err
	}
	var command []string
	err = withNativeWorkspace(ctx, dir, opts, func(ctx context.Context, work, scratch string) error {
		// dir is the real worktree, so the cache key names its repository.
		caches, err := agentBuildCaches(ctx, dir, scratch, opts)
		if err != nil {
			return err
		}
		args := codexPolicy(binary, work, scratch, opts.ReadOnly, caches, opts.Dependencies)
		args = append(args, codexExecArgs(work, model, opts.Effort)...)
		return runFollowupTurns(ctx, prompt, "Continue the same SwarmOS task. Read .swarm/active/SYSTEM_PROMPT.md and .swarm/active/WORKUNIT.md.\n", interactive, true, stdin, stdout, func(turn string) error {
			command = append(append([]string{binary}, args...), turn)
			if err := runNativeCommand(ctx, binary, work, nativeEnv(home, true, opts.Dependencies), command[1:], nil, stdout, stderr); err != nil {
				return fmt.Errorf("native Codex run: %w", err)
			}
			return nil
		}, opts.GuidanceDelivered)
	})
	return command, version, err
}

// codexExecArgs is the native `codex exec` argv after the sandbox policy and
// before the prompt. Only the approved model and effort selectors vary.
func codexExecArgs(work, model, effort string) []string {
	args := []string{"exec", "--ignore-user-config", "--ignore-rules", "--ephemeral", "--skip-git-repo-check", "-C", work}
	if model != "" {
		args = append(args, "--model", model)
	}
	if effort != "" {
		args = append(args, "-c", CodexEffortConfig(effort))
	}
	return args
}

// CodexEffortConfig is the Codex -c override that selects reasoning effort.
func CodexEffortConfig(effort string) string {
	return "model_reasoning_effort=" + strconv.Quote(effort)
}

// RunNativeValidation runs command with the validator's caches as selected by
// ValidatorCacheMounts: writable only during warm-up, scratch otherwise.
// Dependency caches are read-only in every run.
func RunNativeValidation(ctx context.Context, dir, binary, command string, caches []BuildCache, deps Dependencies, warmup bool, stdout, stderr io.Writer) error {
	if err := CheckNativeCodex(ctx, binary); err != nil {
		return err
	}
	return withNativeWorkspace(ctx, dir, WorkerOptions{DiscardChanges: true}, func(ctx context.Context, work, scratch string) error {
		mounts, err := ValidatorCacheMounts(caches, ValidationCodex, warmup, false, scratch)
		if err != nil {
			return err
		}
		args := codexPolicy(binary, work, scratch, false, mounts, deps)
		args = append(args, "sandbox", "-C", work, "-P", "swarm", "--", "/bin/sh", "-c", command)
		return runNativeCommand(ctx, binary, scratch, nativeEnv(codexHostHome(scratch), false, deps), args, nil, stdout, stderr)
	})
}

// Reuse the OCI filesystem acceptance rules. Native workers also operate on a
// detached snapshot, so tools cannot access live Git or coordinator state.
func withNativeWorkspace(ctx context.Context, dir string, opts WorkerOptions, run func(context.Context, string, string) error) (resultErr error) {
	ctx, budget := startAgentBudget(ctx, opts)
	defer budget.close()
	defer func() {
		if budget.timedOut() {
			var reported *AgentTimeoutError
			if !errors.As(resultErr, &reported) {
				resultErr = &AgentTimeoutError{cause: budget.timeoutError(), diagnostic: "timeout during workspace setup; no patch available"}
			}
		}
	}()
	setup := time.Now()
	root, err := os.MkdirTemp("", "swarm-native-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	work, scratch := filepath.Join(root, "workspace"), filepath.Join(root, "scratch")
	for _, p := range []string{work, scratch} {
		if err := os.Mkdir(p, 0700); err != nil {
			return err
		}
	}
	original, err := safefs.Open(dir)
	if err != nil {
		return err
	}
	defer original.Close()
	outputs := workerOutputs(opts)
	input := filepath.Join(root, "input.tar")
	opts.Lifecycle.addSetup(setup)
	snapshot := time.Now()
	initial, err := packWorker(ctx, dir, input, outputs, opts.ReadOnly, ".codex", ".agents")
	if err != nil {
		return err
	}
	if _, err := unpackWorker(ctx, input, work); err != nil {
		return err
	}
	opts.Lifecycle.addSnapshot(snapshot)
	if err := os.MkdirAll(filepath.Join(work, ".swarm", "active"), 0700); err != nil {
		return err
	}
	runErr := run(ctx, work, scratch)
	// Observe the harness's own scratch-HOME session log before cleanup.
	opts.Lifecycle.observe(scratch)
	budget.finish()
	if budget.timedOut() {
		destination := opts.RecoveryRoot
		if destination == "" {
			destination = dir
		}
		recoveryCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		patch, preserveErr := preserveTimeoutPatch(recoveryCtx, input, work, destination, opts.Role)
		if preserveErr != nil {
			return &AgentTimeoutError{cause: budget.timeoutError(), diagnostic: fmt.Sprintf("timeout patch preservation failed: %q", preserveErr.Error())}
		}
		return &AgentTimeoutError{cause: budget.timeoutError(), diagnostic: fmt.Sprintf("recovery patch: %s", patch)}
	}
	paused := false
	if cleanup, cancel, ok := claimpause.Cleanup(ctx, opts.Role); ok && !opts.ReadOnly && !opts.DiscardChanges && (runErr == nil || errors.Is(runErr, context.Canceled)) {
		defer cancel()
		ctx = cleanup
		paused = true
	} else if runErr != nil {
		return runErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if opts.DiscardChanges {
		return nil
	}
	if opts.ReadOnly {
		// Validate every role output before replacing any existing artifact.
		artifacts := make([][]byte, len(outputs))
		for i, name := range outputs {
			if err := ctx.Err(); err != nil {
				return err
			}
			artifacts[i], err = safefs.ReadFile(work, filepath.Join(".swarm", "active", name), 1<<20)
			if err != nil {
				return fmt.Errorf("read native role output %s: %w", name, err)
			}
		}
		destination := original
		if opts.OutputDir != "" {
			destination, err = safefs.Open(opts.OutputDir)
			if err != nil {
				return err
			}
			defer destination.Close()
		}
		for i, name := range outputs {
			dest := filepath.Join(".swarm", "active", name)
			if err := ctx.Err(); err != nil {
				return err
			}
			if opts.OutputDir != "" {
				dest = name
			}
			if err := destination.WriteFile(dest, artifacts[i], 0644); err != nil {
				return err
			}
		}
		return nil
	}
	if paused {
		for _, name := range []string{".git", ".swarm/coordination", ".swarm/local-tracker", ".swarm/worktrees", ".swarm/active-output"} {
			if _, err := os.Lstat(filepath.Join(work, filepath.FromSlash(name))); !os.IsNotExist(err) {
				return fmt.Errorf("paused snapshot contains control path %s", name)
			}
		}
	}
	archive := filepath.Join(root, "result.tar")
	if _, err := packWorker(ctx, work, archive, nil, false, ".codex", ".agents"); err != nil {
		return err
	}
	result := filepath.Join(root, "result")
	if err := os.Mkdir(result, 0700); err != nil {
		return err
	}
	final, err := unpackWorker(ctx, archive, result)
	if err != nil {
		return err
	}
	if err := checkImport(ctx, opts, initial, final); err != nil {
		return preserveRejected(ctx, opts, input, result, err)
	}
	if err := importWorker(ctx, original, result, initial, final); err != nil {
		return err
	}
	if paused {
		return claimpause.ErrBudget
	}
	return nil
}
