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
	"time"

	"github.com/supernovae/swarmos/internal/process"
)

// ToolExecutor keeps harness authentication in the harness. Only detached task
// files cross into Bubblewrap; tools never inherit harness or coordinator state.
type ToolExecutor struct {
	Work     string
	ReadOnly bool
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
	flags := []string{"--unshare-all", "--die-with-parent", "--new-session", "--cap-drop", "ALL", "--clearenv",
		"--ro-bind", "/usr", "/usr", "--proc", "/proc", "--dev", "/dev", "--size", "268435456", "--tmpfs", "/tmp"}
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
	bind := "--bind"
	if e.ReadOnly {
		bind = "--ro-bind"
	}
	flags = append(flags, bind, work, "/workspace")
	if e.ReadOnly {
		flags = append(flags, "--bind", filepath.Join(work, ".swarm", "active"), "/workspace/.swarm/active")
	}
	flags = append(flags, "--dir", "/home/worker", "--setenv", "HOME", "/home/worker", "--setenv", "TMPDIR", "/tmp",
		"--setenv", "PATH", "/usr/local/bin:/usr/bin:/bin", "--chdir", "/workspace", "--",
		"/usr/bin/prlimit", "--as=2147483648", "--cpu=120", "--nproc=128", "--nofile=256", "--fsize=67108864", "--core=0", "--",
		"/bin/sh", "-c", script, "swarm-tool")
	flags = append(flags, args...)
	cmd := exec.CommandContext(ctx, binary, flags...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent"}
	process.Configure(cmd)
	cmd.Cancel = func() error { process.Stop(cmd); return nil }
	cmd.WaitDelay = time.Second
	return cmd, nil
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
