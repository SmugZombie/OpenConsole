package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"

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
	parsed, err := ParseTicket(ticket)
	if err != nil {
		return err
	}
	return joinWith(ctx, cfg, parsed, stdin, stdout, stderr)
}

// joinWith attaches using an already-parsed ticket.
func joinWith(ctx context.Context, cfg Config, parsed Ticket, stdin, stdout *os.File, stderr io.Writer) error {
	sessionID, token := parsed.SessionID, parsed.Token

	// The key never leaves this process. Whether this connection can read the
	// terminal, or type into it, is decided here by what the ticket carries —
	// not by anything the relay says.
	crypt, err := parsed.E2E()
	if err != nil {
		return err
	}
	enc := guestCrypter(crypt)
	if !isTerminal(stdin) || !isTerminal(stdout) {
		return ErrNotATerminal
	}

	api := NewClient(cfg.Server)
	cols, rows := terminalSize(stdout)

	// Ask for read-only when the user did; otherwise present the ticket and
	// let the relay decide what it is worth.
	role := protocol.RoleGuest
	if cfg.ReadOnly || parsed.KeyKind == KeyViewer {
		// A watch-only ticket has no key for the writing direction, so ask for
		// the access it can actually use.
		role = protocol.RoleViewer
	}

	conn, ack, err := openTunnel(ctx, api.TunnelURL(), protocol.Open{
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

	// A link that lost its key would otherwise fill the screen with
	// ciphertext. Say what happened instead; a truncated link is the usual
	// cause.
	if ack.Encrypted && !enc.enabled() {
		conn.Close("no key")
		return fmt.Errorf(
			"this session is end-to-end encrypted but your link carries no key.\n" +
				"The ticket ends with a third part after a dot — ask for the whole thing.")
	}
	if enc.enabled() && !ack.Encrypted {
		conn.Close("unexpected plaintext")
		return fmt.Errorf(
			"your link carries a key but the relay says this session is not encrypted.\n" +
				"Do not continue: someone may be trying to read this terminal.")
	}

	readOnly := ack.Role == protocol.RoleViewer || (enc.enabled() && !crypt.CanWrite())
	if enc.enabled() {
		fmt.Fprintf(stderr, "openconsole: end-to-end encrypted; the relay cannot read this terminal\n")
	} else {
		fmt.Fprintf(stderr, "openconsole: NOT encrypted; whoever runs the relay can read this terminal\n")
	}
	if readOnly {
		fmt.Fprintf(stderr,
			"openconsole: joined %s read-only — you can watch, typing is ignored (Ctrl-] to detach)\n",
			sessionID)
	} else {
		fmt.Fprintf(stderr, "openconsole: joined %s (press Ctrl-] to detach)\n", sessionID)
	}

	// Local listeners come up before the terminal goes raw, so a failure to
	// bind is a plain error message rather than something lost in raw mode.
	// Forwarded bytes are DATA frames too, so they go out through the same
	// sealing step as keystrokes.
	forwards := newGuestForwards(
		func(f protocol.Frame) {
			sealed, err := enc.outbound(f)
			if err != nil {
				return
			}
			_ = conn.Send(ctx, sealed)
		},
		func(msg string) { fmt.Fprintf(stderr, "openconsole: %s\n", msg) },
	)
	defer forwards.Close()

	for _, spec := range cfg.Forwards {
		if readOnly {
			return fmt.Errorf("cannot forward %s: this session is read-only", spec)
		}
		addr, err := forwards.Listen(ctx, spec)
		if err != nil {
			return err
		}
		fmt.Fprintf(stderr, "openconsole: forwarding %s -> %s (on the host)\n",
			addr, net.JoinHostPort(spec.RemoteHost, strconv.Itoa(int(spec.RemotePort))))
	}

	restore, err := rawTerminal(stdin)
	if err != nil {
		return err
	}
	defer restore()

	// The host's terminal is full of escape sequences. On Windows a console
	// shows them as text unless it is told otherwise; elsewhere this does
	// nothing.
	restoreVT := enableVirtualTerminal(stdout)
	defer restoreVT()

	err = runJoin(ctx, conn, stdin, stdout, readOnly, forwards, enc)

	restore()
	restoreVT()
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
func runJoin(ctx context.Context, conn tunnel.Conn, stdin, stdout *os.File, readOnly bool, forwards *guestForwards, enc crypter) error {
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
						if sealed, serr := enc.outbound(protocol.NewData(buf[:i])); serr == nil {
							_ = conn.Send(ctx, sealed)
						}
					}
					input <- errDetached
					return
				}
				// A read-only guest still reads the keyboard, because
				// Ctrl-] has to keep working, but nothing else is sent. The
				// relay would drop it anyway; not sending saves a round trip
				// and makes the intent obvious here.
				if !readOnly {
					sealed, serr := enc.outbound(protocol.NewData(buf[:n]))
					if serr != nil {
						input <- serr
						return
					}
					if err := conn.Send(ctx, sealed); err != nil {
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

			// One decryption point for the terminal and every forwarded
			// connection alike.
			f, err = enc.inbound(f)
			if err != nil {
				output <- err
				return
			}

			// Anything on a non-zero channel belongs to a forwarded
			// connection, not to the screen.
			if !f.Channel.IsTerminal() {
				if forwards != nil {
					forwards.handle(f)
				}
				continue
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
