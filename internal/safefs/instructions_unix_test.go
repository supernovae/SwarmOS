//go:build unix

package safefs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestResolveInstructionRefusesFIFO(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("pipe", filepath.Join(dir, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	root, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := root.ResolveInstructions(); err == nil || !strings.Contains(err.Error(), "special file") {
		t.Fatalf("FIFO target = %v", err)
	}
}
