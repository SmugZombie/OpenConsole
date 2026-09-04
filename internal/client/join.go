package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
	"github.com/SmugZombie/OpenConsole/internal/tunnel"
)

// detachKey is Ctrl-] — the same escape ancient telnet used, and a key
// combination no shell binds.
//
// A guest's terminal is in raw mode, so Ctrl-C and Ctrl-D go to the remote
// shell where they belong. Without a dedicated escape there would be no way to
// leave without killing the host's shell.
const detachKey = 0x1d

// Join attaches to a shared terminal as a guest. It blocks until the guest
// detaches or the host ends the session.
func Join(ctx context.Context, cfg Config, ticket string, stdin, stdout *os.File, stderr io.Writer) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	sessionID, token, err := ParseTicket(ticket)
	if err != nil {
		return err
	}
	if !isTerminal(stdin) || !isTerminal(stdout) {
		return ErrNotATerminal
	}

	api := NewClient(cfg.Server)
	cols, rows := terminalSize(stdout)

	// Ask for read-only when the user did; otherwise present the ticket and
	// let the relay decide what it is worth.
	role := protocol.RoleGuest
	if cfg.ReadOnly {
		role = protocol.RoleViewer
	}

	conn, granted, err := openTunnel(ctx, api.TunnelURL(), protocol.Open{
		SessionID: sessionID,
		Role:      role,
		Token:     token,
		Cols:      cols,
		Rows:      rows,
	})
	if err != nil {
		return err
	}
	defer conn.Close("guest left")

	readOnly := granted == protocol.RoleViewer
	if readOnly {
		fmt.Fprintf(stderr,
			"openconsole: joined %s read-only — you can watch, typing is ignored (Ctrl-] to detach)\n",
			sessionID)
	} else {
		fmt.Fprintf(stderr, "openconsole: joined %s (press Ctrl-] to detach)\n", sessionID)
	}

	restore, err := rawTerminal(stdin)
	if err != nil {
		return err
	}
	defer restore()

	err = runJoin(ctx, conn, stdin, stdout, readOnly)

	restore()
	switch {
	case errors.Is(err, errDetached):
		fmt.Fprintf(stderr, "\nopenconsole: detached\n")
		return nil
	case err == nil, errors.Is(err, tunnel.ErrClosed):
		fmt.Fprintf(stderr, "\nopenconsole: the host ended the session\n")
		return nil
	default:
		return err
	}
}

// errDetached is the sentinel for a local Ctrl-] rather than a failure.
var errDetached = errors.New("detached")

// runJoin pumps keystrokes to the relay and remote output to the screen.
func runJoin(ctx context.Context, conn tunnel.Conn, stdin, stdout *os.File, readOnly bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Keyboard to the relay. os.Stdin cannot be interrupted by a context, so
	// this goroutine is left to die with the process; the read loop below is
	// what decides when Join returns.
	input := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				if i := indexByte(buf[:n], detachKey); i >= 0 {
					// Send anything typed before the escape, then stop.
					if i > 0 && !readOnly {
						_ = conn.Send(ctx, protocol.NewData(buf[:i]))
					}
					input <- errDetached
					return
				}
				// A read-only guest still reads the keyboard, because
				// Ctrl-] has to keep working, but nothing else is sent. The
				// relay would drop it anyway; not sending saves a round trip
				// and makes the intent obvious here.
				if !readOnly {
					if err := conn.Send(ctx, protocol.NewData(buf[:n])); err != nil {
						input <- err
						return
					}
				}
			}
			if err != nil {
				input <- err
				return
			}
		}
	}()

	// Remote output to the screen.
	output := make(chan error, 1)
	go func() {
		for {
			f, err := conn.Recv(ctx)
			if err != nil {
				output <- err
				return
			}
			switch f.Type {
			case protocol.TypeData:
				if _, err := stdout.Write(f.Payload); err != nil {
					output <- err
					return
				}
			case protocol.TypePing:
				if err := conn.Send(ctx, protocol.Frame{Type: protocol.TypePong, Payload: f.Payload}); err != nil {
					output <- err
					return
				}
			case protocol.TypeResize:
				// The host's terminal changed shape. A terminal guest cannot
				// resize its own window, so this is informational; a browser
				// client will act on it.
			case protocol.TypeClose:
				output <- tunnel.ErrClosed
				return
			case protocol.TypeError:
				output <- closeReason(f)
				return
			}
		}
	}()

	select {
	case err := <-input:
		return err
	case err := <-output:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// indexByte reports the first index of c in b, or -1.
func indexByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}
