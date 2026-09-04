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

// DefaultShell is used when neither Options.Shell nor $SHELL names one.
const DefaultShell = "/bin/sh"

// shell resolves which program to run.
func (o Options) shell() string {
	if o.Shell != "" {
		return o.Shell
	}
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	return DefaultShell
}

// unixPTY is a shell on a Unix pseudo-terminal.
type unixPTY struct {
	f   *os.File
	cmd *exec.Cmd
}

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
	cmd.Env = opts.env()
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
		// Every Unix we ship for has ptys; a platform without them should say
		// so in this package's own terms rather than the pty library's.
		if errors.Is(err, pty.ErrUnsupported) {
			return nil, ErrUnsupported
		}
		return nil, fmt.Errorf("terminal: start %s: %w", path, err)
	}

	return newTerminal(&unixPTY{f: f, cmd: cmd}, path), nil
}

// Pid reports the shell's process id.
func (u *unixPTY) Pid() int {
	if u.cmd.Process == nil {
		return 0
	}
	return u.cmd.Process.Pid
}

func (u *unixPTY) Read(p []byte) (int, error)  { return u.f.Read(p) }
func (u *unixPTY) Write(p []byte) (int, error) { return u.f.Write(p) }

// Resize changes the window size and signals the shell (SIGWINCH).
func (u *unixPTY) Resize(cols, rows uint16) error {
	err := pty.Setsize(u.f, &pty.Winsize{
		Cols: orDefault(cols, defaultCols),
		Rows: orDefault(rows, defaultRows),
	})
	if err != nil {
		return fmt.Errorf("terminal: resize: %w", err)
	}
	return nil
}

// Wait reaps the shell and reports its exit status.
func (u *unixPTY) Wait() (int, error) {
	err := u.cmd.Wait()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &ee):
		return ee.ExitCode(), nil
	default:
		return -1, err
	}
}

// Close terminates the shell and releases the pty.
//
// SIGHUP is sent first, which is what a real terminal hangup does and gives the
// shell a chance to clean up. Closing the master then tears down anything that
// ignored it.
func (u *unixPTY) Close() error {
	if u.cmd.Process != nil {
		_ = u.cmd.Process.Signal(syscall.SIGHUP)
	}
	return u.f.Close()
}

// isTerminalClosed reports whether err means the shell is gone.
//
// Reading a pty master whose slave has closed yields EIO on Linux and EOF on
// macOS. Callers only care that there is nothing more to read.
func isTerminalClosed(err error) bool {
	return errors.Is(err, syscall.EIO) || errors.Is(err, fs.ErrClosed)
}
