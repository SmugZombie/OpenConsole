package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
	"github.com/SmugZombie/OpenConsole/internal/terminal"
	"github.com/SmugZombie/OpenConsole/internal/tunnel"
)

// ptyBufferSize is the read size for terminal output. Terminal writes arrive in
// small bursts; a larger buffer would just sit idle.
const ptyBufferSize = 16 << 10

// errRelayTooSlow means the outbound queue filled: the relay or the network
// could not keep up with the terminal.
var errRelayTooSlow = errors.New("the relay is not keeping up with terminal output")

// shareFailureMessage turns a failure into a line worth putting on someone's
// terminal mid-session.
//
// Transport errors read like "tunnel: recv: failed to get reader: failed to
// read frame header: EOF", which tells a person nothing while they are trying
// to work. What matters on screen is that sharing stopped and their shell did
// not; the underlying error is kept for the summary printed afterwards.
func shareFailureMessage(err error) string {
	switch {
	case errors.Is(err, errRelayTooSlow):
		return errRelayTooSlow.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return "the relay stopped responding"
	default:
		return "lost contact with the relay"
	}
}

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

	conn, _, err := openTunnel(ctx, api.TunnelURL(), protocol.Open{
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
		api.JoinURL(sess.SessionID, sess.ViewerToken),
		api.SSHCommand(sess.SessionID, sess.SSHPort))

	restore, err := rawTerminal(stdin)
	if err != nil {
		return 1, err
	}
	defer restore()

	code, shareErr := runShare(ctx, term, conn, stdin, stdout, cfg.AllowForward, forwardLogger(stderr))

	restore()
	// A mid-session failure was already reported on the terminal as it
	// happened; this is the summary, not a second warning.
	if shareErr != nil {
		fmt.Fprintf(stderr, "\nopenconsole: session %s ended (sharing had stopped: %v)\n",
			sess.SessionID, shareErr)
	} else {
		fmt.Fprintf(stderr, "\nopenconsole: session %s ended\n", sess.SessionID)
	}
	return code, nil
}

// runShare wires the four data paths and blocks until the shell exits.
//
//	local keyboard -> pty
//	guest keyboard -> pty
//	pty           -> local screen
//	pty           -> relay -> guests
func runShare(ctx context.Context, term *terminal.Terminal, conn tunnel.Conn, stdin, stdout *os.File, allow Allowlist, logger *slog.Logger) (int, error) {
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

	// Forwarding is off unless the host asked for it. When it is on, the host
	// dials the targets; the relay never does.
	forwards := newHostForwards(allow, out.send, logger)
	defer forwards.closeAll()

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
			// Anything on a non-zero channel is a forwarded connection, not
			// the terminal.
			if !f.Channel.IsTerminal() {
				forwards.handle(ctx, f)
				continue
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

	// Losing the relay must not disturb the shell. Somebody is working in this
	// terminal; the fact that nobody is watching any more is worth a line on
	// their screen, not their session being torn out from under them.
	//
	// This runs as its own goroutine so the notice appears the moment sharing
	// breaks. An idle shell produces no output for minutes at a time, and
	// noticing only on the next read would leave the host typing into a
	// session no one can see.
	go func() {
		select {
		case <-ctx.Done():
		case <-out.Failed():
			// Drop the tunnel so the relay reclaims the session rather than
			// holding it until the TTL.
			conn.Close("sharing stopped")
			if err := out.err(); err != nil {
				// Raw mode is still on, so line endings need the carriage
				// return.
				fmt.Fprintf(stdout,
					"\r\n[openconsole] sharing stopped: %s\r\n"+
						"[openconsole] your shell is still running; type 'exit' to leave it\r\n",
					shareFailureMessage(err))
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
			// Copy: the frame outlives this loop iteration once queued. send
			// is a no-op once sharing has stopped.
			out.send(protocol.NewData(append([]byte(nil), buf[:n]...)))
		}
		if err != nil {
			break
		}
	}

	// The shell has exited. Only announce that to a relay still listening.
	code, _ := waitShell(term)
	if !out.hasFailed() {
		sendClose(ctx, conn, code)
	}
	return code, out.err()
}

// forwardLogger sends forwarding activity to the host's own terminal.
//
// The host opted into forwarding, so they should be able to see it being used.
// It is deliberately terse and goes to stderr, alongside the banner, rather
// than into the shared terminal where a guest would see it too.
func forwardLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
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

	// failed is closed the first time sharing breaks. It is a channel rather
	// than a flag so the failure can be acted on the moment it happens: the
	// alternative, checking after each terminal read, never fires while the
	// shell sits idle, and the host would go on typing into a session nobody
	// is watching.
	failed   chan struct{}
	failOnce sync.Once
}

func newSender(conn tunnel.Conn, depth int) *sender {
	return &sender{
		conn:   conn,
		q:      make(chan protocol.Frame, depth),
		failed: make(chan struct{}),
	}
}

// Failed is closed once sharing has broken.
func (s *sender) Failed() <-chan struct{} { return s.failed }

// hasFailed reports whether sharing has already broken.
func (s *sender) hasFailed() bool {
	select {
	case <-s.failed:
		return true
	default:
		return false
	}
}

// send queues a frame. A full queue means the relay cannot keep up; sharing is
// failed rather than blocking the caller, which would freeze the host's shell.
func (s *sender) send(f protocol.Frame) {
	if s.hasFailed() {
		return
	}
	select {
	case s.q <- f:
	default:
		s.fail(errRelayTooSlow)
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
	if s.failure == nil {
		s.failure = err
	}
	s.mu.Unlock()
	s.failOnce.Do(func() { close(s.failed) })
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
