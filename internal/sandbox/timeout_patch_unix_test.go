//go:build unix

package sandbox

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestTimeoutPatchRejectsSpecialFilesWithoutBlocking(t *testing.T) {
	for _, kind := range []string{"fifo", "socket"} {
		t.Run(kind, func(t *testing.T) {
			work := t.TempDir()
			path := filepath.Join(work, "special")
			if kind == "fifo" {
				if err := unix.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				listener, err := net.Listen("unix", path)
				if err != nil {
					t.Skip(err)
				}
				defer listener.Close()
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			start := time.Now()
			if _, err := timeoutFiles(ctx, work); err == nil {
				t.Fatal("accepted special file")
			}
			if time.Since(start) > time.Second {
				t.Fatal("blocked opening special file")
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal(err)
			}
		})
	}
}
