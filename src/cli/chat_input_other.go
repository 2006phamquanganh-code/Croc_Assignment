//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly && !solaris && !aix && !windows

package cli

import (
	"context"
	"errors"
	"os"
)

type chatInput struct{ file *os.File }

func (*chatInput) read(context.Context) ([]byte, error) {
	return nil, errors.New("interactive chat is unsupported on this platform")
}
