package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/process"
	"github.com/supernovae/swarmos/internal/storageuse"
)

const browserMemoryMax = "2147483648"

func browserManagerEnv() []string {
	// These values are connection data only. No host environment is inherited.
	runtimeDir := fmt.Sprintf("/run/user/%d", os.Getuid())
	return []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "XDG_RUNTIME_DIR=" + runtimeDir,
		"DBUS_SESSION_BUS_ADDRESS=unix:path=" + runtimeDir + "/bus"}
}

func browserScopeArgs(unit, executable, binary string, flags []string) []string {
	args := []string{"--user", "--scope", "--expand-environment=no", "--quiet", "--unit=" + unit, "--property=MemoryMax=" + browserMemoryMax, "--property=MemorySwapMax=0", "--", executable, "internal-browser-exec", unit, binary}
	return append(args, flags...)
}

// Older systemd-run versions expand payload variables and cannot safely launch tools.
func checkBrowserLauncher(ctx context.Context, launcher string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, launcher, "--help")
	cmd.Env = browserManagerEnv()
	process.Configure(cmd)
	cmd.Cancel = func() error { return process.StopVerified(cmd) }
	cmd.WaitDelay = time.Second
	var out process.Output
	cmd.Stdout, cmd.Stderr = &out, &out
	err := errors.Join(cmd.Run(), process.StopVerified(cmd))
	if err != nil || !strings.Contains(out.String(), "--expand-environment=") {
		return fmt.Errorf("browser profile requires systemd 254+ with systemd-run --expand-environment=no (capability probe: %v)", err)
	}
	return nil
}

func browserCommand(ctx context.Context, binary string, flags []string) (*exec.Cmd, string, error) {
	if err := browserScopePrerequisites(); err != nil {
		return nil, "", err
	}
	launcher, err := exec.LookPath("systemd-run")
	if err != nil {
		return nil, "", fmt.Errorf("browser profile requires systemd-run: %w", err)
	}
	if err := checkBrowserLauncher(ctx, launcher); err != nil {
		return nil, "", err
	}
	if _, err = exec.LookPath("systemctl"); err != nil {
		return nil, "", fmt.Errorf("browser profile requires systemctl: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, "", err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, "", err
	}
	var id [16]byte
	if _, err = rand.Read(id[:]); err != nil {
		return nil, "", err
	}
	unit := "swarm-browser-" + hex.EncodeToString(id[:]) + ".scope"
	cmd := exec.CommandContext(ctx, launcher, browserScopeArgs(unit, executable, binary, flags)...)
	cmd.Env = browserManagerEnv()
	process.Configure(cmd)
	cmd.Cancel = func() error { return process.StopVerified(cmd) }
	cmd.WaitDelay = time.Second
	return cmd, unit, nil
}

// stopBrowserScope uses an independent bounded context: the command context
// may already be cancelled. A missing unit is empty; every other uncertain
// state is an error so callers retain their cache readers.
func stopBrowserScope(unit string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	control, err := exec.LookPath("systemctl")
	if err != nil {
		return err
	}
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, control, append([]string{"--user"}, args...)...)
		cmd.Env = browserManagerEnv()
		process.Configure(cmd)
		cmd.Cancel = func() error { return process.StopVerified(cmd) }
		cmd.WaitDelay = time.Second
		var out process.Output
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()
		err = errors.Join(err, process.StopVerified(cmd))
		return out.String(), err
	}
	// Kill every scope member before StopUnit; do not wait for the manager
	// default termination grace period on timeout or cancellation.
	_, killErr := run("kill", "--kill-whom=all", "--signal=KILL", unit)
	_, stopErr := run("stop", unit)
	stopErr = errors.Join(killErr, stopErr)
	state, showErr := run("show", unit, "--property=LoadState", "--property=ActiveState", "--property=ControlGroup")

	if showErr != nil && !strings.Contains("\n"+state, "\nLoadState=not-found\n") {
		return fmt.Errorf("verify browser scope teardown: %w", errors.Join(stopErr, showErr))
	}
	if err := verifyBrowserScopeStopped(unit, state, os.ReadFile); err != nil {
		return errors.Join(stopErr, err)
	}
	return nil
}

func verifyBrowserScopeStopped(unit, state string, readFile func(string) ([]byte, error)) error {
	values := map[string]string{}
	for _, line := range strings.Split(state, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			values[k] = v
		}
	}
	if values["LoadState"] == "not-found" {
		return nil
	}
	if values["LoadState"] != "loaded" {
		return fmt.Errorf("browser scope load state unverified")
	}

	group := values["ControlGroup"]
	if group != "" {
		if _, err := readFile("/sys/fs/cgroup/cgroup.controllers"); err != nil {
			return fmt.Errorf("verify browser cleanup cgroup v2 mount: %w", err)
		}
		if !strings.HasPrefix(group, "/") || filepath.Clean(group) != group || filepath.Base(group) != unit {
			return fmt.Errorf("unexpected browser cleanup cgroup")
		}
		data, err := readFile(filepath.Join("/sys/fs/cgroup", group, "cgroup.events"))
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("verify browser scope empty: %w", err)
		}
		if err == nil && !strings.Contains("\n"+string(data), "\npopulated 0\n") {
			return fmt.Errorf("browser scope still populated")
		}
	}
	if values["ActiveState"] != "inactive" && values["ActiveState"] != "failed" {
		return fmt.Errorf("browser scope teardown incomplete: %s", values["ActiveState"])
	}
	return nil
}

// browserShell resolves only the existing read-only node-browsers mounts.
func browserShell(deps Dependencies, work string) (string, error) {
	for _, bind := range deps.toolchains() {
		if bind.Ecosystem != "node-browsers" || bind.EnvKey != "PLAYWRIGHT_BROWSERS_PATH" {
			continue
		}
		root, err := filepath.EvalSymlinks(bind.Mount)
		if err != nil {
			continue
		}
		if !filepath.IsAbs(root) || root == "/" || (work != "" && (isSubpath(root, work) || isSubpath(work, root))) {
			continue
		}
		matches, err := filepath.Glob(filepath.Join(root, "chromium_headless_shell-*", "chrome-headless-shell-*", "chrome-headless-shell"))
		if err != nil {
			return "", err
		}
		for _, match := range matches {
			resolved, err := filepath.EvalSymlinks(match)
			if err != nil || !isSubpath(resolved, root) {
				continue
			}
			info, err := os.Stat(resolved)
			if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
				return resolved, nil
			}
		}
	}
	return "", fmt.Errorf("browser profile requires cached Playwright Chromium headless shell; provision Chromium on the host with playwright install chromium")
}

// CheckBrowserProfile uses the production launcher and pre-exec verifier. An
// availability result never authorizes a later invocation to omit verification.
func CheckBrowserProfile(ctx context.Context) (resultErr error) {
	ctx, own, err := storageuse.Ensure(ctx, "host")
	if err != nil {
		return err
	}
	if own != nil {
		defer own.Complete(&resultErr)
	}

	if _, err := exec.LookPath("bwrap"); err != nil {
		return fmt.Errorf("browser profile requires Bubblewrap: %w", err)
	}
	if err := browserScopePrerequisites(); err != nil {
		return err
	}
	shell, err := browserShell(Dependencies{}, "")
	if err != nil {
		return err
	}
	if err := checkToolExecutor(ctx, true); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	work, err := storageuse.FromContext(ctx).TempDir(ctx, "", "swarm-browser-probe-*", "sandbox-scratch")
	if err != nil {
		return err
	}
	defer storageuse.FromContext(ctx).CleanupTemp(work, &resultErr)
	var out process.Output
	err = (ToolExecutor{Work: work, BrowserProfile: true}).Run(ctx, `"$1" --no-sandbox --headless --disable-gpu --user-data-dir=/tmp/browser-probe --dump-dom about:blank`, &out, &out, shell)
	if err != nil {
		return fmt.Errorf("browser profile Chromium launch: %w: %s", err, out.String())
	}
	return nil
}
