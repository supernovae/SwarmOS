//go:build !linux

package sandbox

import (
	"context"
	"errors"
	"os/exec"
)

func restoreCommand(context.Context, string, string, string, []string) (*exec.Cmd, error) {
	return nil, errors.New("restore boundary unavailable: Linux Bubblewrap required")
}

func restoreCloseDescriptors() error { return errors.New("restore helper requires Linux") }
