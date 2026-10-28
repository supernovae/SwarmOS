//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/supernovae/swarmos/internal/process"
	"golang.org/x/sys/unix"
)

// Bound runtime parallelism before Go/cgo startup: the runtime sizes per-P
// allocations from GOMAXPROCS before main, and host CPU counts can exhaust the
// address-space limit even before the helper validates its arguments. This is
// helper-only; the payload receives its own explicit environment.
const restoreHelperMaxProcs = "2"

func restoreResourceFlags() []string {
	return []string{"--as=1073741824", "--cpu=300", "--nproc=128", "--nofile=128", "--fsize=268435456", "--core=0"}
}

func restoreCommand(ctx context.Context, control, cache, positive string, argv []string) (*exec.Cmd, error) {
	binary, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("restore Bubblewrap unavailable: %w", err)
	}
	flags, err := restoreFlags(control, cache, positive, argv)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, binary, flags...)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	process.Configure(cmd)
	cmd.Cancel = func() error { return process.StopVerified(cmd) }
	cmd.WaitDelay = 5 * time.Second
	return cmd, nil
}
func restoreFlags(control, cache, positive string, argv []string) ([]string, error) {
	flags := []string{"--unshare-all", "--disable-userns", "--assert-userns-disabled", "--die-with-parent", "--new-session", "--cap-drop", "ALL", "--clearenv",
		"--ro-bind", "/usr/bin", "/usr/bin", "--ro-bind", "/usr/lib", "/usr/lib", "--proc", "/proc", "--dev", "/dev"}
	for _, path := range []string{"/bin", "/lib", "/lib64", "/usr/lib64"} {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
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
	flags = append(flags, "--ro-bind", filepath.Join(control, "source"), "/source", "--bind", cache, "/cache",
		"--ro-bind", filepath.Join(control, "trust"), "/restore-trust", "--ro-bind", filepath.Join(control, "socket"), "/restore-broker",
		"--ro-bind", filepath.Join(control, "helper"), "/restore-helper", "--remount-ro", "/proc", "--remount-ro", "/dev", "--remount-ro", "/",
		"--setenv", "GOMAXPROCS", restoreHelperMaxProcs, "--setenv", "PATH", "/usr/bin:/bin", "--setenv", "HOME", "/cache/home", "--setenv", "TMPDIR", "/cache/tmp", "--chdir", "/source", "--",
		"/usr/bin/prlimit")
	flags = append(flags, restoreResourceFlags()...)
	flags = append(flags, "--", "/restore-helper", positive, "--")
	return append(flags, argv...), nil
}

func restoreCloseDescriptors() error {
	// Mark every non-control descriptor close-on-exec before starting goroutines.
	// This also excludes a deliberately non-CLOEXEC host sentinel descriptor.
	return unix.CloseRange(3, ^uint(0), unix.CLOSE_RANGE_CLOEXEC)
}
