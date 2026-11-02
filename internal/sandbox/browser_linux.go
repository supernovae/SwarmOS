//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
