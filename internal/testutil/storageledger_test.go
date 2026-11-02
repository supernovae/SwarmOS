package testutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckStorageLedger(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name, path string
		err        error
		valid      bool
	}{
		{"inside", filepath.Join(root, "cache", "swarm-use"), nil, true},
		{"sibling prefix", root + "-other/swarm-use", nil, false},
		{"outside", filepath.Join(t.TempDir(), "swarm-use"), nil, false},
		{"resolution error", filepath.Join(root, "swarm-use"), errors.New("resolver failed"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := CheckStorageLedger(root, tc.path, tc.err); (err == nil) != tc.valid {
				t.Fatal(err)
			}
		})
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err == nil {
		if CheckStorageLedger(root, filepath.Join(root, "link", "swarm-use"), nil) == nil {
			t.Fatal("symlink escape accepted")
		}
	}
}
