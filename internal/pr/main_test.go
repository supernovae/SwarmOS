package pr

import (
	"fmt"
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
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
