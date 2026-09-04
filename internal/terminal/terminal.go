// Package terminal runs a shell on a pseudo-terminal and exposes it as a byte
// stream.
//
// It knows nothing about sessions, relays or transports: it produces and
// consumes bytes, and something else decides where they go. That is what keeps
// terminal handling swappable and testable without a network.
//
// Unix uses a pty; Windows uses a pseudo-console (ConPTY). The two differ in
// every detail of how they are created, resized and torn down, so each
// platform supplies a ptyShell and everything above that line is shared.
package terminal

import (
	"errors"
	"io"
	"os"
	"runtime"
	"sync"
)

// ErrUnsupported is returned on platforms without pseudo-terminal support.
var ErrUnsupported = errors.New("terminal: pseudo-terminals are not supported on " + runtime.GOOS)

// Default window size when the caller does not know one yet.
const (
	defaultCols = 80
	defaultRows = 24
)

// Options configures Start.
type Options struct {
	// Shell is the program to run. Empty means the platform's default: $SHELL
	// then /bin/sh on Unix, %COMSPEC% then cmd.exe on Windows.
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

// env resolves the child environment.
func (o Options) env() []string {
	if o.Env == nil {
		return os.Environ()
	}
	return o.Env
}

// ptyShell is the platform's half of a Terminal: a shell running on a
// pseudo-terminal, with the lifecycle operations that a pseudo-terminal has to
// implement natively.
//
// Everything above this interface — translating a closed terminal to io.EOF,
// making Close and Wait one-shot, publishing Done — is identical on every
// platform, so it lives in Terminal and each platform implements only what
// genuinely differs.
type ptyShell interface {
	io.ReadWriter
	// Resize changes the window size and tells the shell about it, so
	// full-screen programs redraw.
	Resize(cols, rows uint16) error
	// Wait blocks until the shell exits and reports its exit status. A
	// non-zero status is not an error; only a failure to run or reap is.
	Wait() (int, error)
	// Close hangs the terminal up and releases it.
	Close() error
	// Pid reports the shell's process id.
	Pid() int
}

// Terminal is a shell running on a pseudo-terminal.
//
// Read returns what the shell prints; Write feeds it input. Both are safe to
// call concurrently with each other, which is the whole point: one goroutine
// pumps output while another delivers keystrokes.
type Terminal struct {
	pty  ptyShell
	name string

	closeOnce sync.Once
	closeErr  error
	waitOnce  sync.Once
	exitCode  int
	waitErr   error
	done      chan struct{}
}

// newTerminal wraps a platform pseudo-terminal running name.
func newTerminal(pty ptyShell, name string) *Terminal {
	return &Terminal{pty: pty, name: name, done: make(chan struct{})}
}

// Command reports the program running on the terminal.
func (t *Terminal) Command() string { return t.name }

// Pid reports the shell's process id.
func (t *Terminal) Pid() int { return t.pty.Pid() }

// Read returns terminal output.
//
// When the shell exits, reading the terminal fails rather than returning a
// clean EOF: Linux surfaces that as EIO, Windows as a broken pipe. Both mean
// the same thing to a caller, so they are translated to io.EOF.
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

// Resize changes the terminal window size and tells the shell, so full-screen
// programs such as vim or top redraw.
func (t *Terminal) Resize(cols, rows uint16) error { return t.pty.Resize(cols, rows) }

// Done is closed once the shell has exited and Wait has observed it.
func (t *Terminal) Done() <-chan struct{} { return t.done }

// Wait blocks until the shell exits and returns its exit status. It is safe to
// call from multiple goroutines; every caller sees the same result.
//
// A non-zero exit status is not an error: a user typing "exit 1" is ordinary.
// Only a failure to run or reap the process is.
func (t *Terminal) Wait() (int, error) {
	t.waitOnce.Do(func() {
		t.exitCode, t.waitErr = t.pty.Wait()
		close(t.done)
	})
	<-t.done
	return t.exitCode, t.waitErr
}

// Close terminates the shell and releases the terminal. It is idempotent, and
// every caller sees the same result.
func (t *Terminal) Close() error {
	t.closeOnce.Do(func() { t.closeErr = t.pty.Close() })
	return t.closeErr
}

// orDefault substitutes def for a zero dimension.
func orDefault(v, def uint16) uint16 {
	if v == 0 {
		return def
	}
	return v
}
