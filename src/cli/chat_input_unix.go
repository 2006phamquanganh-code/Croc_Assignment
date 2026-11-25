//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly || solaris || aix

package cli

import (
	"context"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

type chatInput struct{ file *os.File }

func (in *chatInput) read(ctx context.Context) ([]byte, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		poll := []unix.PollFd{{Fd: int32(in.file.Fd()), Events: unix.POLLIN}}
		n, err := unix.Poll(poll, 50)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return nil, err
		}
		if n == 0 {
			continue
		}
		data := make([]byte, 256)
		n, err = in.file.Read(data)
		if n > 0 {
			return data[:n], nil
		}
		if err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
}
