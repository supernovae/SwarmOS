//go:build windows

package storageuse

import (
	"errors"
	"golang.org/x/sys/windows"
)

func lockBusy(err error) bool { return errors.Is(err, windows.ERROR_LOCK_VIOLATION) }
