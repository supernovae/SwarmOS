//go:build linux || darwin

package storage

import (
	"os"
	"syscall"
)

type fileIdentity struct{ device, inode uint64 }

func physicalStat(info os.FileInfo) (fileIdentity, int64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Blocks < 0 || st.Blocks > int64(^uint64(0)>>1)/512 {
		return fileIdentity{}, 0, false
	}
	return fileIdentity{uint64(st.Dev), uint64(st.Ino)}, st.Blocks * 512, true
}
