// Package terminal runs a shell on a pseudo-terminal and exposes it as a byte
// stream.
//
// It knows nothing about sessions, relays or transports: it produces and
// consumes bytes, and something else decides where they go. That is what keeps
// terminal handling swappable and testable without a network.
package terminal

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"
)

// ErrUnsupported is returned on platforms without pseudo-terminal support.
var ErrUnsupported = errors.New("terminal: pseudo-terminals are not supported on " + runtime.GOOS)

// DefaultShell is used when neither Options.Shell nor $SHELL names one.
const DefaultShell = "/bin/sh"

// Options configures Start.
type Options struct {
	// Shell is the program to run. Empty means $SHELL, then DefaultShell.
	//
	// This must never be taken from a network request. The shell is chosen by
	// the person running the CLI on their own machine; a relay-supplied value
	// would be remote code execution.
	Shell string
	// Args are passed to the shell. Empty means run it interactively.
	Args []string
	// Env is the child environment. Nil inherits the parent's.
	Env []string
	// Dir is the working directory. Empty inherits the parent's.
	Dir string
	// Cols and Rows set the initial window size. Zero uses 80x24.
	Cols, Rows uint16
}

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

// Terminal is a shell running on a pseudo-terminal.
//
// Read returns what the shell prints; Write feeds it input. Both are safe to
// call concurrently with each other, which is the whole point: one goroutine
// pumps output while another delivers keystrokes.
type Terminal struct {
	pty  *os.File
	cmd  *exec.Cmd
	name string

	closeOnce sync.Once
	waitOnce  sync.Once
	exitCode  int
	waitErr   error
	done      chan struct{}
}

// Command reports the program running on the terminal.
func (t *Terminal) Command() string { return t.name }

// Pid reports the shell's process id.
func (t *Terminal) Pid() int {
	if t.cmd == nil || t.cmd.Process == nil {
		return 0
	}
	return t.cmd.Process.Pid
}

// Read returns terminal output.
//
// When the shell exits, the pty master read fails; on Linux that surfaces as
// EIO rather than EOF. Both mean the same thing to a caller, so EIO is
// translated to io.EOF.
func (t *Terminal) Read(p []byte) (int, error) {
	n, err := t.pty.Read(p)
	if err != nil && isTerminalClosed(err) {
		return n, io.EOF
	}
	return n, err
}

// Write delivers input to the shell.
func (t *Terminal) Write(p []byte) (int, error) {
	n, err := t.pty.Write(p)
	if err != nil && isTerminalClosed(err) {
		return n, io.EOF
	}
	return n, err
}

// Done is closed once the shell has exited and Wait has observed it.
func (t *Terminal) Done() <-chan struct{} { return t.done }

// Wait blocks until the shell exits and returns its exit status. It is safe to
// call from multiple goroutines; every caller sees the same result.
//
// A non-zero exit status is not an error: a user typing "exit 1" is ordinary.
// Only a failure to run or reap the process is.
func (t *Terminal) Wait() (int, error) {
	t.waitOnce.Do(func() {
		err := t.cmd.Wait()
		var ee *exec.ExitError
		switch {
		case err == nil:
			t.exitCode = 0
		case errors.As(err, &ee):
			t.exitCode = ee.ExitCode()
		default:
			t.exitCode = -1
			t.waitErr = err
		}
		close(t.done)
	})
	<-t.done
	return t.exitCode, t.waitErr
}
