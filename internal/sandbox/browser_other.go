//go:build !linux

package sandbox

import (
	"context"
	"fmt"
)

func browserScopePrerequisites() error {
	return fmt.Errorf("browser profile requires Linux with systemd and cgroup v2")
}
func ExecBrowserScope(unit, binary string, args []string) error { return browserScopePrerequisites() }

func prepareNativeScope(context.Context) (nativeScopeBoundary, error) {
	return nil, errNativeScopeUnavailable
}
func ExecNativeScope(string) error { return errNativeScopeUnavailable }
