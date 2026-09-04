package client

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
)

// The guest end of TCP forwarding: listen locally, and turn each accepted
// connection into a channel on the tunnel the terminal is already using.
//
// Channel numbers here are the guest's own. The relay translates them, so two
// guests numbering from 1 never collide.

// guestForwards owns the local listeners and the sockets behind them.
type guestForwards struct {
	send   func(protocol.Frame)
	notify func(string)

	mu       sync.Mutex
	conns    map[protocol.ChannelID]*guestForward
	nextChan protocol.ChannelID
	closed   bool

	listeners []net.Listener
	wg        sync.WaitGroup
}

// guestForward is one accepted local connection waiting on, or bound to, a
// channel.
type guestForward struct {
	conn net.Conn
	// ready is closed once the host has accepted or refused the channel, so
	// no local bytes are sent into a stream that may not exist.
	ready  chan error
	once   sync.Once
	closed bool
}

func newGuestForwards(send func(protocol.Frame), notify func(string)) *guestForwards {
	if notify == nil {
		notify = func(string) {}
	}
	return &guestForwards{
		send:   send,
		notify: notify,
		conns:  make(map[protocol.ChannelID]*guestForward),
	}
}

// Listen starts a local listener for one -L spec and reports the address it
// actually bound, which is what the user should be told: a spec may name port
// 0, and the resolved address is the one they can connect to.
func (g *guestForwards) Listen(ctx context.Context, spec ForwardSpec) (string, error) {
	ln, err := net.Listen("tcp", spec.ListenAddr)
	if err != nil {
		return "", fmt.Errorf("cannot listen on %s: %w", spec.ListenAddr, err)
	}

	g.mu.Lock()
	g.listeners = append(g.listeners, ln)
	g.mu.Unlock()

	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // the listener was closed
			}
			g.wg.Add(1)
			go func() {
				defer g.wg.Done()
				g.forward(ctx, conn, spec)
			}()
		}
	}()
	return ln.Addr().String(), nil
}

// forward carries one local connection over a channel.
func (g *guestForwards) forward(ctx context.Context, conn net.Conn, spec ForwardSpec) {
	ch, fwd, err := g.register(conn)
	if err != nil {
		conn.Close()
		return
	}
	defer g.closeChannel(ch, true)

	req, err := protocol.NewControl(protocol.TypeOpen, protocol.ChannelOpen{
		Kind: protocol.ChannelKindTCP,
		Host: spec.RemoteHost,
		Port: spec.RemotePort,
	})
	if err != nil {
		return
	}
	req.Channel = ch
	g.send(req)

	// Wait for the host before sending anything: bytes written into a channel
	// the host refused would be silently discarded, and the person at the
	// other end of the local socket would see a connection that accepted their
	// request and then did nothing.
	select {
	case err := <-fwd.ready:
		if err != nil {
			g.notify(err.Error())
			return
		}
	case <-ctx.Done():
		return
	}

	buf := make([]byte, forwardBufferSize)
	for {
		n, rerr := conn.Read(buf)
		if n > 0 {
			g.send(protocol.Frame{
				Type:    protocol.TypeData,
				Channel: ch,
				Payload: append([]byte(nil), buf[:n]...),
			})
		}
		if rerr != nil {
			if rerr != io.EOF {
				return
			}
			return
		}
	}
}

// register allocates a channel for a new local connection.
func (g *guestForwards) register(conn net.Conn) (protocol.ChannelID, *guestForward, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return 0, nil, io.ErrClosedPipe
	}
	if len(g.conns) >= protocol.MaxChannels {
		return 0, nil, fmt.Errorf("too many forwarded connections")
	}

	// Never reused, so a late frame from a closed stream cannot land on a new
	// connection.
	g.nextChan++
	ch := g.nextChan

	fwd := &guestForward{conn: conn, ready: make(chan error, 1)}
	g.conns[ch] = fwd
	return ch, fwd, nil
}

// handle routes one frame that arrived on a forwarding channel.
func (g *guestForwards) handle(f protocol.Frame) {
	g.mu.Lock()
	fwd, ok := g.conns[f.Channel]
	g.mu.Unlock()
	if !ok {
		return
	}

	switch f.Type {
	case protocol.TypeOpen:
		// The host connected. Release the reader.
		fwd.once.Do(func() { fwd.ready <- nil })

	case protocol.TypeData:
		if _, err := fwd.conn.Write(f.Payload); err != nil {
			g.closeChannel(f.Channel, true)
		}

	case protocol.TypeClose:
		var c protocol.Close
		_ = protocol.DecodeControl(f, &c)
		fwd.once.Do(func() { fwd.ready <- nil })
		g.closeChannel(f.Channel, false)

	case protocol.TypeError:
		var e protocol.Error
		_ = protocol.DecodeControl(f, &e)
		msg := e.Message
		if msg == "" {
			msg = e.Code
		}
		fwd.once.Do(func() { fwd.ready <- fmt.Errorf("forward refused: %s", msg) })
		g.closeChannel(f.Channel, false)
	}
}

// closeChannel drops a forward. tellHost is false when the host is the one who
// closed it.
func (g *guestForwards) closeChannel(ch protocol.ChannelID, tellHost bool) {
	g.mu.Lock()
	fwd, ok := g.conns[ch]
	if ok {
		delete(g.conns, ch)
	}
	g.mu.Unlock()
	if !ok {
		return
	}

	fwd.conn.Close()
	fwd.once.Do(func() { fwd.ready <- nil })

	if tellHost {
		if fr, err := protocol.NewControl(protocol.TypeClose, protocol.Close{}); err == nil {
			fr.Channel = ch
			g.send(fr)
		}
	}
}

// Close stops listening and drops every forwarded connection.
func (g *guestForwards) Close() {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.closed = true
	listeners := g.listeners
	g.listeners = nil
	live := make([]net.Conn, 0, len(g.conns))
	for ch, f := range g.conns {
		live = append(live, f.conn)
		delete(g.conns, ch)
	}
	g.mu.Unlock()

	for _, ln := range listeners {
		ln.Close()
	}
	for _, c := range live {
		c.Close()
	}
	g.wg.Wait()
}

// count reports how many forwarded connections are live, for tests.
func (g *guestForwards) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.conns)
}
