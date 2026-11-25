//go:build windows

package cli

import (
	"context"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var chatReadConsoleInput = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReadConsoleInputW")

type chatInput struct {
	file *os.File
	keys chatConsoleKeys
}

func (in *chatInput) read(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if data := in.keys.next(); len(data) > 0 {
		return data, nil
	}
	handle := windows.Handle(in.file.Fd())
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		status, err := windows.WaitForSingleObject(handle, 50)
		if err != nil {
			return nil, err
		}
		if status == uint32(windows.WAIT_TIMEOUT) {
			continue
		}
		var record [20]byte
		var count uint32
		ok, _, callErr := chatReadConsoleInput.Call(uintptr(handle), uintptr(unsafe.Pointer(&record[0])), 1, uintptr(unsafe.Pointer(&count)))
		if ok == 0 {
			return nil, callErr
		}
		if count == 0 {
			continue
		}
		if data := in.keys.event(record); len(data) > 0 {
			return data, nil
		}
	}
}

// chat reads KEY_EVENT records directly, so leave VT input conversion disabled.
// Output still needs VT processing for prompt redraw and /clear.
func configureChatConsole(input, output *os.File) (func(), error) {
	inHandle, outHandle := windows.Handle(input.Fd()), windows.Handle(output.Fd())
	var inMode, outMode uint32
	if err := windows.GetConsoleMode(inHandle, &inMode); err != nil {
		return nil, err
	}
	if err := windows.GetConsoleMode(outHandle, &outMode); err != nil {
		return nil, err
	}
	if err := windows.SetConsoleMode(inHandle, inMode&^windows.ENABLE_VIRTUAL_TERMINAL_INPUT); err != nil {
		return nil, err
	}
	if err := windows.SetConsoleMode(outHandle, outMode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING|windows.ENABLE_PROCESSED_OUTPUT); err != nil {
		return nil, err
	}
	return func() { _ = windows.SetConsoleMode(outHandle, outMode) }, nil
}
