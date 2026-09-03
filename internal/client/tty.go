package client

import (
	"errors"
	"os"

	"golang.org/x/term"
)

// ErrNotATerminal is returned when the CLI is not attached to a TTY.
var ErrNotATerminal = errors.New("openconsole needs an interactive terminal (stdin and stdout must be a TTY)")

// rawTerminal puts the local terminal in raw mode and returns a restore
// function.
//
// Raw mode is what makes the shared shell feel like a shell: without it the
// local line discipline would buffer input until Enter, swallow Ctrl-C, and
// echo characters twice. Restore must run on every exit path, including a
// panic, or the user is left with a terminal that does not echo.
func rawTerminal(f *os.File) (restore func(), err error) {
	fd := int(f.Fd())
	if !term.IsTerminal(fd) {
		return nil, ErrNotATerminal
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	var restored bool
	return func() {
		if restored {
			return
		}
		restored = true
		_ = term.Restore(fd, state)
	}, nil
}

// isTerminal reports whether f is attached to a TTY.
func isTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// terminalSize reports the size of the local terminal, falling back to a
// conventional 80x24 when it cannot be determined.
func terminalSize(f *os.File) (cols, rows uint16) {
	w, h, err := term.GetSize(int(f.Fd()))
	if err != nil || w <= 0 || h <= 0 {
		return 80, 24
	}
	return uint16(w), uint16(h)
}
