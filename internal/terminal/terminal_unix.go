//go:build !windows

package terminal

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
)

// Default window size when the caller does not know one yet.
const (
	defaultCols = 80
	defaultRows = 24
)

// Start launches a shell on a new pseudo-terminal.
func Start(opts Options) (*Terminal, error) {
	name := opts.shell()

	// Resolve through PATH here so a missing shell fails with a clear error
	// rather than inside the child after the fork.
	path, err := exec.LookPath(name)
	if err != nil {
		return nil, fmt.Errorf("terminal: cannot run %q: %w", name, err)
	}

	cmd := exec.Command(path, opts.Args...)
	cmd.Dir = opts.Dir
	cmd.Env = opts.Env
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	// Setsid gives the shell its own session with the pty as controlling
	// terminal. Without it job control, Ctrl-C and SIGWINCH do not reach the
	// child correctly.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}

	size := &pty.Winsize{
		Cols: orDefault(opts.Cols, defaultCols),
		Rows: orDefault(opts.Rows, defaultRows),
	}
	f, err := pty.StartWithSize(cmd, size)
	if err != nil {
		return nil, fmt.Errorf("terminal: start %s: %w", path, err)
	}

	return &Terminal{pty: f, cmd: cmd, name: path, done: make(chan struct{})}, nil
}

// Resize changes the terminal window size and signals the shell (SIGWINCH), so
// full-screen programs such as vim or top redraw.
func (t *Terminal) Resize(cols, rows uint16) error {
	err := pty.Setsize(t.pty, &pty.Winsize{
		Cols: orDefault(cols, defaultCols),
		Rows: orDefault(rows, defaultRows),
	})
	if err != nil {
		return fmt.Errorf("terminal: resize: %w", err)
	}
	return nil
}

// Close terminates the shell and releases the pty.
//
// SIGHUP is sent first, which is what a real terminal hangup does and gives the
// shell a chance to clean up. Closing the master then tears down anything that
// ignored it.
func (t *Terminal) Close() error {
	var err error
	t.closeOnce.Do(func() {
		if t.cmd.Process != nil {
			_ = t.cmd.Process.Signal(syscall.SIGHUP)
		}
		err = t.pty.Close()
	})
	return err
}

// isTerminalClosed reports whether err means the shell is gone.
//
// Reading a pty master whose slave has closed yields EIO on Linux and EOF on
// macOS. Callers only care that there is nothing more to read.
func isTerminalClosed(err error) bool {
	return errors.Is(err, syscall.EIO) || errors.Is(err, fs.ErrClosed)
}

func orDefault(v, def uint16) uint16 {
	if v == 0 {
		return def
	}
	return v
}
