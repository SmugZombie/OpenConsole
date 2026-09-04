package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
	"github.com/SmugZombie/OpenConsole/internal/tunnel"
)

// openTimeout bounds the dial plus handshake.
const openTimeout = 20 * time.Second

// openTunnel dials the relay and completes the OPEN handshake, returning a
// connection that is authenticated and attached to a session, along with the
// access the relay granted.
//
// The granted role comes from the relay's acknowledgement rather than what was
// asked for: a viewer ticket presented as an ordinary join is accepted and
// answered with "viewer", so the client learns what it may do instead of
// finding out by having its keystrokes ignored.
//
// The caller closes the returned connection.
func openTunnel(ctx context.Context, url string, open protocol.Open) (tunnel.Conn, protocol.Open, error) {
	dialCtx, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()

	conn, err := tunnel.Dial(dialCtx, url, tunnel.DialOptions{})
	if err != nil {
		return nil, protocol.Open{}, err
	}

	open.Version = protocol.Version
	if err := tunnel.SendControl(dialCtx, conn, protocol.TypeOpen, open); err != nil {
		conn.Close("handshake failed")
		return nil, protocol.Open{}, fmt.Errorf("sending OPEN: %w", err)
	}

	// The relay answers with OPEN to confirm the attachment, or ERROR to
	// refuse it. Waiting for that turns "connected" into "actually attached",
	// so failures surface here instead of as silence later.
	f, err := conn.Recv(dialCtx)
	if err != nil {
		conn.Close("handshake failed")
		return nil, protocol.Open{}, fmt.Errorf("waiting for relay: %w", err)
	}

	switch f.Type {
	case protocol.TypeOpen:
		var ack protocol.Open
		if err := protocol.DecodeControl(f, &ack); err != nil {
			conn.Close("protocol error")
			return nil, protocol.Open{}, fmt.Errorf("malformed acknowledgement: %w", err)
		}
		if ack.Role == "" {
			// A relay from before roles were reported back. Assume the
			// narrower capability rather than the wider one.
			ack.Role = protocol.RoleViewer
		}
		return conn, ack, nil
	case protocol.TypeError:
		var e protocol.Error
		_ = protocol.DecodeControl(f, &e)
		conn.Close("rejected")
		return nil, protocol.Open{}, relayRefusal(e)
	default:
		conn.Close("protocol error")
		return nil, protocol.Open{}, fmt.Errorf("relay answered OPEN with %s", f.Type)
	}
}

// relayRefusal turns a protocol ERROR into a message a user can act on.
func relayRefusal(e protocol.Error) error {
	switch e.Code {
	case protocol.ErrCodeUnauthorized:
		return errors.New("relay rejected the session or token (it may have expired)")
	case protocol.ErrCodeSessionNotFound:
		return errors.New("no live terminal for that session (the host may have disconnected)")
	case protocol.ErrCodeVersionUnsupport:
		return errors.New("relay speaks a different protocol version; upgrade the client or the relay")
	case "":
		return errors.New("relay refused the connection")
	default:
		if e.Message != "" {
			return fmt.Errorf("relay refused the connection: %s (%s)", e.Message, e.Code)
		}
		return fmt.Errorf("relay refused the connection: %s", e.Code)
	}
}
