package session

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
)

// Stream is the transport-shaped dependency the bridge needs.
//
// It is declared here, by the consumer, rather than imported from the tunnel
// package. That inversion is what keeps session management free of any
// transport: the bridge can be exercised in tests with an in-memory pipe, and
// the WebSocket implementation satisfies this interface without either package
// knowing about the other.
type Stream interface {
	Send(ctx context.Context, f protocol.Frame) error
	Recv(ctx context.Context) (protocol.Frame, error)
	Close(reason string) error
}

// Bridge errors.
var (
	// ErrHostAlreadyAttached means a second host tried to claim a session. A
	// session has exactly one terminal.
	ErrHostAlreadyAttached = errors.New("session: host already attached")
	// ErrNoHost means a guest arrived before the host connected, or after it
	// left.
	ErrNoHost = errors.New("session: host not connected")
	// ErrBridgeClosed means the session's terminal has ended.
	ErrBridgeClosed = errors.New("session: bridge closed")
)

// guestQueueDepth is how many frames may be outstanding to one guest.
//
// Fan-out must never let a slow guest stall the host's terminal, so delivery is
// buffered and a guest that falls this far behind is disconnected instead of
// applying backpressure. Dropping one guest is strictly better than freezing
// the shared terminal for everyone.
const guestQueueDepth = 256

// scrollbackBytes is how much recent output a joining guest is replayed.
//
// Without it a guest sees a blank screen until the host next types, which looks
// broken. 64 KiB covers a screenful of even a busy full-screen program while
// staying a trivial per-session cost.
const scrollbackBytes = 64 << 10

// guestSendTimeout bounds a single write to one guest. Thirty seconds to
// deliver one frame means the connection is broken, not merely slow.
const guestSendTimeout = 30 * time.Second

// Bridge is the live wiring of one session: one host terminal, any number of
// guests watching and typing into it.
type Bridge struct {
	id  string
	log *slog.Logger

	mu       sync.Mutex
	host     Stream
	guests   map[*guestConn]struct{}
	closed   bool
	cols     uint16
	rows     uint16
	scroll   *ringBuffer
	onClosed func()
}

// guestConn is one attached guest and its outbound queue.
//
// Teardown has two flavours, and the difference is visible to the person at the
// other end. A graceful stop lets the writer flush what is already queued —
// which is how the final CLOSE frame reaches a guest when the host exits,
// instead of the connection just vanishing. A kill is for a guest that has
// stopped reading: there is no point flushing to it, and the transport has to
// be torn down to unblock whoever is stuck writing.
type guestConn struct {
	out    chan protocol.Frame
	closed chan struct{} // writer should flush and exit
	dead   chan struct{} // transport must be torn down now

	stopOnce sync.Once
	killOnce sync.Once
}

// stop asks the writer to flush and finish.
func (g *guestConn) stop() { g.stopOnce.Do(func() { close(g.closed) }) }

// kill tears the connection down without flushing.
func (g *guestConn) kill() {
	g.killOnce.Do(func() { close(g.dead) })
	g.stop()
}

// newBridge creates a bridge for session id.
func newBridge(id string, log *slog.Logger) *Bridge {
	return &Bridge{
		id:     id,
		log:    log,
		guests: make(map[*guestConn]struct{}),
		scroll: newRingBuffer(scrollbackBytes),
	}
}

// ID reports the session this bridge serves.
func (b *Bridge) ID() string { return b.id }

// Guests reports how many guests are currently attached.
func (b *Bridge) Guests() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.guests)
}

// HostAttached reports whether a host terminal is connected.
func (b *Bridge) HostAttached() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.host != nil
}

// Size reports the host terminal's last known window size.
func (b *Bridge) Size() (cols, rows uint16) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cols, b.rows
}

// ServeHost pumps frames from the host terminal until it disconnects.
//
// It blocks. When it returns the session is over: every guest is disconnected
// and the bridge is closed, because there is nothing to attach to without the
// host.
func (b *Bridge) ServeHost(ctx context.Context, s Stream, cols, rows uint16) error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return ErrBridgeClosed
	}
	if b.host != nil {
		b.mu.Unlock()
		return ErrHostAlreadyAttached
	}
	b.host = s
	b.cols, b.rows = cols, rows
	b.mu.Unlock()

	b.log.Info("host attached", slog.String("session_id", b.id))
	defer func() {
		b.log.Info("host detached", slog.String("session_id", b.id))
		b.Close("host disconnected")
	}()

	for {
		f, err := s.Recv(ctx)
		if err != nil {
			return err
		}
		switch f.Type {
		case protocol.TypeData:
			// Copy before queueing: the transport owns the payload only
			// until the next Recv, and it is about to be handed to other
			// goroutines.
			payload := append([]byte(nil), f.Payload...)
			b.mu.Lock()
			b.scroll.Write(payload)
			b.mu.Unlock()
			b.broadcast(protocol.Frame{Type: protocol.TypeData, Channel: f.Channel, Payload: payload})

		case protocol.TypeResize:
			var r protocol.Resize
			if err := protocol.DecodeControl(f, &r); err != nil {
				b.log.Warn("bad resize from host", slog.String("session_id", b.id), slog.Any("error", err))
				continue
			}
			b.mu.Lock()
			b.cols, b.rows = r.Cols, r.Rows
			b.mu.Unlock()
			b.broadcastControl(protocol.TypeResize, r)

		case protocol.TypePing:
			if err := s.Send(ctx, protocol.Frame{Type: protocol.TypePong, Payload: f.Payload}); err != nil {
				return err
			}

		case protocol.TypePong:
			// Liveness only; receiving any frame is the signal.

		case protocol.TypeClose:
			var c protocol.Close
			_ = protocol.DecodeControl(f, &c)
			b.broadcastControl(protocol.TypeClose, c)
			return nil

		default:
			b.log.Warn("unexpected frame from host",
				slog.String("session_id", b.id),
				slog.String("type", f.Type.String()))
		}
	}
}

// ServeGuest pumps frames for one guest until it disconnects or the session
// ends. It blocks.
func (b *Bridge) ServeGuest(ctx context.Context, s Stream) error {
	g := &guestConn{
		out:    make(chan protocol.Frame, guestQueueDepth),
		closed: make(chan struct{}),
		dead:   make(chan struct{}),
	}

	b.mu.Lock()
	if b.closed || b.host == nil {
		b.mu.Unlock()
		return ErrNoHost
	}
	b.guests[g] = struct{}{}
	cols, rows := b.cols, b.rows
	backlog := b.scroll.Bytes()
	n := len(b.guests)
	b.mu.Unlock()

	b.log.Info("guest attached", slog.String("session_id", b.id), slog.Int("guests", n))
	defer func() {
		b.mu.Lock()
		delete(b.guests, g)
		n := len(b.guests)
		b.mu.Unlock()
		g.stop()
		b.log.Info("guest detached", slog.String("session_id", b.id), slog.Int("guests", n))
	}()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// This watchdog starts before the first send, and before the pumps below,
	// because every one of them can block inside the transport.
	//
	// A guest gets dropped precisely when its transport has stopped accepting
	// writes, which means whatever goroutine is writing is blocked *inside*
	// Send and will never look at g.closed again. Closing the stream is what
	// actually unblocks it; without this, dropping a slow guest would leak the
	// connection and its goroutines.
	go func() {
		select {
		case <-g.dead:
			_ = s.Close("disconnected by relay")
			cancel()
		case <-ctx.Done():
		}
	}()

	// Tell the guest the terminal's shape, then replay recent output so it has
	// something on screen immediately. Both are bounded: a client that
	// connects and never reads must not park this goroutine.
	if cols != 0 || rows != 0 {
		if err := sendBounded(ctx, s, mustControl(protocol.TypeResize, protocol.Resize{Cols: cols, Rows: rows})); err != nil {
			return err
		}
	}
	if len(backlog) > 0 {
		if err := sendBounded(ctx, s, protocol.Frame{Type: protocol.TypeData, Payload: backlog}); err != nil {
			return err
		}
	}

	// Writer: drains this guest's queue.
	writeErr := make(chan error, 1)
	go func() {
		defer close(writeErr)
		for {
			select {
			case <-ctx.Done():
				return
			case <-g.closed:
				// Flush what is already queued so a final CLOSE is not lost.
				// If the guest is dead the stream is closed by now, so these
				// sends fail immediately rather than stalling.
				flush(ctx, s, g.out)
				writeErr <- ErrBridgeClosed
				return
			case f := <-g.out:
				if err := sendBounded(ctx, s, f); err != nil {
					writeErr <- err
					return
				}
			}
		}
	}()

	// Reader: guest input goes to the host.
	readErr := make(chan error, 1)
	go func() {
		defer close(readErr)
		for {
			f, err := s.Recv(ctx)
			if err != nil {
				readErr <- err
				return
			}
			if err := b.fromGuest(ctx, s, f); err != nil {
				readErr <- err
				return
			}
		}
	}()

	select {
	case err := <-readErr:
		return err
	case err := <-writeErr:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// fromGuest handles one frame arriving from a guest.
func (b *Bridge) fromGuest(ctx context.Context, s Stream, f protocol.Frame) error {
	switch f.Type {
	case protocol.TypeData:
		return b.toHost(ctx, protocol.Frame{Type: protocol.TypeData, Payload: f.Payload})

	case protocol.TypeResize:
		// Guests do not resize the host's terminal. With several guests
		// attached, whichever one last resized would win and the host's real
		// window would fight them. The host owns its size and announces it;
		// guest-driven sizing needs multi-guest arbitration, which is a later
		// phase.
		b.log.Debug("ignoring resize from guest", slog.String("session_id", b.id))
		return nil

	case protocol.TypePing:
		return s.Send(ctx, protocol.Frame{Type: protocol.TypePong, Payload: f.Payload})

	case protocol.TypePong:
		return nil

	case protocol.TypeClose:
		return ErrBridgeClosed

	default:
		b.log.Warn("unexpected frame from guest",
			slog.String("session_id", b.id),
			slog.String("type", f.Type.String()))
		return nil
	}
}

// toHost forwards a frame to the host terminal.
func (b *Bridge) toHost(ctx context.Context, f protocol.Frame) error {
	b.mu.Lock()
	host := b.host
	b.mu.Unlock()
	if host == nil {
		return ErrNoHost
	}
	return host.Send(ctx, f)
}

// broadcast queues a frame for every guest, dropping any that has fallen behind.
func (b *Bridge) broadcast(f protocol.Frame) {
	b.mu.Lock()
	slow := make([]*guestConn, 0, len(b.guests))
	for g := range b.guests {
		select {
		case g.out <- f:
		default:
			slow = append(slow, g)
			delete(b.guests, g)
		}
	}
	b.mu.Unlock()

	for _, g := range slow {
		b.log.Warn("dropping guest that fell behind", slog.String("session_id", b.id))
		g.kill()
	}
}

// broadcastControl marshals v and broadcasts it as a control frame.
func (b *Bridge) broadcastControl(t protocol.Type, v any) {
	f, err := protocol.NewControl(t, v)
	if err != nil {
		b.log.Error("marshal control frame", slog.String("type", t.String()), slog.Any("error", err))
		return
	}
	b.broadcast(f)
}

// Close ends the session: every guest is disconnected and further attachments
// are refused. It is safe to call repeatedly.
func (b *Bridge) Close(reason string) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	b.host = nil
	guests := make([]*guestConn, 0, len(b.guests))
	for g := range b.guests {
		guests = append(guests, g)
	}
	b.guests = make(map[*guestConn]struct{})
	onClosed := b.onClosed
	b.mu.Unlock()

	for _, g := range guests {
		g.stop()
	}
	if onClosed != nil {
		onClosed()
	}
}

// flush drains whatever is already queued, stopping at the first failure or
// once the queue is empty. It never waits for new frames.
func flush(ctx context.Context, s Stream, out <-chan protocol.Frame) {
	for {
		select {
		case f := <-out:
			if err := sendBounded(ctx, s, f); err != nil {
				return
			}
		default:
			return
		}
	}
}

// sendBounded writes one frame with a deadline.
//
// The deadline matters because a half-open connection — one where writes block
// until the kernel buffer fills rather than failing — would otherwise park the
// caller indefinitely.
func sendBounded(ctx context.Context, s Stream, f protocol.Frame) error {
	sendCtx, cancel := context.WithTimeout(ctx, guestSendTimeout)
	defer cancel()
	return s.Send(sendCtx, f)
}

// mustControl marshals a control frame whose payload is a fixed struct from
// this package, so marshalling cannot fail in practice.
func mustControl(t protocol.Type, v any) protocol.Frame {
	f, err := protocol.NewControl(t, v)
	if err != nil {
		// Only reachable if a payload type in this package stops being
		// JSON-encodable, which a test would catch first.
		panic("session: marshalling " + t.String() + ": " + err.Error())
	}
	return f
}
