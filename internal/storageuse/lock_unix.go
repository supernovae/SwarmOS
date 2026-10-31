//go:build !windows

package storageuse

import (
	"errors"
	"syscall"
)

func lockBusy(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}
