//go:build linux || darwin

package storage

import "syscall"

// FilesystemSpace performs a single statfs observation without enumerating files.
func FilesystemSpace(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err = syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	size := uint64(st.Bsize)
	if size == 0 || uint64(st.Blocks) > ^uint64(0)/size || uint64(st.Bavail) > ^uint64(0)/size {
		return 0, 0, syscall.EOVERFLOW
	}
	return uint64(st.Bavail) * size, uint64(st.Blocks) * size, nil
}
