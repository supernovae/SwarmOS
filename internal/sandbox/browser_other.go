//go:build !linux

package sandbox

import "fmt"

func browserScopePrerequisites() error {
	return fmt.Errorf("browser profile requires Linux with systemd and cgroup v2")
}
func ExecBrowserScope(unit, binary string, args []string) error { return browserScopePrerequisites() }
