//go:build !linux

package sandbox

import (
	"context"
	"fmt"
	"os"
)

func nativeInputByte(context.Context, *os.File, []byte) (int, error) {
	return 0, fmt.Errorf("native interactive input is currently qualified on Linux")
}
