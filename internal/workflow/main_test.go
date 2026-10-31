package workflow

import (
	"fmt"
	"github.com/supernovae/swarmos/internal/storageuse"
	"os"
	"testing"
)

// TestMain keeps the persistent Go build cache that host validation creates
// under XDG_CACHE_HOME out of the real user cache directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "swarm-test-cache-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("XDG_CACHE_HOME", dir)
	os.Setenv("HOME", dir)
	storageuse.UserCacheDir = func() (string, error) {
		base, err := userCacheDir()
		if err != nil {
			return os.UserCacheDir()
		}
		return base, nil
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
