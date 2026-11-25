//go:build !windows

package cli

import "os"

func configureChatConsole(*os.File, *os.File) (func(), error) { return func() {}, nil }
