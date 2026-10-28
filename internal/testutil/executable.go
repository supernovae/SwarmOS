package testutil

import (
	"os"
	"syscall"
)

// WriteExecutable holds the fork lock through opening, writing, and closing the
// file so a parallel fork cannot inherit its writable descriptor and make exec
// fail with "text file busy" (golang/go#22315). Like os.WriteFile, it preserves
// the permissions of an existing file.
func WriteExecutable(path string, data []byte, perm os.FileMode) error {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	return os.WriteFile(path, data, perm)
}
