package client

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
)

// The host end of TCP forwarding: a guest asks for a connection, and if the
// allowlist permits it the host dials and pipes bytes both ways.
//
// The relay never dials anything. It brokers frames, exactly as it does for the
// terminal, so the only machine that reaches a forward target is the one whose
// owner opted in.

const (
	// dialTimeout bounds how long a guest can make the host wait on a target
	// that is not answering.
	dialTimeout = 10 * time.Second
	// forwardBufferSize is the read size on a forwarded socket. Bulk transfer
	// wants more than a terminal does.
	forwardBufferSize = 32 << 10
)

// hostForwards tracks the sockets this host has open on guests' behalf.
type hostForwards struct {
	allow Allowlist
	send  func(protocol.Frame)
	log   *slog.Logger

	mu    sync.Mutex
	conns map[protocol.ChannelID]*hostConn
}

// hostConn is one dialled socket and the credit it may still send.
type hostConn struct {
	conn net.Conn
	// out is what the guest will still accept from us. Reading from the
	// target faster than the guest drains it is what used to force a reset.
	out *window
}

func newHostForwards(allow Allowlist, send func(protocol.Frame), log *slog.Logger) *hostForwards {
	if log == nil {
		log = slog.Default()
	}
	return &hostForwards{
		allow: allow,
		send:  send,
		log:   log,
		conns: make(map[protocol.ChannelID]*hostConn),
	}
}

// handle routes one frame that arrived on a forwarding channel.
func (h *hostForwards) handle(ctx context.Context, f protocol.Frame) {
	switch f.Type {
	case protocol.TypeOpen:
		go h.openChannel(ctx, f)
	case protocol.TypeData:
		h.write(f)
	case protocol.TypeWindow:
		h.grant(f)
	case protocol.TypeClose, protocol.TypeError:
		h.close(f.Channel)
	}
}

// grant credits this channel with room the guest has freed up.
func (h *hostForwards) grant(f protocol.Frame) {
	var win protocol.Window
	if err := protocol.DecodeControl(f, &win); err != nil {
		return
	}
	h.mu.Lock()
	hc, ok := h.conns[f.Channel]
	h.mu.Unlock()
	if ok {
		hc.out.grant(win.Bytes)
	}
}

// openChannel dials a target for a guest, if the host allowed it.
func (h *hostForwards) openChannel(ctx context.Context, f protocol.Frame) {
	ch := f.Channel

	var req protocol.ChannelOpen
	if err := protocol.DecodeControl(f, &req); err != nil {
		h.refuse(ch, protocol.ErrCodeProtocol, "malformed channel open")
		return
	}
	if err := req.Validate(); err != nil {
		h.refuse(ch, protocol.ErrCodeProtocol, "invalid channel open")
		return
	}

	if !h.allow.Allows(req.Host, req.Port) {
		// Say which target was refused, not which are permitted: the guest
		// asked for this one, and listing the rest would hand them a map of
		// the host's network.
		h.log.Warn("refused a forward",
			slog.String("target", req.Target()),
			slog.String("allowed", h.allow.String()))
		h.refuse(ch, protocol.ErrCodeForwardDenied, "this host is not forwarding to "+req.Target())
		return
	}

	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	// A plain dial to a literal address. Nothing here reaches a shell, and the
	// address is checked against the allowlist before we get this far.
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", req.Target())
	if err != nil {
		h.log.Info("forward dial failed",
			slog.String("target", req.Target()),
			slog.Any("error", err))
		h.refuse(ch, protocol.ErrCodeForwardFailed, "could not reach "+req.Target())
		return
	}

	hc := &hostConn{conn: conn, out: newWindow(protocol.InitialWindow)}

	h.mu.Lock()
	if _, exists := h.conns[ch]; exists {
		h.mu.Unlock()
		conn.Close()
		h.refuse(ch, protocol.ErrCodeProtocol, "channel already open")
		return
	}
	h.conns[ch] = hc
	h.mu.Unlock()

	h.log.Info("forward opened",
		slog.String("target", req.Target()),
		slog.Uint64("channel", uint64(ch)))

	// Tell the guest the connection is live before any bytes flow.
	if fr, err := protocol.NewControl(protocol.TypeOpen, protocol.ChannelOpen{
		Kind: protocol.ChannelKindTCP, Host: req.Host, Port: req.Port,
	}); err == nil {
		fr.Channel = ch
		h.send(fr)
	}

	go h.pump(ctx, ch, hc)
}

// pump copies from the target back to the guest until the socket ends.
func (h *hostForwards) pump(ctx context.Context, ch protocol.ChannelID, hc *hostConn) {
	buf := make([]byte, forwardBufferSize)
	for {
		n, err := hc.conn.Read(buf)
		if n > 0 {
			// Wait for room before sending. This is what turns "read as fast
			// as the kernel allows and hope" into a stream that keeps pace
			// with whoever is reading it.
			if werr := hc.out.reserve(ctx, n); werr != nil {
				h.closeWithReason(ch, "the forwarded connection ended")
				return
			}
			// Copy: the frame outlives this iteration once queued.
			h.send(protocol.Frame{
				Type:    protocol.TypeData,
				Channel: ch,
				Payload: append([]byte(nil), buf[:n]...),
			})
		}
		if err != nil {
			reason := "target closed the connection"
			if err != io.EOF {
				reason = "the forwarded connection ended"
			}
			h.closeWithReason(ch, reason)
			return
		}
	}
}

// write delivers guest bytes to the target.
func (h *hostForwards) write(f protocol.Frame) {
	h.mu.Lock()
	hc, ok := h.conns[f.Channel]
	h.mu.Unlock()
	if !ok {
		// Both ends can close at once, so data after a close is ordinary.
		return
	}
	if _, err := hc.conn.Write(f.Payload); err != nil {
		h.closeWithReason(f.Channel, "could not write to the target")
		return
	}
	// Those bytes are spent, so tell the guest it may send that much more.
	// Crediting only after the write is what makes the window reflect the
	// target's pace rather than ours.
	h.creditGuest(f.Channel, len(f.Payload))
}

// creditGuest returns window space to the guest.
func (h *hostForwards) creditGuest(ch protocol.ChannelID, n int) {
	if n <= 0 {
		return
	}
	if fr, err := protocol.NewControl(protocol.TypeWindow, protocol.Window{Bytes: n}); err == nil {
		fr.Channel = ch
		h.send(fr)
	}
}

// close drops a forward without telling the guest, used when the guest is the
// one who closed it.
func (h *hostForwards) close(ch protocol.ChannelID) {
	h.mu.Lock()
	hc, ok := h.conns[ch]
	delete(h.conns, ch)
	h.mu.Unlock()
	if ok {
		hc.out.close()
		hc.conn.Close()
	}
}

// closeWithReason drops a forward and tells the guest why.
func (h *hostForwards) closeWithReason(ch protocol.ChannelID, reason string) {
	h.mu.Lock()
	hc, ok := h.conns[ch]
	delete(h.conns, ch)
	h.mu.Unlock()
	if !ok {
		return
	}
	hc.out.close()
	hc.conn.Close()

	if fr, err := protocol.NewControl(protocol.TypeClose, protocol.Close{Reason: reason}); err == nil {
		fr.Channel = ch
		h.send(fr)
	}
}

// refuse tells the guest a channel could not be opened.
func (h *hostForwards) refuse(ch protocol.ChannelID, code, msg string) {
	fr, err := protocol.NewControl(protocol.TypeError, protocol.Error{Code: code, Message: msg})
	if err != nil {
		return
	}
	fr.Channel = ch
	h.send(fr)
}

// closeAll drops every forward, used when the session ends.
func (h *hostForwards) closeAll() {
	h.mu.Lock()
	live := make([]*hostConn, 0, len(h.conns))
	for ch, c := range h.conns {
		live = append(live, c)
		delete(h.conns, ch)
	}
	h.mu.Unlock()

	for _, c := range live {
		c.out.close()
		c.conn.Close()
	}
}

// count reports how many forwards are open, for tests.
func (h *hostForwards) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns)
}
