//go:build !linux && !darwin

package storage

import "os"

type fileIdentity struct{ device, inode uint64 }

func physicalStat(os.FileInfo) (fileIdentity, int64, bool) { return fileIdentity{}, 0, false }

func singleLink(os.FileInfo) bool { return false }
