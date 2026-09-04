package session

import (
	"context"
	"errors"
	"log/slog"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
)

// Channel routing.
//
// Guests number their own channels, so two guests will happily both open
// channel 1. The relay therefore keeps two numbering spaces and translates
// between them: each guest's own IDs on one side, relay-assigned IDs toward the
// host on the other. Without that, one guest's forwarded database connection
// would receive another guest's bytes.
//
//	guest A ch 1 ─┐
//	guest B ch 1 ─┼─▶ relay ch 1, 2, 3 ─▶ host
//	guest A ch 2 ─┘
//
// Channel 0 is the terminal and is never translated: it is broadcast to every
// guest, which is the whole point of a shared terminal.

// forwardQueueDepth bounds how much forwarded data may be waiting for one
// guest.
//
// End-to-end flow control is what actually keeps this from filling: neither
// side may send more than its peer has granted, so the amount in flight is
// bounded by the window. This is sized comfortably above that so it acts as a
// backstop rather than a limit anyone reaches in normal use.
//
// It still matters, because terminal output and TCP bytes fail differently.
// Dropping terminal output makes a screen look wrong for a moment; dropping
// TCP bytes silently corrupts whatever is being carried. So a forward that
// somehow overruns is closed, and that one connection resets rather than either
// end being lied to.
const forwardQueueDepth = (protocol.InitialWindow / (32 << 10)) * 4

// forward is one guest's TCP stream, as the relay sees it.
type forward struct {
	guest     *guestConn
	guestChan protocol.ChannelID
	hostChan  protocol.ChannelID

	// out carries host→guest bytes. It is separate from the guest's terminal
	// queue so that a bulk transfer cannot fill the queue the terminal shares,
	// and so an overrun closes one forward instead of dropping the guest.
	out    chan protocol.Frame
	closed chan struct{}
	// last carries a close the relay itself decided on. It has a slot of its
	// own because the reason for sending one is usually that the ordinary
	// queue is full, and a stream that is being given up on has to be able to
	// say so even then.
	last chan protocol.Frame
}

// openForward allocates a relay-side channel for a guest's request.
//
// It returns the host-side channel ID the request should be rewritten to.
func (b *Bridge) openForward(g *guestConn, guestChan protocol.ChannelID) (*forward, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed || b.host == nil {
		return nil, ErrNoHost
	}
	if len(b.forwards) >= protocol.MaxChannels {
		return nil, ErrChannelLimit
	}
	if _, exists := g.channels[guestChan]; exists {
		return nil, ErrChannelInUse
	}

	// IDs are never reused within a session. Reuse would let a late frame from
	// a closed stream land on a new one.
	b.nextChan++
	hostChan := b.nextChan

	f := &forward{
		guest:     g,
		guestChan: guestChan,
		hostChan:  hostChan,
		out:       make(chan protocol.Frame, forwardQueueDepth),
		closed:    make(chan struct{}),
		last:      make(chan protocol.Frame, 1),
	}
	b.forwards[hostChan] = f
	g.channels[guestChan] = hostChan
	return f, nil
}

// lookupGuestChannel maps one of a guest's channel IDs to the host-side ID.
func (b *Bridge) lookupGuestChannel(g *guestConn, guestChan protocol.ChannelID) (protocol.ChannelID, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	h, ok := g.channels[guestChan]
	return h, ok
}

// lookupHostChannel maps a host-side channel ID back to its forward.
func (b *Bridge) lookupHostChannel(hostChan protocol.ChannelID) (*forward, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	f, ok := b.forwards[hostChan]
	return f, ok
}

// closeForward removes a forward and stops its writer.
func (b *Bridge) closeForward(hostChan protocol.ChannelID) *forward {
	b.mu.Lock()
	f, ok := b.forwards[hostChan]
	if ok {
		delete(b.forwards, hostChan)
		delete(f.guest.channels, f.guestChan)
	}
	b.mu.Unlock()

	if !ok {
		return nil
	}
	f.stop()
	return f
}

// retireForward ends a forward the relay itself has given up on, and tells both
// ends that it did.
//
// Nothing else says so. A forward dropped without a word leaves the guest
// holding a socket that will never deliver another byte and never end, and the
// host holding a target connection open for a stream nobody is reading. Both
// wait for the other, and the relay is the only one that knows.
//
// The guest's notice is queued before the writer is stopped, because stopping
// it is what makes the notice go out.
func (b *Bridge) retireForward(ctx context.Context, hostChan protocol.ChannelID, reason string) {
	fwd, ok := b.lookupHostChannel(hostChan)
	if !ok {
		return
	}

	notice := onChannel(mustControl(protocol.TypeClose, protocol.Close{Reason: reason}), fwd.guestChan)
	select {
	case fwd.last <- notice:
	default:
		// Something has already given this forward its last word. One is
		// enough, and the first reason is the true one.
	}

	b.closeForward(hostChan)
	b.tellHostForwardEnded(ctx, hostChan, reason)
}

// tellHostForwardEnded asks the host to drop its end of a forward.
//
// Best effort: the host may be gone, which is one of the ways a forward ends
// in the first place.
func (b *Bridge) tellHostForwardEnded(ctx context.Context, hostChan protocol.ChannelID, reason string) {
	_ = b.toHost(ctx, onChannel(
		mustControl(protocol.TypeClose, protocol.Close{Reason: reason}), hostChan))
}

// closeGuestForwards tears down every forward a guest owns, telling the host so
// it can drop the far end. Called when the guest disconnects.
func (b *Bridge) closeGuestForwards(ctx context.Context, g *guestConn) {
	b.mu.Lock()
	owned := make([]*forward, 0, len(g.channels))
	for guestChan, hostChan := range g.channels {
		if f, ok := b.forwards[hostChan]; ok {
			owned = append(owned, f)
			delete(b.forwards, hostChan)
		}
		delete(g.channels, guestChan)
	}
	b.mu.Unlock()

	for _, f := range owned {
		f.stop()
		// Best effort: the host may already be gone, in which case there is
		// nothing left to tell.
		_ = b.toHost(ctx, onChannel(
			mustControl(protocol.TypeClose, protocol.Close{Reason: "guest disconnected"}),
			f.hostChan))
	}
}

func (f *forward) stop() {
	select {
	case <-f.closed:
	default:
		close(f.closed)
	}
}

// queue hands a frame to the guest's forward writer.
//
// It never blocks: this runs on the host's read loop, and stalling there would
// freeze the terminal for everyone because one guest is slow to drain a bulk
// transfer.
func (f *forward) queue(fr protocol.Frame) bool {
	select {
	case f.out <- fr:
		return true
	case <-f.closed:
		return false
	default:
		return false
	}
}

// runForwardWriter drains one forward to its guest.
func (b *Bridge) runForwardWriter(ctx context.Context, s Stream, f *forward) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-f.closed:
			b.flushForward(ctx, s, f)
			return
		case fr := <-f.out:
			if err := sendBounded(ctx, s, fr); err != nil {
				b.log.Debug("forward write failed",
					slog.String("session_id", b.id),
					slog.Any("error", err))
				// The guest cannot be told — telling it is what just failed —
				// but the host is still there holding a socket open for a
				// stream that has no reader. It should not have to guess.
				b.closeForward(f.hostChan)
				b.tellHostForwardEnded(ctx, f.hostChan, "the guest could not be written to")
				return
			}
		}
	}
}

// flushForward delivers what a stopped forward had already been handed.
//
// The close path queues the guest's CLOSE and then stops the forward, so both
// are ready by the time this goroutine looks at them and a select picks
// between the two at random. Returning on the stop dropped the frame that says
// the stream ended about half the time, and left the guest holding a
// connection that had gone quiet rather than closed — for a forwarded
// database connection, a client waiting on a socket nobody will ever write to
// again.
//
// Stopping means "no more will be accepted", not "discard what was". Anything
// queued before the stop goes out; anything after it does not, and a write
// that fails ends the flush, because a guest that cannot be written to cannot
// be told anything either.
func (b *Bridge) flushForward(ctx context.Context, s Stream, f *forward) {
	for {
		select {
		case fr := <-f.out:
			if err := sendBounded(ctx, s, fr); err != nil {
				b.log.Debug("forward flush failed",
					slog.String("session_id", b.id),
					slog.Any("error", err))
				return
			}
		default:
			// Whatever the relay decided to say goes last, after the bytes it
			// is closing behind.
			select {
			case fr := <-f.last:
				if err := sendBounded(ctx, s, fr); err != nil {
					b.log.Debug("forward close notice failed",
						slog.String("session_id", b.id),
						slog.Any("error", err))
				}
			default:
			}
			return
		}
	}
}

// onChannel returns a copy of a frame addressed to a different channel.
//
// Translating IDs is the relay's whole job here, and doing it by copy keeps a
// frame from being mutated while another goroutine still holds it.
func onChannel(f protocol.Frame, id protocol.ChannelID) protocol.Frame {
	f.Channel = id
	return f
}

// fromGuestChannel routes a frame a guest sent on a forwarding channel.
func (b *Bridge) fromGuestChannel(ctx context.Context, s Stream, g *guestConn, f protocol.Frame) error {
	// Read-only means read-only. A forwarded connection reaches whatever the
	// host can reach, which is a far larger capability than typing, so a
	// viewer must not get one by opening a channel instead.
	if !g.canWrite {
		return sendBounded(ctx, s, onChannel(mustControl(protocol.TypeError, protocol.Error{
			Code:    protocol.ErrCodeForwardDenied,
			Message: "this session is read-only",
		}), f.Channel))
	}

	switch f.Type {
	case protocol.TypeOpen:
		return b.openGuestChannel(ctx, s, g, f)

	case protocol.TypeData, protocol.TypeWindow, protocol.TypeClose, protocol.TypeError:
		hostChan, ok := b.lookupGuestChannel(g, f.Channel)
		if !ok {
			// Frames after a close are ordinary — both ends can close at once
			// — so this is not worth an error back to the guest.
			return nil
		}
		if f.Type == protocol.TypeClose || f.Type == protocol.TypeError {
			b.closeForward(hostChan)
		}
		return b.toHost(ctx, onChannel(f, hostChan))

	default:
		return nil
	}
}

// openGuestChannel allocates a channel and passes the request to the host.
func (b *Bridge) openGuestChannel(ctx context.Context, s Stream, g *guestConn, f protocol.Frame) error {
	var req protocol.ChannelOpen
	if err := protocol.DecodeControl(f, &req); err != nil {
		return b.refuseChannel(ctx, s, f.Channel, protocol.ErrCodeProtocol, "malformed channel open")
	}
	if err := req.Validate(); err != nil {
		return b.refuseChannel(ctx, s, f.Channel, protocol.ErrCodeProtocol, "invalid channel open")
	}

	fwd, err := b.openForward(g, f.Channel)
	switch {
	case errors.Is(err, ErrChannelLimit):
		return b.refuseChannel(ctx, s, f.Channel, protocol.ErrCodeChannelLimit, "too many open forwards")
	case errors.Is(err, ErrChannelInUse):
		return b.refuseChannel(ctx, s, f.Channel, protocol.ErrCodeProtocol, "channel already open")
	case err != nil:
		return b.refuseChannel(ctx, s, f.Channel, protocol.ErrCodeSessionNotFound, "the host is not connected")
	}

	// One writer per forward, so a bulk transfer to one guest cannot hold up
	// the terminal or another guest's stream.
	go b.runForwardWriter(ctx, s, fwd)

	b.log.Info("forward opened",
		slog.String("session_id", b.id),
		slog.String("target", req.Target()),
		slog.Uint64("channel", uint64(fwd.hostChan)))

	// The host answers with OPEN or ERROR on this channel; the reply is routed
	// back by fromHostChannel.
	return b.toHost(ctx, onChannel(f, fwd.hostChan))
}

// refuseChannel tells a guest its channel could not be opened.
func (b *Bridge) refuseChannel(ctx context.Context, s Stream, ch protocol.ChannelID, code, msg string) error {
	return sendBounded(ctx, s, onChannel(mustControl(protocol.TypeError, protocol.Error{
		Code:    code,
		Message: msg,
	}), ch))
}

// fromHostChannel routes a frame the host sent on a forwarding channel back to
// the one guest that owns it.
func (b *Bridge) fromHostChannel(ctx context.Context, f protocol.Frame) {
	fwd, ok := b.lookupHostChannel(f.Channel)
	if !ok {
		// The guest closed the stream while the host was still writing. There
		// is nobody to deliver to and nothing to report.
		return
	}

	// Copy before queueing: the transport owns the payload only until the next
	// Recv, and this is about to cross a goroutine.
	out := onChannel(f, fwd.guestChan)
	out.Payload = append([]byte(nil), f.Payload...)

	closing := f.Type == protocol.TypeClose || f.Type == protocol.TypeError

	if !fwd.queue(out) && !closing {
		// Flow control should have prevented this: the host cannot send more
		// than the guest granted. Reaching here means a peer ignored the
		// window, so close the one stream rather than drop bytes into it —
		// and say so to both ends, because a forwarded connection that simply
		// stops is indistinguishable from one that is merely idle.
		b.log.Warn("forward exceeded its window; closing it",
			slog.String("session_id", b.id),
			slog.Uint64("channel", uint64(f.Channel)))
		b.retireForward(ctx, f.Channel, "the forward overran its window")
		return
	}

	if closing {
		b.closeForward(f.Channel)
	}
}
