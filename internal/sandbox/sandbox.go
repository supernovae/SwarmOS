// Package sandbox runs untrusted reviewer commands with a filesystem and network boundary.
package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

// Options describes the reviewer boundary. ContainerIDFile is coordinator-private
// and must never be mounted into the container.
type Options struct {
	Runtime          string
	ContainerRuntime string
	ContainerImage   string
	ContainerIDFile  string
	Home             string
	DenyPaths        []string
	Env              []string
	AllowNetwork     bool
}

// CheckReadOnlyRuntime verifies that the selected isolation boundary is
// available before a caller reserves work.
func CheckReadOnlyRuntime(runtimeName, containerRuntime, container string) error {
	if runtimeName == "container" {
		if container == "" {
			return fmt.Errorf("review isolation container image is required")
		}
		if containerRuntime == "" {
			containerRuntime = "docker"
		}
		if _, err := exec.LookPath(containerRuntime); err != nil {
			return fmt.Errorf("review isolation unavailable: configured container runtime %q is required: %w", containerRuntime, err)
		}
		return nil
	}
	switch runtime.GOOS {
	case "darwin":
		if runtimeName != "" && runtimeName != "sandbox-exec" {
			return fmt.Errorf("configured review isolation runtime %q is unavailable on macOS", runtimeName)
		}
		if _, err := exec.LookPath("sandbox-exec"); err != nil {
			return fmt.Errorf("review isolation unavailable: sandbox-exec is required on macOS: %w", err)
		}
		return nil
	case "linux":
		if runtimeName != "" && runtimeName != "bwrap" {
			return fmt.Errorf("configured review isolation runtime %q is unavailable on Linux", runtimeName)
		}
		if _, err := exec.LookPath("bwrap"); err != nil {
			return fmt.Errorf("review isolation unavailable: bwrap is required on Linux or configure a container: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("review isolation unsupported on %s; configure a container runtime", runtime.GOOS)
	}
}

// ReadOnlyCommand never falls back to an unsafe host run.
func ReadOnlyCommand(dir, output, bin string, args []string, opts Options) (*exec.Cmd, error) {
	runtimeName, containerRuntime, container := opts.Runtime, opts.ContainerRuntime, opts.ContainerImage
	agentHome, allowNetwork, agentEnv := opts.Home, opts.AllowNetwork, opts.Env
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	absOutput, err := filepath.Abs(output)
	if err != nil {
		return nil, err
	}
	if runtimeName == "container" {
		if container == "" {
			return nil, fmt.Errorf("review isolation container image is required")
		}
		if opts.ContainerIDFile == "" {
			return nil, fmt.Errorf("container review requires a coordinator-owned container ID file")
		}
		runtimeBin := containerRuntime
		if runtimeBin == "" {
			runtimeBin = "docker"
		}
		tool, err := exec.LookPath(runtimeBin)
		if err != nil {
			return nil, fmt.Errorf("review isolation unavailable: configured container runtime %q is required: %w", runtimeBin, err)
		}
		wrapped := []string{"run", "--rm", "--cidfile", opts.ContainerIDFile, "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--mount", "type=bind,src=" + absDir + ",dst=/workspace,readonly", "--mount", "type=bind,src=" + absOutput + ",dst=" + absOutput, "--mount", "type=bind,src=" + agentHome + ",dst=" + agentHome, "-w", "/workspace"}
		if runtime.GOOS != "windows" {
			current, userErr := user.Current()
			if userErr != nil {
				return nil, fmt.Errorf("container reviewer resolves host uid:gid: %w", userErr)
			}
			if current.Uid == "" || current.Gid == "" {
				return nil, fmt.Errorf("container reviewer requires host uid:gid identity")
			}
			wrapped = append(wrapped, "--user", current.Uid+":"+current.Gid)
		}
		if !allowNetwork {
			wrapped = append(wrapped, "--network", "none")
		}
		for _, name := range append([]string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "TMPDIR", "TMP", "TEMP"}, agentEnv...) {
			wrapped = append(wrapped, "--env", name)
		}
		wrapped = append(wrapped, container, bin)
		wrapped = append(wrapped, args...)
		return exec.Command(tool, wrapped...), nil
	}
	denyPaths := make([]string, 0, len(opts.DenyPaths)+1)
	denyPaths = append(denyPaths, opts.DenyPaths...)
	if current, userErr := user.Current(); userErr == nil && current.HomeDir != "" {
		denyPaths = append(denyPaths, current.HomeDir)
	}
	for i, path := range denyPaths {
		if canonical, resolveErr := filepath.EvalSymlinks(path); resolveErr == nil {
			denyPaths[i] = canonical
		}
	}
	switch runtime.GOOS {
	case "darwin":
		if runtimeName != "" && runtimeName != "sandbox-exec" {
			return nil, fmt.Errorf("configured review isolation runtime %q is unavailable on macOS", runtimeName)
		}
		tool, err := exec.LookPath("sandbox-exec")
		if err != nil {
			return nil, fmt.Errorf("review isolation unavailable: sandbox-exec is required on macOS: %w", err)
		}
		profile := fmt.Sprintf(`(version 1) (deny default) (allow process*) (allow sysctl-read) (allow file-read*) (allow file-write* (subpath %q) (subpath %q))`, absOutput, agentHome)
		for _, denied := range denyPaths {
			if absDenied, denyErr := filepath.Abs(denied); denyErr == nil {
				profile += fmt.Sprintf(` (deny file-read* (subpath %q))`, absDenied)
			}
		}
		if allowNetwork {
			profile += ` (allow network*)`
		}
		wrapped := append([]string{"-p", profile, "--", bin}, args...)
		cmd := exec.Command(tool, wrapped...)
		cmd.Dir = absDir
		return cmd, nil
	case "linux":
		if runtimeName != "" && runtimeName != "bwrap" {
			return nil, fmt.Errorf("configured review isolation runtime %q is unavailable on Linux", runtimeName)
		}
		tool, err := exec.LookPath("bwrap")
		if err != nil {
			return nil, fmt.Errorf("review isolation unavailable: bwrap is required on Linux or configure a container: %w", err)
		}
		wrapped := []string{"--ro-bind", "/", "/", "--tmpfs", "/home", "--tmpfs", "/root", "--tmpfs", "/tmp", "--unshare-user", "--unshare-ipc", "--unshare-pid", "--unshare-uts"}
		if !allowNetwork {
			wrapped = append(wrapped, "--unshare-net")
		}
		for _, denied := range denyPaths {
			if absDenied, denyErr := filepath.Abs(denied); denyErr == nil && absDenied != "/" && filepath.Dir(absDenied) != "/" {
				wrapped = append(wrapped, "--tmpfs", filepath.Dir(absDenied))
			}
		}
		wrapped = append(wrapped, "--ro-bind", absDir, absDir, "--bind", absOutput, absOutput, "--bind", agentHome, agentHome, "--ro-bind", bin, bin, "--proc", "/proc", "--dev", "/dev", "--chdir", absDir, "--", bin)
		wrapped = append(wrapped, args...)
		cmd := exec.Command(tool, wrapped...)
		cmd.Dir = absDir
		return cmd, nil
	default:
		return nil, fmt.Errorf("review isolation unsupported on %s; configure a container runtime", runtime.GOOS)
	}
}

// RemoveContainer stops daemon-owned processes after a failed/cancelled client.
// Killing the Docker client process tree alone does not stop its container.
func RemoveContainer(ctx context.Context, runtimePath string, env []string, idFile string) error {
	data, err := os.ReadFile(idFile)
	if os.IsNotExist(err) {
		return nil // The client never reached container creation/start.
	}
	if err != nil {
		return err
	}
	id := strings.TrimSpace(string(data))
	if len(id) != 64 {
		return fmt.Errorf("invalid container ID in coordinator control file")
	}
	for _, ch := range id {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return fmt.Errorf("invalid container ID in coordinator control file")
		}
	}
	cmd := exec.CommandContext(ctx, runtimePath, "rm", "--force", id)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil && !strings.Contains(strings.ToLower(string(out)), "no such container") {
		return fmt.Errorf("remove reviewer container: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
