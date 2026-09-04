//go:build windows

package terminal

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// hangupGrace is how long a shell gets to notice that its console has gone
// before it is terminated outright.
//
// Closing a pseudo-console is the Windows equivalent of a hangup, and a shell
// normally exits the moment it happens. If one does not, openconsole must
// still be able to exit: a person who has closed their session should get their
// prompt back, not a process that never returns.
const hangupGrace = 5 * time.Second

// conPTY is a shell running on a Windows pseudo-console.
//
// The pseudo-console occupies the place a pty master does on Unix, but the
// bytes travel over two ordinary anonymous pipes: one this process writes
// (the shell's input) and one it reads (the shell's output).
type conPTY struct {
	console windows.Handle // the pseudo-console (HPCON)
	in      *os.File       // our end of the shell's input
	out     *os.File       // our end of the shell's output
	proc    windows.Handle
	pid     int

	mu     sync.Mutex
	closed bool
	// reaped guards the process handle: once the shell has been waited for,
	// the handle is closed and must not be used again.
	reaped   bool
	exitCode int
	waitErr  error
	// exited is closed once the shell has exited and its status is recorded.
	exited chan struct{}
}

// startConPTY creates a pseudo-console and starts path attached to it.
func startConPTY(path string, opts Options) (*conPTY, error) {
	// Two anonymous pipes. The pseudo-console keeps one end of each; this
	// process keeps the other.
	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return nil, fmt.Errorf("terminal: input pipe: %w", err)
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		closeHandles(inRead, inWrite)
		return nil, fmt.Errorf("terminal: output pipe: %w", err)
	}

	size := windows.Coord{
		X: int16(orDefault(opts.Cols, defaultCols)),
		Y: int16(orDefault(opts.Rows, defaultRows)),
	}
	var console windows.Handle
	err := windows.CreatePseudoConsole(size, inRead, outWrite, 0, &console)
	// conhost duplicates the two handles it was given, so this process must
	// let go of them either way. Holding on would keep the pipes open after
	// conhost had finished with them, and the reader would never see the end
	// of the output.
	closeHandles(inRead, outWrite)
	if err != nil {
		closeHandles(inWrite, outRead)
		return nil, fmt.Errorf("terminal: creating a pseudo-console: %w", err)
	}

	pid, proc, err := startOnConsole(console, path, opts)
	if err != nil {
		windows.ClosePseudoConsole(console)
		closeHandles(inWrite, outRead)
		return nil, err
	}

	c := &conPTY{
		console: console,
		in:      os.NewFile(uintptr(inWrite), "conpty-input"),
		out:     os.NewFile(uintptr(outRead), "conpty-output"),
		proc:    proc,
		pid:     pid,
		exited:  make(chan struct{}),
	}
	go c.reap()
	return c, nil
}

// startOnConsole launches path attached to an existing pseudo-console.
func startOnConsole(console windows.Handle, path string, opts Options) (int, windows.Handle, error) {
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return 0, 0, fmt.Errorf("terminal: process attribute list: %w", err)
	}
	defer attrs.Delete()

	if err := setPseudoConsole(attrs.List(), console); err != nil {
		return 0, 0, fmt.Errorf("terminal: attaching the pseudo-console: %w", err)
	}

	// Cb must describe the extended struct, not the plain StartupInfo it
	// starts with, or CreateProcess ignores the attribute list.
	si := &windows.StartupInfoEx{
		StartupInfo:             windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{}))},
		ProcThreadAttributeList: attrs.List(),
	}
	// STARTF_USESTDHANDLES with all three handles left null. This looks like a
	// no-op and is not: without it a child takes its standard handles from
	// this process, and a pseudo-console attachment does not stop it. The
	// shell then writes to whatever openconsole's own stdout is — under `go
	// test`, the test runner's pipe — while dutifully setting the title on a
	// console nobody can read. Saying "use the handles I am giving you", and
	// giving none, is what makes Windows hand it the console's own.
	si.StartupInfo.Flags |= windows.STARTF_USESTDHANDLES

	exe, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, fmt.Errorf("terminal: cannot run %q: %w", path, err)
	}
	cmdline, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{path}, opts.Args...)))
	if err != nil {
		return 0, 0, fmt.Errorf("terminal: command line: %w", err)
	}
	env, err := envBlock(opts.env())
	if err != nil {
		return 0, 0, err
	}
	var dir *uint16
	if opts.Dir != "" {
		if dir, err = windows.UTF16PtrFromString(opts.Dir); err != nil {
			return 0, 0, fmt.Errorf("terminal: working directory: %w", err)
		}
	}

	// Handles are not inherited: the shell gets its console from the
	// attribute list, and nothing else of this process's is its business.
	var pi windows.ProcessInformation
	err = windows.CreateProcess(exe, cmdline, nil, nil, false,
		windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_UNICODE_ENVIRONMENT,
		env, dir, &si.StartupInfo, &pi)
	if err != nil {
		return 0, 0, fmt.Errorf("terminal: start %s: %w", path, err)
	}
	// Only the process handle is worth keeping; the thread handle is not used.
	windows.CloseHandle(pi.Thread)
	return int(pi.ProcessId), pi.Process, nil
}

func (c *conPTY) Read(p []byte) (int, error)  { return c.out.Read(p) }
func (c *conPTY) Write(p []byte) (int, error) { return c.in.Write(p) }
func (c *conPTY) Pid() int                    { return c.pid }

// Resize changes the window size. The shell is told by the console itself, so
// there is no signal to send.
func (c *conPTY) Resize(cols, rows uint16) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// A resize that arrives as the session ends is not a failure worth
	// reporting: the window whose size changed no longer has a shell in it.
	if c.closed {
		return nil
	}
	size := windows.Coord{X: int16(orDefault(cols, defaultCols)), Y: int16(orDefault(rows, defaultRows))}
	if err := windows.ResizePseudoConsole(c.console, size); err != nil {
		return fmt.Errorf("terminal: resize: %w", err)
	}
	return nil
}

// Wait blocks until the shell exits and reports its status.
func (c *conPTY) Wait() (int, error) {
	<-c.exited
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exitCode, c.waitErr
}

// Close hangs the terminal up and releases it.
func (c *conPTY) Close() error {
	// Keep the output moving while the console is taken down. A pseudo-console
	// with nobody reading it can block its own shutdown waiting to flush, and
	// Close is reached both after a read loop has finished and on paths where
	// one never started — a relay that could not be dialled, say. By this
	// point nothing is waiting to be told what the shell printed, so draining
	// it costs nothing and removes the only way this can wedge.
	go func() { _, _ = io.Copy(io.Discard, c.out) }()

	err := c.hangup()
	select {
	case <-c.exited:
	case <-time.After(hangupGrace):
		// The shell ignored its console going away. End it rather than let
		// openconsole hang waiting for a process that will not leave.
		c.terminate()
	}
	// The output is released here and nowhere else. hangup leaves it open on
	// purpose, so that whatever the shell printed on its way out can still be
	// read; by the time Close is called, nobody is reading any more.
	if oerr := c.out.Close(); err == nil {
		err = oerr
	}
	return err
}

// hangup takes the console away from the shell.
//
// This is also what ends a read. Unlike a Unix pty, a pseudo-console holds its
// end of the output pipe open after the shell has exited, so a reader would
// otherwise block for ever on a terminal that has nothing left to say. Closing
// the console flushes what is left and then breaks the pipe, which is how "the
// shell exited" becomes EOF — after the last of the output, not instead of it.
func (c *conPTY) hangup() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	console := c.console
	c.mu.Unlock()

	// Input first: a shell still running sees EOF on its stdin, which is the
	// gentlest way to ask it to leave.
	err := c.in.Close()
	windows.ClosePseudoConsole(console)
	return err
}

// reap waits for the shell, records its status, and closes the terminal so
// whoever is reading it learns that the session is over.
func (c *conPTY) reap() {
	code, err := waitProcess(c.proc)

	c.mu.Lock()
	c.exitCode, c.waitErr, c.reaped = code, err, true
	windows.CloseHandle(c.proc)
	c.mu.Unlock()

	_ = c.hangup()
	close(c.exited)
}

// terminate kills a shell that would not go quietly.
func (c *conPTY) terminate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reaped {
		return
	}
	_ = windows.TerminateProcess(c.proc, 1)
}

// waitProcess blocks until a process exits and returns its exit code.
func waitProcess(proc windows.Handle) (int, error) {
	if _, err := windows.WaitForSingleObject(proc, windows.INFINITE); err != nil {
		return -1, fmt.Errorf("terminal: waiting for the shell: %w", err)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(proc, &code); err != nil {
		return -1, fmt.Errorf("terminal: reading the shell's exit status: %w", err)
	}
	return int(code), nil
}

// envBlock builds the environment CreateProcess wants: NUL-separated UTF-16
// strings ending in a NUL of their own.
func envBlock(env []string) (*uint16, error) {
	var block []uint16
	for _, entry := range env {
		if entry == "" {
			continue
		}
		u, err := windows.UTF16FromString(entry)
		if err != nil {
			return nil, fmt.Errorf("terminal: environment entry %q: %w", entry, err)
		}
		block = append(block, u...)
	}
	// The block itself is NUL-terminated, on top of each entry's own NUL. An
	// empty environment is therefore two NULs rather than one.
	block = append(block, 0)
	if len(block) == 1 {
		block = append(block, 0)
	}
	return &block[0], nil
}

// closeHandles closes every handle given, ignoring failures. Nothing useful
// can be done about a handle that will not close.
func closeHandles(handles ...windows.Handle) {
	for _, h := range handles {
		if h != 0 && h != windows.InvalidHandle {
			windows.CloseHandle(h)
		}
	}
}

var (
	kernel32                      = windows.NewLazySystemDLL("kernel32.dll")
	procCreatePseudoConsole       = kernel32.NewProc("CreatePseudoConsole")
	procUpdateProcThreadAttribute = kernel32.NewProc("UpdateProcThreadAttribute")
)

// conptySupported reports whether this Windows has pseudo-consoles at all.
//
// They arrived in Windows 10 1809. On anything older the kernel32 entry point
// is simply absent, and calling through x/sys would panic looking for it, so
// the presence of the export is checked first and turned into an error a person
// can act on.
func conptySupported() error {
	if err := procCreatePseudoConsole.Find(); err != nil {
		return fmt.Errorf("%w: sharing a terminal needs Windows 10 1809 or newer", ErrUnsupported)
	}
	return nil
}

// setPseudoConsole adds the pseudo-console attribute to a process attribute
// list.
//
// UpdateProcThreadAttribute is called directly rather than through the typed
// helper in x/sys because this particular attribute takes the console handle
// *by value*: the documented lpValue is the HPCON itself, not a pointer to it.
// The helper takes an unsafe.Pointer, which cannot express that without a
// uintptr-to-pointer conversion that go vet objects to, and rightly so.
func setPseudoConsole(list *windows.ProcThreadAttributeList, console windows.Handle) error {
	r, _, err := procUpdateProcThreadAttribute.Call(
		uintptr(unsafe.Pointer(list)),
		0, // reserved
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		uintptr(console),
		unsafe.Sizeof(console),
		0, // previous value, not wanted
		0, // return size, not wanted
	)
	if r == 0 {
		return err
	}
	return nil
}
