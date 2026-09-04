package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
	"github.com/SmugZombie/OpenConsole/internal/session"
	"github.com/SmugZombie/OpenConsole/internal/tunnel"
)

// handshakeTimeout bounds how long a connection may stay unauthenticated.
// Without it, opening sockets and never sending OPEN would be a cheap way to
// pin server resources.
const handshakeTimeout = 10 * time.Second

// handleTunnel upgrades a connection and joins it to a session.
//
// The session ID and token arrive in the OPEN frame rather than the URL, so
// credentials never reach access logs, proxy logs or browser history. That is
// why this is a single endpoint for both roles instead of a path per session.
func (a *API) handleTunnel(w http.ResponseWriter, r *http.Request) {
	conn, err := tunnel.Accept(w, r)
	if err != nil {
		// Accept has already written an HTTP error response.
		a.log.Debug("websocket upgrade failed", slog.Any("error", err))
		return
	}
	// The request context ends when the handler returns, which would kill a
	// long-lived tunnel; derive the connection's lifetime from the server's
	// base context instead.
	ctx, cancel := context.WithCancel(a.baseCtx)
	defer cancel()
	defer conn.Close("done")

	open, sess, access, err := a.authenticate(ctx, conn)
	if err != nil {
		a.log.Info("tunnel handshake rejected",
			slog.String("remote", remoteHost(r.RemoteAddr)),
			slog.Any("error", err))
		return
	}

	if access == session.AccessHost {
		a.serveHostTunnel(ctx, conn, sess, open)
		return
	}
	a.serveGuestTunnel(ctx, conn, sess, access)
}

// authenticate reads and validates the opening frame.
//
// It deliberately does not acknowledge: a caller still has to attach to a
// bridge, and that can fail. The OPEN ack is sent by the role handlers once the
// connection is genuinely attached, so a client that receives it knows it is
// live rather than merely authenticated.
func (a *API) authenticate(ctx context.Context, conn tunnel.Conn) (protocol.Open, *session.Session, session.Access, error) {
	hsCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	f, err := conn.Recv(hsCtx)
	if err != nil {
		return protocol.Open{}, nil, "", err
	}
	if f.Type != protocol.TypeOpen {
		tunnel.SendError(hsCtx, conn, protocol.ErrCodeProtocol, "expected OPEN as the first frame")
		return protocol.Open{}, nil, "", errors.New("first frame was " + f.Type.String())
	}

	var open protocol.Open
	if err := protocol.DecodeControl(f, &open); err != nil {
		tunnel.SendError(hsCtx, conn, protocol.ErrCodeProtocol, "malformed OPEN")
		return protocol.Open{}, nil, "", err
	}
	if open.Version != protocol.Version {
		tunnel.SendError(hsCtx, conn, protocol.ErrCodeVersionUnsupport, "unsupported protocol version")
		return protocol.Open{}, nil, "", errors.New("unsupported protocol version")
	}
	switch open.Role {
	case protocol.RoleHost, protocol.RoleGuest, protocol.RoleViewer:
	default:
		tunnel.SendError(hsCtx, conn, protocol.ErrCodeProtocol, "unknown role")
		return protocol.Open{}, nil, "", errors.New("unknown role")
	}

	sess, access, err := a.sessions.Authenticate(hsCtx, open.SessionID, open.Role, open.Token)
	if err != nil {
		// A bad token and an unknown session are reported identically, so a
		// caller cannot use this to discover which session IDs exist.
		tunnel.SendError(hsCtx, conn, protocol.ErrCodeUnauthorized, "invalid session or token")
		return protocol.Open{}, nil, "", err
	}

	return open, sess, access, nil
}

// ack confirms attachment. The echoed OPEN carries no credential.
//
// Its Role is the access the relay actually granted, not what the client asked
// for, so a client learns from this whether it may type.
func ack(ctx context.Context, conn tunnel.Conn, sess *session.Session, access session.Access) error {
	return tunnel.SendControl(ctx, conn, protocol.TypeOpen, protocol.Open{
		Version:   protocol.Version,
		SessionID: sess.SessionID,
		Role:      access.Role(),
	})
}

// serveHostTunnel attaches a host terminal to its session.
func (a *API) serveHostTunnel(ctx context.Context, conn tunnel.Conn, sess *session.Session, open protocol.Open) {
	bridge, err := a.bridges.Open(sess.SessionID)
	if err != nil {
		tunnel.SendError(ctx, conn, protocol.ErrCodeProtocol, "a host is already attached to this session")
		return
	}

	// The session record outlives the terminal only to serve lookups; once the
	// host disconnects there is nothing to join, so drop it immediately rather
	// than waiting for the TTL.
	defer a.sessions.Delete(sess.SessionID)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := ack(ctx, conn, sess, session.AccessHost); err != nil {
		bridge.Close("host handshake failed")
		return
	}
	go a.keepalive(ctx, conn)

	err = bridge.ServeHost(ctx, conn, open.Cols, open.Rows)
	if err != nil && !isDisconnect(err) {
		a.log.Info("host tunnel ended",
			slog.String("session_id", sess.SessionID),
			slog.Any("error", err))
	}
}

// serveGuestTunnel attaches a guest to a session's live terminal.
func (a *API) serveGuestTunnel(ctx context.Context, conn tunnel.Conn, sess *session.Session, access session.Access) {
	bridge, ok := a.bridges.Get(sess.SessionID)
	if !ok {
		tunnel.SendError(ctx, conn, protocol.ErrCodeSessionNotFound, "the host is not connected")
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := ack(ctx, conn, sess, access); err != nil {
		return
	}
	go a.keepalive(ctx, conn)

	err := bridge.ServeGuest(ctx, conn, session.GuestOptions{Access: access})
	if errors.Is(err, session.ErrNoHost) {
		tunnel.SendError(ctx, conn, protocol.ErrCodeSessionNotFound, "the host is not connected")
		return
	}
	if err != nil && !isDisconnect(err) {
		a.log.Info("guest tunnel ended",
			slog.String("session_id", sess.SessionID),
			slog.Any("error", err))
	}
}

// keepalive sends periodic PINGs so a connection dropped by a NAT or a proxy is
// noticed rather than lingering as a half-open socket holding a session.
//
// PING is a protocol frame rather than a WebSocket ping so that any future
// transport inherits liveness without implementing its own.
func (a *API) keepalive(ctx context.Context, conn tunnel.Conn) {
	t := time.NewTicker(tunnel.KeepaliveInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sendCtx, cancel := context.WithTimeout(ctx, tunnel.KeepaliveTimeout)
			err := conn.Send(sendCtx, protocol.Frame{Type: protocol.TypePing})
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// isDisconnect reports whether err is an ordinary end of connection rather than
// a fault worth logging.
func isDisconnect(err error) bool {
	return errors.Is(err, tunnel.ErrClosed) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, session.ErrBridgeClosed)
}
