//go:build !linux && !darwin

package storage

import "errors"

func FilesystemSpace(string) (uint64, uint64, error) {
	return 0, 0, errors.New("filesystem space unavailable")
}
