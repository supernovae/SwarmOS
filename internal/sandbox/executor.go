package sandbox

import (
	"context"
	"encoding/json"
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

	"github.com/supernovae/swarmos/internal/process"
	"github.com/supernovae/swarmos/internal/storageuse"
)

// ToolExecutor keeps harness authentication in the harness. Only detached task
// files cross into Bubblewrap; tools never inherit harness or coordinator state.
type ToolExecutor struct {
	BrowserProfile bool
	Work           string
	ReadOnly       bool
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
	flags, err := executorProfileFlags(work, e.ReadOnly, e.BuildCaches, e.Dependencies, script, args, e.BrowserProfile)
	if err != nil {
		return nil, err
	}
	if e.BrowserProfile {
		if _, err := browserShell(e.Dependencies, work); err != nil {
			return nil, err
		}
		cmd, _, err := browserCommand(ctx, binary, flags)
		return cmd, err
	}
	cmd := exec.CommandContext(ctx, binary, flags...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent"}
	process.Configure(cmd)
	cmd.Cancel = func() error { return process.StopVerified(cmd) }
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

func (e ToolExecutor) Run(ctx context.Context, script string, stdout, stderr io.Writer, args ...string) (resultErr error) {
	// The external internal-tools process carries worker data, never coordinator
	// storage authority. Its enclosing delegated run retains every cache use.
	// In-process coordinator callers can share an existing scope across tools.
	if storageuse.FromContext(ctx) != nil {
		if err := acquireRuntimeCachesMode(ctx, e.BuildCaches, e.Dependencies, !e.BrowserProfile); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, toolCommandSeconds*time.Second)
	defer cancel()
	cmd, err := e.command(ctx, script, args...)
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	var reservation *storageuse.BoundaryReservation
	var infoRead, infoWrite *os.File
	if scope := storageuse.FromContext(ctx); scope != nil {
		if e.BrowserProfile {
			if err := scope.OutsideConsumer(ctx); err != nil {
				return err
			}
		} else {
			locations := []string{e.Work}
			for _, c := range e.BuildCaches {
				locations = append(locations, c.Dir)
			}
			for _, c := range e.Dependencies.Caches {
				locations = append(locations, c.Dir)
			}
			reservation, err = scope.ReserveBoundary(ctx, locations)
			if err != nil {
				return err
			}
			infoRead, infoWrite, err = os.Pipe()
			if err != nil {
				return err
			}
			defer infoRead.Close()
			defer infoWrite.Close()
			cmd.ExtraFiles = []*os.File{infoWrite}
			cmd.Args = append(cmd.Args[:1], append([]string{"--info-fd", "3"}, cmd.Args[1:]...)...)
		}
	}
	err = cmd.Start()
	if infoWrite != nil {
		_ = infoWrite.Close()
	}
	if err == nil {
		if reservation != nil {
			// Missing/short-lived identity leaves the durable reservation incomplete.
			// It does not weaken execution isolation or invent evidence after exit.
			_ = infoRead.SetReadDeadline(time.Now().Add(time.Second))
			var status struct {
				PID int `json:"child-pid"`
			}
			if json.NewDecoder(io.LimitReader(infoRead, 4096)).Decode(&status) == nil {
				if identity, identityErr := executorBoundaryIdentity(status.PID, cmd.Process.Pid); identityErr == nil {
					if sealErr := reservation.Seal(ctx, identity); sealErr != nil {
						// A partially sealed multi-resource launch remains permanently uncertain.
						// Retain all scope uses and prevent execution from being reported complete.
						storageuse.FromContext(ctx).Retain()
						err = sealErr
					}
				}
			}
		}
		err = errors.Join(err, cmd.Wait())
	}
	stopErr := process.StopVerified(cmd)
	if e.BrowserProfile {
		var unit string
		for _, arg := range cmd.Args {
			if strings.HasPrefix(arg, "--unit=swarm-browser-") {
				unit = strings.TrimPrefix(arg, "--unit=")
				break
			}
		}
		if unit == "" {
			stopErr = errors.Join(stopErr, fmt.Errorf("browser scope unit missing"))
		} else {
			stopErr = errors.Join(stopErr, stopBrowserScope(unit))
		}
	}
	if stopErr != nil {
		if scope := storageuse.FromContext(ctx); scope != nil {
			scope.Retain()
		}
		return errors.Join(err, stopErr)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// CheckToolExecutor tests the same command builder used by every tool. The
// positive network probe prevents an ambient firewall from passing this check.
func CheckToolExecutor(ctx context.Context) error { return checkToolExecutor(ctx, false) }

func checkToolExecutor(ctx context.Context, browser bool) (resultErr error) {
	ctx, own, err := storageuse.Ensure(ctx, "host")
	if err != nil {
		return err
	}
	if own != nil {
		defer own.Complete(&resultErr)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	root, err := storageuse.FromContext(ctx).TempDir(ctx, "", "swarm-executor-check-*", "sandbox-scratch")
	if err != nil {
		return err
	}
	defer storageuse.FromContext(ctx).CleanupTemp(root, &resultErr)
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
	e := ToolExecutor{Work: work, BrowserProfile: browser}
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

// acquireRuntimeCaches reacquires managed readers for standalone role and tool
// invocations. Descriptors remain data-only; lifetime authority stays in ctx.
func acquireRuntimeCaches(ctx context.Context, caches []BuildCache, deps Dependencies) error {
	return acquireRuntimeCachesMode(ctx, caches, deps, false)
}

func acquireRuntimeCachesMode(ctx context.Context, caches []BuildCache, deps Dependencies, contained bool) error {
	scope := storageuse.FromContext(ctx)
	if scope == nil {
		return fmt.Errorf("storage use scope required for cache readers")
	}
	admit := func(path string) error {
		path = filepath.Clean(path)
		keyDir := filepath.Dir(path)
		class := filepath.Base(filepath.Dir(keyDir))
		switch class {
		case "agent-build", "validate-build", "agent-deps", "validate-deps":
		default:
			return nil
		}
		key := filepath.Base(keyDir)
		if len(key) != 32 {
			return fmt.Errorf("invalid managed cache repository key")
		}
		resource := storageuse.ResourceFor(key, class, path)
		if contained {
			return scope.AcquirePrepared(ctx, resource)
		}
		return scope.Acquire(ctx, resource)
	}
	for _, c := range caches {
		if err := admit(c.Dir); err != nil {
			return err
		}
	}
	for _, c := range deps.Caches {
		if err := admit(c.Dir); err != nil {
			return err
		}
	}
	return nil
}

func executorProfileFlags(work string, readOnly bool, caches []BuildCache, deps Dependencies, script string, args []string, browser bool) ([]string, error) {
	flags, err := executorFlags(work, readOnly, caches, deps, script, args)
	if err != nil {
		return nil, err
	}
	if browser {
		// This argv is executable only through the scope's pre-exec verifier.
		for i, flag := range flags {
			if flag == "--as=8589934592" {
				return append(flags[:i], flags[i+1:]...), nil
			}
		}
		return nil, fmt.Errorf("browser profile address-space limit missing")
	}
	return flags, nil
}

// executorBoundaryIdentity verifies the coordinator's Bubblewrap status channel
// names the private PID namespace init, rather than its outer monitor. Workers
// cannot supply either the status pipe or the PID used here.
func executorBoundaryIdentity(pid, launcher int) (storageuse.ProcessIdentity, error) {
	identity, err := storageuse.CaptureProcess(pid)
	if err != nil {
		return storageuse.ProcessIdentity{}, err
	}
	status, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return storageuse.ProcessIdentity{}, err
	}
	parentOK, initOK := false, false
	for _, line := range strings.Split(string(status), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "PPid:" {
			parentOK = fields[1] == strconv.Itoa(launcher)
		}
		if len(fields) >= 3 && fields[0] == "NSpid:" {
			initOK = fields[1] == strconv.Itoa(pid) && fields[len(fields)-1] == "1"
		}
	}
	ownNS, ownErr := os.Readlink("/proc/self/ns/pid")
	childNS, childErr := os.Readlink("/proc/" + strconv.Itoa(pid) + "/ns/pid")
	again, captureErr := storageuse.CaptureProcess(pid)
	if !parentOK || !initOK || ownErr != nil || childErr != nil || childNS == ownNS || captureErr != nil || again != identity {
		return storageuse.ProcessIdentity{}, storageuse.ErrUnsafe
	}
	return identity, nil
}
