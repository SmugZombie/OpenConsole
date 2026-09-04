//go:build windows

package client

import (
	"os"
	"sync"

	"golang.org/x/sys/windows"
)

// enableVirtualTerminal turns on ANSI escape handling for a console, and
// returns a function that puts the mode back.
//
// A Windows console does not interpret escape sequences unless it is asked to.
// Without this, a shared shell's colours, cursor movement and screen clears
// arrive as visible gibberish — the bytes are right, the console is simply not
// reading them as anything. Raw mode covers the input side; this is the output
// side, and it is a different handle with a different mode.
//
// Failure is not fatal. A console too old to have the mode still shows the
// text, and a redirected stdout has no console mode at all; neither is worth
// refusing to run over.
func enableVirtualTerminal(f *os.File) (restore func()) {
	handle := windows.Handle(f.Fd())

	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return func() {}
	}
	// ENABLE_PROCESSED_OUTPUT is a prerequisite: escape sequences are part of
	// the processing that flag turns on.
	want := mode | windows.ENABLE_PROCESSED_OUTPUT | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING
	if want == mode {
		return func() {}
	}
	if err := windows.SetConsoleMode(handle, want); err != nil {
		return func() {}
	}

	var once sync.Once
	return func() {
		once.Do(func() { _ = windows.SetConsoleMode(handle, mode) })
	}
}
