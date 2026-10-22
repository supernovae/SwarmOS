package sandbox

import (
	"context"
	"io"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

func nativeInputByte(ctx context.Context, input *os.File, b []byte) (int, error) {
	defer runtime.KeepAlive(input)
	fd := int(input.Fd())
	if fd < 0 {
		return 0, os.ErrClosed
	}
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(poll, 50)
		if err == unix.EINTR || n == 0 && err == nil {
			continue
		}
		if err != nil {
			return 0, err
		}
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		n, err = unix.Read(fd, b)
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		if n == 0 && err == nil {
			err = io.EOF
		}
		return n, err
	}
}
