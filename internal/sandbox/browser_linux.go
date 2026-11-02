//go:build linux

package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/process"
	"github.com/supernovae/swarmos/internal/storageuse"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// verifyBrowserScope reads the kernel's membership and memory controller state,
// rather than accepting a successful systemd property assignment as evidence.
func verifyBrowserScope(unit string, readFile func(string) ([]byte, error)) error {
	if !strings.HasPrefix(unit, "swarm-browser-") || strings.ContainsAny(unit, "/\\\n\r") || !strings.HasSuffix(unit, ".scope") {
		return fmt.Errorf("invalid browser scope unit")
	}
	raw, err := readFile("/proc/self/cgroup")
	if err != nil {
		return fmt.Errorf("browser scope membership: %w", err)
	}
	var group string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.HasPrefix(line, "0::/") {
			if group != "" {
				return fmt.Errorf("ambiguous browser cgroup membership")
			}
			group = strings.TrimPrefix(line, "0::")
		}
	}
	if group == "" || filepath.Clean(group) != group || filepath.Base(group) != unit {
		return fmt.Errorf("browser process is not in expected scope %s", unit)
	}
	for name, want := range map[string]string{"memory.max": "2147483648", "memory.swap.max": "0"} {
		value, err := readFile(filepath.Join("/sys/fs/cgroup", group, name))
		if err != nil {
			return fmt.Errorf("browser scope %s: %w", name, err)
		}
		if strings.TrimSpace(string(value)) != want {
			return fmt.Errorf("browser scope %s must be %s", name, want)
		}
	}
	return nil
}

func browserScopePrerequisites() error {
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return fmt.Errorf("browser profile requires a mounted cgroup v2 memory controller: %w", err)
	}
	return nil
}

// ExecBrowserScope is called only by the coordinator's internal startup argv.
// Verification precedes exec and the manager connection environment is removed.
func ExecBrowserScope(unit, binary string, args []string) error {
	if err := verifyBrowserScope(unit, os.ReadFile); err != nil {
		return err
	}
	if binary != "/usr/bin/bwrap" && binary != "/bin/bwrap" {
		return fmt.Errorf("browser helper requires system Bubblewrap")
	}
	return syscall.Exec(binary, append([]string{binary}, args...), []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent"})
}

// nativeSystemdScope deliberately does not inherit the browser resource policy.
type nativeSystemdScope struct{ launcher, control, executable, unit string }

type nativeScopeCapabilities struct {
	stat     func(string) (os.FileInfo, error)
	lookup   func(string) (string, error)
	launcher func(context.Context, string) error
	manager  func(context.Context, string) error
}

func nativeManagerAvailable(ctx context.Context, control string) error {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	probe := exec.CommandContext(probeCtx, control, "--user", "show", "--property=Version", "--value")
	probe.Env = browserManagerEnv()
	probe.Stdout, probe.Stderr = io.Discard, io.Discard
	process.Configure(probe)
	probe.Cancel = func() error { return process.StopVerified(probe) }
	probe.WaitDelay = time.Second
	return errors.Join(probe.Run(), process.StopVerified(probe))
}

func prepareNativeScope(ctx context.Context) (nativeScopeBoundary, error) {
	return prepareNativeScopeWith(ctx, nativeScopeCapabilities{stat: os.Stat, lookup: exec.LookPath, launcher: checkBrowserLauncher, manager: nativeManagerAvailable})
}

func prepareNativeScopeWith(ctx context.Context, capabilities nativeScopeCapabilities) (nativeScopeBoundary, error) {
	unavailable := func() (nativeScopeBoundary, error) { return nil, errNativeScopeUnavailable }
	if _, err := capabilities.stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return unavailable()
	}
	launcher, err := capabilities.lookup("systemd-run")
	if err != nil {
		return unavailable()
	}
	control, err := capabilities.lookup("systemctl")
	if err != nil {
		return unavailable()
	}
	if err = capabilities.launcher(ctx, launcher); err != nil {
		return unavailable()
	}
	if err = capabilities.manager(ctx, control); err != nil {
		return unavailable()
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, err
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	return &nativeSystemdScope{launcher: launcher, control: control, executable: executable, unit: "swarm-native-" + hex.EncodeToString(nonce[:]) + ".scope"}, nil
}

func nativeScopeArgs(unit, executable string) []string {
	return []string{"--user", "--scope", "--expand-environment=no", "--quiet", "--unit=" + unit, "--", executable, "internal-native-exec", unit}
}
func (n *nativeSystemdScope) command(ctx context.Context, binary, dir string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, n.launcher, nativeScopeArgs(n.unit, n.executable)...)
	cmd.Dir, cmd.Env = dir, browserManagerEnv()
	return cmd, nil
}
func (n *nativeSystemdScope) verify(identity storageuse.NativeIdentity) error {
	if identity.Unit != n.unit || !storageuse.ValidNativeIdentity(identity) {
		return storageuse.ErrUnsafe
	}
	// Cross-check the helper's report with current kernel state, including the
	// boot and mounted hierarchy. It must still be the same populated scope.
	read := func(path string) ([]byte, error) {
		if path == "/proc/self/cgroup" {
			return []byte("0::" + identity.Group), nil
		}
		return os.ReadFile(path)
	}
	observed, err := storageuse.CaptureNativeIdentity(n.unit, read, os.Stat)
	if err != nil {
		return err
	}
	if observed != identity {
		return storageuse.ErrUnsafe
	}
	return nil
}
func (n *nativeSystemdScope) stop(identity storageuse.NativeIdentity) error {
	if identity.Unit == n.unit && storageuse.NativeBoundaryEmpty(identity, os.ReadFile, os.Stat) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	run := func(args ...string) error {
		cmd := exec.CommandContext(ctx, n.control, append([]string{"--user"}, args...)...)
		cmd.Env = browserManagerEnv()
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		process.Configure(cmd)
		cmd.Cancel = func() error { return process.StopVerified(cmd) }
		cmd.WaitDelay = time.Second
		return errors.Join(cmd.Run(), process.StopVerified(cmd))
	}
	killErr := run("kill", "--kill-whom=all", "--signal=KILL", n.unit)
	stopErr := run("stop", n.unit)
	if identity.Unit == n.unit && storageuse.NativeBoundaryEmpty(identity, os.ReadFile, os.Stat) {
		return errors.Join(killErr, stopErr)
	}
	return errors.Join(storageuse.ErrUnsafe, killErr, stopErr)
}

// ExecNativeScope is the hidden pre-exec helper. Control descriptors never
// survive exec; only the supplied filtered payload environment is installed.
func ExecNativeScope(unit string) error {
	payload := os.NewFile(3, "native-payload")
	identity := os.NewFile(4, "native-identity")
	ack := os.NewFile(5, "native-ack")
	if payload == nil || identity == nil || ack == nil {
		return fmt.Errorf("missing native control pipes")
	}
	defer payload.Close()
	defer identity.Close()
	defer ack.Close()
	for _, pipe := range []*os.File{payload, identity, ack} {
		info, err := pipe.Stat()
		if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
			return fmt.Errorf("invalid native control pipe")
		}
	}
	var p nativePayload
	if err := readNativeFrame(payload, &p); err != nil {
		return err
	}
	payload.Close()
	if p.Binary == "" || p.Arg0 == "" || !filepath.IsAbs(p.Dir) || strings.ContainsRune(p.Binary, 0) {
		return fmt.Errorf("invalid native payload")
	}
	observed, err := storageuse.CaptureNativeIdentity(unit, os.ReadFile, os.Stat)
	if err != nil {
		return err
	}
	if err = writeNativeFrame(identity, observed); err != nil {
		return err
	}
	identity.Close()
	var approval [1]byte
	if _, err = io.ReadFull(ack, approval[:]); err != nil || approval[0] != 1 {
		return fmt.Errorf("native payload was not acknowledged")
	}
	ack.Close()
	if err = os.Chdir(p.Dir); err != nil {
		return err
	}
	return syscall.Exec(p.Binary, append([]string{p.Arg0}, p.Args...), p.Env)
}
