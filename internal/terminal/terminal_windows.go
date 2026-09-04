//go:build windows

package terminal

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"

	"golang.org/x/sys/windows"
)

// DefaultShell is used when neither Options.Shell nor %COMSPEC% names one.
const DefaultShell = "cmd.exe"

// shell resolves which program to run.
//
// $SHELL is deliberately ignored here, unlike on Unix. On Windows it is
// normally set by an MSYS environment — Git Bash sets SHELL=/usr/bin/bash —
// and that is a path CreateProcess cannot run. %COMSPEC% is Windows' own
// answer to the same question, and it is always set.
//
// PowerShell is a flag away: `openconsole -shell powershell.exe`, or pwsh.exe
// for PowerShell 7. Guessing it from the environment is not possible with any
// reliability, because PSModulePath is a machine-wide variable that cmd.exe
// inherits too.
func (o Options) shell() string {
	if o.Shell != "" {
		return o.Shell
	}
	if s := os.Getenv("COMSPEC"); s != "" {
		return s
	}
	return DefaultShell
}

// Start launches a shell on a new pseudo-console.
func Start(opts Options) (*Terminal, error) {
	name := opts.shell()

	// Resolve through PATH here so a missing shell fails with a clear error
	// rather than inside CreateProcess. LookPath also applies PATHEXT, so
	// "powershell" finds powershell.exe.
	path, err := exec.LookPath(name)
	if err != nil {
		return nil, fmt.Errorf("terminal: cannot run %q: %w", name, err)
	}
	if err := conptySupported(); err != nil {
		return nil, err
	}

	pty, err := startConPTY(path, opts)
	if err != nil {
		return nil, err
	}
	return newTerminal(pty, path), nil
}

// isTerminalClosed reports whether err means the shell is gone.
//
// The pseudo-console's output is an anonymous pipe, so the far end going away
// surfaces as a broken pipe. A read that loses a race with Close sees the file
// already closed instead. Callers only care that there is nothing more to read.
func isTerminalClosed(err error) bool {
	return errors.Is(err, windows.ERROR_BROKEN_PIPE) ||
		errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED) ||
		errors.Is(err, windows.ERROR_HANDLE_EOF) ||
		errors.Is(err, fs.ErrClosed)
}
