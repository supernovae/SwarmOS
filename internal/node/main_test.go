package node

import (
	"github.com/supernovae/swarmos/internal/storageuse"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "swarm-node-test-*")
	if err != nil {
		panic(err)
	}
	storageuse.UserCacheDir = func() (string, error) { return root, nil }
	code := m.Run()
	os.RemoveAll(root)
	os.Exit(code)
}
