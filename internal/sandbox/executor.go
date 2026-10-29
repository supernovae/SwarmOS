package sandbox

import (
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

	"github.com/supernovae/swarmos/internal/process"
)

// ToolExecutor keeps harness authentication in the harness. Only detached task
// files cross into Bubblewrap; tools never inherit harness or coordinator state.
type ToolExecutor struct {
	Work     string
	ReadOnly bool
	// BuildCaches are bound at their host paths; overlay caches are read-only
	// with a write layer discarded when the command exits.
	BuildCaches []BuildCache
	// Dependencies are always bound read-only; only the host prefetch
	// writes them.
	Dependencies Dependencies
}

func (e ToolExecutor) command(ctx context.Context, script string, args ...string) (*exec.Cmd, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("isolated tool execution requires Linux with Bubblewrap; on Windows use WSL2")
	}
	binary, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("install bubblewrap for isolated harness tools: %w", err)
	}
	work, err := filepath.EvalSymlinks(e.Work)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(work) || work == "/" {
		return nil, fmt.Errorf("executor requires an absolute task workspace")
	}
	flags, err := executorFlags(work, e.ReadOnly, e.BuildCaches, e.Dependencies, script, args)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, binary, flags...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent"}
	process.Configure(cmd)
	cmd.Cancel = func() error { process.Stop(cmd); return nil }
	cmd.WaitDelay = time.Second
	return cmd, nil
}

// executorFlags builds the Bubblewrap arguments for a resolved workspace.
func executorFlags(work string, readOnly bool, caches []BuildCache, deps Dependencies, script string, args []string) ([]string, error) {
	flags := []string{"--unshare-all", "--die-with-parent", "--new-session", "--cap-drop", "ALL", "--clearenv",
		"--ro-bind", "/usr", "/usr", "--proc", "/proc", "--dev", "/dev", "--size", "1073741824", "--tmpfs", "/tmp"}
	// Account for both merged-/usr and older Linux distributions.
	for _, path := range []string{"/bin", "/lib", "/lib64"} {
		info, statErr := os.Lstat(path)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return nil, statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return nil, err
			}
			flags = append(flags, "--symlink", target, path)
		} else {
			flags = append(flags, "--ro-bind", path, path)
		}
	}
	toolchains := deps.toolchains()
	for _, mount := range ToolchainMounts(toolchains, true) {
		if mount != work && !isSubpath(mount, work) {
			flags = append(flags, "--ro-bind", mount, mount)
		}
	}
	var cacheEnv []string
	for _, c := range caches {
		if !IsBuildCacheKey(c.EnvKey) {
			return nil, fmt.Errorf("build cache %q has no table entry", c.EnvKey)
		}
		dir, err := filepath.EvalSymlinks(c.Dir)
		if err != nil {
			return nil, fmt.Errorf("build cache %s: %w", c.Ecosystem, err)
		}
		if !filepath.IsAbs(dir) || dir == "/" || isSubpath(dir, work) || isSubpath(work, dir) {
			return nil, fmt.Errorf("build cache %s must be an absolute directory outside the workspace", c.Ecosystem)
		}
		if c.Overlay {
			flags = append(flags, "--overlay-src", dir, "--tmp-overlay", dir)
		} else {
			flags = append(flags, "--bind", dir, dir)
		}
		cacheEnv = append(cacheEnv, "--setenv", c.EnvKey, dir)
	}
	for _, c := range deps.Caches {
		if err := checkDependencyCache(c, work); err != nil {
			return nil, err
		}
		dir, err := filepath.EvalSymlinks(c.Dir)
		if err != nil {
			return nil, fmt.Errorf("dependency cache %s: %w", c.Ecosystem, err)
		}
		if isSubpath(dir, work) || isSubpath(work, dir) {
			return nil, fmt.Errorf("dependency cache %s must be an absolute directory outside the workspace", c.Ecosystem)
		}
		flags = append(flags, "--ro-bind", dir, dir)
		c.Dir = dir
		for _, entry := range c.env() {
			key, value, _ := strings.Cut(entry, "=")
			cacheEnv = append(cacheEnv, "--setenv", key, value)
		}
	}
	bind := "--bind"
	if readOnly {
		bind = "--ro-bind"
	}
	flags = append(flags, bind, work, "/workspace")
	if readOnly {
		flags = append(flags, "--bind", filepath.Join(work, ".swarm", "active"), "/workspace/.swarm/active")
	}
	flags = append(flags, "--dir", "/home/worker", "--setenv", "HOME", "/home/worker", "--setenv", "TMPDIR", "/tmp",
		"--setenv", "XDG_CACHE_HOME", "/home/worker/.cache", "--setenv", "XDG_CONFIG_HOME", "/home/worker/.config", "--setenv", "XDG_DATA_HOME", "/home/worker/.local/share",
		"--setenv", "GOFLAGS", linuxGoFlags, "--setenv", "GOMAXPROCS", linuxGoMaxProcs, "--setenv", "PATH", SandboxPath(toolchains))
	for _, b := range toolchains {
		if b.EnvKey != "" && b.EnvVal != "" && !IsBuildCacheKey(b.EnvKey) {
			flags = append(flags, "--setenv", b.EnvKey, b.EnvVal)
		}
	}
	flags = append(flags, cacheEnv...)
	flags = append(flags, "--chdir", "/workspace", "--",
		"/usr/bin/prlimit", "--as=8589934592", "--cpu=300", "--nproc=1024", "--nofile=4096", "--fsize=268435456", "--core=0", "--",
		"/bin/sh", "-c", script, "swarm-tool")
	return append(flags, args...), nil
}

func (e ToolExecutor) Run(ctx context.Context, script string, stdout, stderr io.Writer, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd, err := e.command(ctx, script, args...)
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err = cmd.Run()
	process.Stop(cmd)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// CheckToolExecutor tests the same command builder used by every tool. The
// positive network probe prevents an ambient firewall from passing this check.
func CheckToolExecutor(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	root, err := os.MkdirTemp("", "swarm-executor-check-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	work := filepath.Join(root, "workspace")
	if err := os.MkdirAll(filepath.Join(work, ".swarm", "active"), 0700); err != nil {
		return err
	}
	secret := filepath.Join(root, "coordinator-token")
	if err := os.WriteFile(secret, []byte("synthetic-secret"), 0600); err != nil {
		return err
	}
	if err := os.Symlink(secret, filepath.Join(work, "escape")); err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("executor network canary: %w", err)
	}
	defer listener.Close()
	conn, err := net.DialTimeout("tcp4", listener.Addr().String(), time.Second)
	if err != nil {
		return err
	}
	conn.Close()
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	var out process.Output
	e := ToolExecutor{Work: work}
	script := `set -eu
test ! -r "$1"
test ! -r escape
test ! -e "/proc/1/root$1"
test -z "${GH_TOKEN+x}${GITHUB_TOKEN+x}${OPENAI_API_KEY+x}"
printf allowed > source
test "$(cat source)" = allowed
/usr/bin/python3 -c 'from pathlib import Path; assert Path("source").read_text() == "allowed"'
if /bin/bash -c 'echo forbidden > /dev/tcp/127.0.0.1/'"$2" 2>/dev/null; then exit 42; fi
printf EXECUTOR_OK`
	if err := e.Run(ctx, script, &out, &out, secret, port); err != nil {
		return fmt.Errorf("isolated tool canary: %w: %s", err, out.String())
	}
	if out.String() != "EXECUTOR_OK" {
		return fmt.Errorf("isolated tool canary returned unexpected output")
	}
	e.ReadOnly = true
	if err := e.Run(ctx, `set -eu; if (echo bad > source) 2>/dev/null; then exit 43; fi; echo allowed > .swarm/active/probe`, &out, &out); err != nil {
		return fmt.Errorf("read-only tool canary: %w: %s", err, out.String())
	}
	data, err := os.ReadFile(secret)
	if err != nil || string(data) != "synthetic-secret" {
		return fmt.Errorf("executor changed host canary")
	}
	return nil
}
