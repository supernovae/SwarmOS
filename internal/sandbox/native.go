package sandbox

import (
	"bufio"
	"context"
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

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/process"
	"github.com/supernovae/swarmos/internal/safefs"
)

// NativeCodexVersion pins the tool/configuration surface qualified by the adapter.
// Expanding this range requires running the native boundary tests on that version.
const NativeCodexVersion = "0.163.0"

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

func nativeEnv(home string, authentication bool) []string {
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + home}
	if authentication {
		// Codex alone opens its normal credential store. Never read or copy it here.
		for _, key := range []string{"CODEX_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
			if value, ok := os.LookupEnv(key); ok {
				env = append(env, key+"="+value)
			}
		}
	}
	return env
}

// codexPolicy uses a named native profile rather than workspace-write's broad
// default read access. :minimal supplies Codex's platform runtime files.
func codexPolicy(binary, work, scratch string, readOnly bool) []string {
	access := "write"
	if readOnly {
		access = "read"
	}
	fs := []string{`":root"="deny"`, `":minimal"="read"`, strconv.Quote(binary) + `="read"`, strconv.Quote(work) + "=" + strconv.Quote(access), strconv.Quote(scratch) + `="write"`}
	if readOnly {
		fs = append(fs, strconv.Quote(filepath.Join(work, ".swarm", "active"))+`="write"`)
	}
	config := []string{
		`default_permissions="swarm"`,
		`permissions={swarm={filesystem={` + strings.Join(fs, ",") + `},network={enabled=false}}}`,
		`approval_policy="never"`, `approvals_reviewer="user"`,
		`mcp_servers={}`, `notify=[]`, `web_search="disabled"`,
		`model_provider="openai"`, `model_providers={}`,
		`projects={}`, `profiles={}`,
		`shell_environment_policy={inherit="none",experimental_use_profile=false,set={PATH="/usr/bin:/bin",HOME=` + strconv.Quote(scratch) + `,TMPDIR=` + strconv.Quote(scratch) + `}}`,
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
	cmd.Cancel = func() error { process.Stop(cmd); return nil }
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	process.Stop(cmd)
	return err
}

// CheckNativeCodex verifies actual filesystem and loopback restrictions using
// dummy data. No login or provider request is needed for these checks.
func CheckNativeCodex(ctx context.Context, binary string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("native Codex isolation is currently qualified on Linux; select execution.backend: oci for a qualified container setup")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	root, err := os.MkdirTemp("", "swarm-native-check-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	work, home := filepath.Join(root, "work"), filepath.Join(root, "home")
	for _, dir := range []string{work, home} {
		if err := os.Mkdir(dir, 0700); err != nil {
			return err
		}
	}
	var version, versionDiagnostics process.Output
	if err := runNativeCommand(ctx, binary, home, nativeEnv(home, false), []string{"--version"}, nil, &version, &versionDiagnostics); err != nil {
		return fmt.Errorf("check Codex version: %w", err)
	}
	if strings.TrimSpace(version.String()) != "codex-cli "+NativeCodexVersion {
		return fmt.Errorf("native isolation requires qualified Codex %s; update Codex or select a supported execution backend", NativeCodexVersion)
	}
	canary := filepath.Join(root, "coordinator-token")
	if err := os.WriteFile(canary, []byte("SWARM_DUMMY_CREDENTIAL"), 0600); err != nil {
		return err
	}
	if err := os.Symlink(canary, filepath.Join(work, "outside-link")); err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("native network canary: %w", err)
	}
	defer listener.Close()
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
	args := codexPolicy(binary, work, home, false)
	args = append(args, "sandbox", "-C", work, "-P", "swarm", "--", "/bin/sh", "-c", script, "swarm-canary", canary, port)
	var out, diagnostics process.Output
	if err := runNativeCommand(ctx, binary, home, nativeEnv(home, false), args, nil, &out, &diagnostics); err != nil {
		return fmt.Errorf("Codex native boundary check failed: %w: %s", err, diagnostics.String())
	}
	data, err := os.ReadFile(canary)
	if err != nil || string(data) != "SWARM_DUMMY_CREDENTIAL" || strings.TrimSpace(out.String()) != "SWARM_NATIVE_BOUNDARY_OK" {
		return fmt.Errorf("Codex native boundary check did not preserve the outside canary")
	}
	return nil
}

func CodexAuthentication(ctx context.Context, binary string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var out process.Output
	err = runNativeCommand(ctx, binary, os.TempDir(), nativeEnv(home, true), []string{"login", "status"}, nil, &out, &out)
	if err != nil {
		return "", fmt.Errorf("Codex login is unavailable; run codex login, then swarm doctor")
	}
	if strings.Contains(out.String(), "Logged in using ChatGPT") {
		return "Codex-managed ChatGPT login", nil
	}
	if strings.Contains(out.String(), "Logged in using an API key") {
		return "Codex-managed API key", nil
	}
	return "", fmt.Errorf("Codex login status was not recognized; run codex login")
}

// RunNativeCodex leaves provider authentication in the trusted Codex process.
// Model-controlled commands use Codex's native profile, with no host credentials.
func RunNativeCodex(ctx context.Context, dir, binary, model, prompt string, opts WorkerOptions, interactive bool, stdin io.Reader, stdout, stderr io.Writer) ([]string, error) {
	if err := CheckNativeCodex(ctx, binary); err != nil {
		return nil, err
	}
	if _, err := CodexAuthentication(ctx, binary); err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	var command []string
	err = withNativeWorkspace(ctx, dir, opts, func(ctx context.Context, work, scratch string) error {
		args := codexPolicy(binary, work, scratch, opts.ReadOnly)
		args = append(args, "exec", "--ignore-user-config", "--ignore-rules", "--ephemeral", "--skip-git-repo-check", "-C", work)
		if model != "" {
			args = append(args, "--model", model)
		}
		reader := bufio.NewReader(strings.NewReader(""))
		if stdin != nil {
			reader = bufio.NewReader(stdin)
		}
		for {
			command = append(append([]string{binary}, args...), prompt)
			if err := runNativeCommand(ctx, binary, work, nativeEnv(home, true), command[1:], nil, stdout, stderr); err != nil {
				return fmt.Errorf("native Codex run: %w", err)
			}
			if !interactive {
				return nil
			}
			fmt.Fprint(stdout, "\nFollow-up instruction (Enter to finish this phase): ")
			line, err := nativeInstruction(ctx, reader)
			if err != nil && err != io.EOF {
				return err
			}
			line = strings.TrimSpace(line)
			if line == "" {
				return nil
			}
			prompt = "Continue the same SwarmOS task. Read .swarm/active/SYSTEM_PROMPT.md and .swarm/active/WORKUNIT.md.\n" + line
		}
	})
	return command, err
}

func nativeInstruction(ctx context.Context, reader *bufio.Reader) (string, error) {
	type result struct {
		line string
		err  error
	}
	ready := make(chan result, 1)
	go func() { line, err := reader.ReadString('\n'); ready <- result{line, err} }()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-ready:
		return r.line, r.err
	}
}

func RunNativeValidation(ctx context.Context, dir, binary, command string, stdout, stderr io.Writer) error {
	if err := CheckNativeCodex(ctx, binary); err != nil {
		return err
	}
	return withNativeWorkspace(ctx, dir, WorkerOptions{DiscardChanges: true}, func(ctx context.Context, work, scratch string) error {
		args := codexPolicy(binary, work, scratch, false)
		args = append(args, "sandbox", "-C", work, "-P", "swarm", "--", "/bin/sh", "-c", command)
		return runNativeCommand(ctx, binary, scratch, nativeEnv(scratch, false), args, nil, stdout, stderr)
	})
}

// Reuse the OCI filesystem acceptance rules. Native workers also operate on a
// detached snapshot, so tools cannot access live Git or coordinator state.
func withNativeWorkspace(ctx context.Context, dir string, opts WorkerOptions, run func(context.Context, string, string) error) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
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
	initial, err := packWorker(ctx, dir, input, outputs, opts.ReadOnly, ".codex", ".agents")
	if err != nil {
		return err
	}
	if _, err := unpackWorker(ctx, input, work); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(work, ".swarm", "active"), 0700); err != nil {
		return err
	}
	if err := run(ctx, work, scratch); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if opts.DiscardChanges {
		return nil
	}
	if opts.ReadOnly {
		destination := original
		if opts.OutputDir != "" {
			destination, err = safefs.Open(opts.OutputDir)
			if err != nil {
				return err
			}
			defer destination.Close()
		}
		for _, name := range outputs {
			data, err := safefs.ReadFile(work, filepath.Join(".swarm", "active", name), 1<<20)
			if err != nil {
				return fmt.Errorf("read native role output %s: %w", name, err)
			}
			dest := filepath.Join(".swarm", "active", name)
			if opts.OutputDir != "" {
				dest = name
			}
			if err := destination.WriteFile(dest, data, 0644); err != nil {
				return err
			}
		}
		return nil
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
	return importWorker(ctx, original, result, initial, final)
}
