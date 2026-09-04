package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
	"github.com/SmugZombie/OpenConsole/internal/terminal"
	"github.com/SmugZombie/OpenConsole/internal/tunnel"
)

// ptyBufferSize is the read size for terminal output. Terminal writes arrive in
// small bursts; a larger buffer would just sit idle.
const ptyBufferSize = 16 << 10

// outboundQueue is how many frames may be waiting to go to the relay.
//
// The host's own shell must never stall because the relay or the network is
// slow, so sends are queued rather than performed inline. If the queue fills,
// sharing stops and the local shell carries on — an explicit stop is better
// than silently dropping output and leaving guests with a corrupted screen.
const outboundQueue = 256

// Share runs a shell locally and mirrors it through a relay.
//
// It blocks until the shell exits. The returned int is the shell's exit code.
func Share(ctx context.Context, cfg Config, stdin, stdout *os.File, stderr io.Writer) (int, error) {
	if err := cfg.Validate(); err != nil {
		return 1, err
	}

	api := NewClient(cfg.Server)

	// Everything below assumes a real terminal, so fail before creating a
	// session that would otherwise leak until its TTL.
	if !isTerminal(stdin) || !isTerminal(stdout) {
		return 1, ErrNotATerminal
	}

	sess, err := api.CreateSession(ctx)
	if err != nil {
		return 1, err
	}

	cols, rows := terminalSize(stdout)

	term, err := terminal.Start(terminal.Options{
		Shell: cfg.Shell,
		Cols:  cols,
		Rows:  rows,
		Env:   sharedEnv(sess.SessionID),
	})
	if err != nil {
		return 1, err
	}
	defer term.Close()

	conn, err := openTunnel(ctx, api.TunnelURL(), protocol.Open{
		SessionID: sess.SessionID,
		Role:      protocol.RoleHost,
		Token:     sess.HostToken,
		Cols:      cols,
		Rows:      rows,
	})
	if err != nil {
		return 1, fmt.Errorf("connecting to relay: %w", err)
	}
	defer conn.Close("host exited")

	printBanner(stderr, cfg, sess,
		api.JoinURL(sess.SessionID, sess.GuestToken),
		api.SSHCommand(sess.SessionID, sess.SSHPort))

	restore, err := rawTerminal(stdin)
	if err != nil {
		return 1, err
	}
	defer restore()

	code, shareErr := runShare(ctx, term, conn, stdin, stdout)

	restore()
	if shareErr != nil {
		fmt.Fprintf(stderr, "\r\nopenconsole: sharing stopped: %v\r\n", shareErr)
	}
	fmt.Fprintf(stderr, "\r\nopenconsole: session %s ended\r\n", sess.SessionID)
	return code, nil
}

// runShare wires the four data paths and blocks until the shell exits.
//
//	local keyboard -> pty
//	guest keyboard -> pty
//	pty           -> local screen
//	pty           -> relay -> guests
func runShare(ctx context.Context, term *terminal.Terminal, conn tunnel.Conn, stdin, stdout *os.File) (int, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// ptyMu serialises writes to the pty. Local keystrokes and guest keystrokes
	// arrive on different goroutines, and interleaving them mid-write would
	// split multi-byte input such as an arrow key's escape sequence.
	var ptyMu sync.Mutex
	writePTY := func(p []byte) error {
		ptyMu.Lock()
		defer ptyMu.Unlock()
		_, err := term.Write(p)
		return err
	}

	out := newSender(conn, outboundQueue)
	go out.run(ctx)

	// Local keyboard into the shell. os.Stdin has no interruptible read, so
	// this goroutine may outlive cancellation; it exits when the process does.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				if werr := writePTY(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Guest input and control frames from the relay.
	go func() {
		for {
			f, err := conn.Recv(ctx)
			if err != nil {
				out.fail(err)
				return
			}
			switch f.Type {
			case protocol.TypeData:
				if err := writePTY(f.Payload); err != nil {
					out.fail(err)
					return
				}
			case protocol.TypePing:
				out.send(protocol.Frame{Type: protocol.TypePong, Payload: append([]byte(nil), f.Payload...)})
			case protocol.TypePong:
			case protocol.TypeClose, protocol.TypeError:
				out.fail(closeReason(f))
				return
			}
		}
	}()

	// Local window resizes follow the host's terminal and are announced to
	// guests; the host owns its size.
	winch := make(chan os.Signal, 1)
	notifyResize(winch)
	defer stopResize(winch)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-winch:
				c, r := terminalSize(stdout)
				_ = term.Resize(c, r)
				if f, err := protocol.NewControl(protocol.TypeResize, protocol.Resize{Cols: c, Rows: r}); err == nil {
					out.send(f)
				}
			}
		}
	}()

	// Shell output to the local screen and to the relay. This runs on the
	// calling goroutine: when it ends, the shell has ended.
	buf := make([]byte, ptyBufferSize)
	for {
		n, err := term.Read(buf)
		if n > 0 {
			if _, werr := stdout.Write(buf[:n]); werr != nil {
				break
			}
			// Copy: the frame outlives this loop iteration once queued.
			out.send(protocol.NewData(append([]byte(nil), buf[:n]...)))
		}
		if err != nil {
			break
		}
		if err := out.err(); err != nil {
			// The relay is gone. Keep the local shell running and report it.
			code, _ := waitShell(term)
			return code, err
		}
	}

	code, _ := waitShell(term)
	sendClose(ctx, conn, code)
	return code, out.err()
}

// waitShell reaps the shell.
func waitShell(t *terminal.Terminal) (int, error) {
	_ = t.Close()
	return t.Wait()
}

// sendClose tells the relay the terminal ended, on a best-effort basis.
func sendClose(ctx context.Context, conn tunnel.Conn, code int) {
	_ = tunnel.SendControl(ctx, conn, protocol.TypeClose, protocol.Close{
		Reason:   "host shell exited",
		ExitCode: &code,
	})
}

// closeReason turns a CLOSE or ERROR frame into an error for the host.
func closeReason(f protocol.Frame) error {
	if f.Type == protocol.TypeError {
		var e protocol.Error
		if err := protocol.DecodeControl(f, &e); err == nil {
			return relayRefusal(e)
		}
	}
	return tunnel.ErrClosed
}

// sender owns the single goroutine allowed to write to the tunnel.
type sender struct {
	conn tunnel.Conn
	q    chan protocol.Frame

	mu      sync.Mutex
	failure error
}

func newSender(conn tunnel.Conn, depth int) *sender {
	return &sender{conn: conn, q: make(chan protocol.Frame, depth)}
}

// send queues a frame. A full queue means the relay cannot keep up; sharing is
// failed rather than blocking the caller, which would freeze the host's shell.
func (s *sender) send(f protocol.Frame) {
	select {
	case s.q <- f:
	default:
		s.fail(errors.New("relay is not keeping up with terminal output"))
	}
}

func (s *sender) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case f := <-s.q:
			if err := s.conn.Send(ctx, f); err != nil {
				s.fail(err)
				return
			}
		}
	}
}

// fail records the first failure; later ones are noise from the same cause.
func (s *sender) fail(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure == nil {
		s.failure = err
	}
}

func (s *sender) err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if errors.Is(s.failure, tunnel.ErrClosed) || errors.Is(s.failure, context.Canceled) {
		return nil
	}
	return s.failure
}

// sharedEnv marks the child shell so scripts and prompts can tell they are
// being shared. The session ID is public; the tokens are not passed down.
func sharedEnv(sessionID string) []string {
	env := os.Environ()
	return append(env,
		"OPENCONSOLE=1",
		"OPENCONSOLE_SESSION="+sessionID,
	)
}
