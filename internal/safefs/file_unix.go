//go:build unix

package safefs

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"syscall"
)

const nonblock = syscall.O_NONBLOCK

func singleLink(_ *os.File, info os.FileInfo) error {
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Nlink != 1 {
		return fmt.Errorf("refuse multiply-linked file %q", info.Name())
	}
	return nil
}

// os.Root resolves contained symlinks even with O_NOFOLLOW. Open the final
// basename directly relative to its already-validated directory descriptor.
func openExisting(parent *os.Root, name string, flags int) (*os.File, error) {
	dir, err := parent.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	fd, err := unix.Openat(int(dir.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), name), nil
}
