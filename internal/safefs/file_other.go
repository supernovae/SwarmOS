//go:build !unix && !windows

package safefs

import (
	"fmt"
	"os"
)

const nonblock = 0

func singleLink(_ *os.File, _ os.FileInfo) error {
	return fmt.Errorf("secure regular-file verification is unavailable on this platform")
}

func openExisting(_ *os.Root, _ string, _ int) (*os.File, error) {
	return nil, fmt.Errorf("secure file opening unavailable on this platform")
}
