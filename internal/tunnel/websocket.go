package tunnel

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
)

// WebSocket transport notes:
//
// The message boundary is the frame boundary, so no length prefix is needed.
// The binary/text split carries the protocol's own split: DATA frames go in
// binary messages, control frames in text. That means a packet capture shows
// readable JSON for control traffic and opaque bytes for terminal data, which
// is exactly the debugging experience we want.
//
// coder/websocket is used rather than gorilla: it has no dependencies of its
// own and its API is context-native, so every read and write honours the
// caller's deadline without a separate SetReadDeadline dance.

// readLimit bounds a single incoming message. It must exceed MaxPayloadLen by
// the header size, plus room for a control envelope's JSON overhead.
const readLimit = protocol.MaxPayloadLen + 4096

// DialOptions configures Dial.
type DialOptions struct {
	// HTTPClient issues the upgrade request. Nil uses the default client.
	HTTPClient *http.Client
	// Header carries extra HTTP headers on the upgrade request.
	Header http.Header
}

// Dial opens a tunnel to a relay. rawURL may use http, https, ws or wss; the
// http forms are rewritten, so callers can pass the same base URL they use for
// the REST API.
func Dial(ctx context.Context, rawURL string, opts DialOptions) (Conn, error) {
	u, err := normalizeWebSocketURL(rawURL)
	if err != nil {
		return nil, err
	}

	c, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{
		HTTPClient: opts.HTTPClient,
		HTTPHeader: opts.Header,
	})
	if err != nil {
		return nil, fmt.Errorf("tunnel: dial %s: %w", u, err)
	}
	return newWSConn(c), nil
}

// Accept upgrades an inbound HTTP request into a tunnel.
//
// Origin checking is left to the caller. The relay does not rely on browser
// origin for authorization — every tunnel must authenticate with an OPEN frame
// carrying a session token — so a rejected origin would add no security while
// breaking non-browser clients.
func Accept(w http.ResponseWriter, r *http.Request) (Conn, error) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, fmt.Errorf("tunnel: accept: %w", err)
	}
	return newWSConn(c), nil
}

// normalizeWebSocketURL turns a relay base URL into a tunnel endpoint URL.
func normalizeWebSocketURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("tunnel: invalid url %q: %w", raw, err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("tunnel: unsupported scheme %q in %q", u.Scheme, raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("tunnel: missing host in %q", raw)
	}
	return u.String(), nil
}

// wsConn adapts a WebSocket to Conn.
type wsConn struct {
	c *websocket.Conn

	// writeMu serialises writes. A WebSocket permits only one writer at a
	// time, and the relay writes terminal output and keepalive pings from
	// different goroutines.
	writeMu sync.Mutex

	closeOnce sync.Once
	closeErr  error
}

func newWSConn(c *websocket.Conn) *wsConn {
	c.SetReadLimit(readLimit)
	return &wsConn{c: c}
}

func (w *wsConn) Send(ctx context.Context, f protocol.Frame) error {
	var (
		msgType websocket.MessageType
		b       []byte
		err     error
	)
	if f.Type == protocol.TypeData {
		msgType = websocket.MessageBinary
		b, err = protocol.MarshalBinary(f)
	} else {
		msgType = websocket.MessageText
		b, err = protocol.MarshalControl(f)
	}
	if err != nil {
		return err
	}

	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	if err := w.c.Write(ctx, msgType, b); err != nil {
		return fmt.Errorf("tunnel: send %s: %w", f.Type, translateClose(err))
	}
	return nil
}

func (w *wsConn) Recv(ctx context.Context) (protocol.Frame, error) {
	msgType, b, err := w.c.Read(ctx)
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("tunnel: recv: %w", translateClose(err))
	}
	switch msgType {
	case websocket.MessageBinary:
		return protocol.UnmarshalBinary(b)
	case websocket.MessageText:
		return protocol.UnmarshalControl(b)
	default:
		return protocol.Frame{}, fmt.Errorf("%w: unexpected websocket message type", protocol.ErrInvalidFrame)
	}
}

// closeGrace is how long Close waits for the peer to answer the close
// handshake.
//
// The library's own Close writes the close frame and then waits five seconds
// for a reply. That reply only arrives if the peer is still reading, and by the
// time either side closes, its read pump has usually already exited — so the
// common case would be a five second stall on every teardown. The protocol has
// its own CLOSE frame for orderly shutdown, so the WebSocket handshake is a
// courtesy, not a requirement: send it, wait briefly, move on.
const closeGrace = time.Second

func (w *wsConn) Close(reason string) error {
	w.closeOnce.Do(func() {
		done := make(chan error, 1)
		go func() {
			// The reason is capped at 125 bytes by RFC 6455; an oversized one
			// makes the library refuse to send the close frame at all.
			done <- w.c.Close(websocket.StatusNormalClosure, truncateReason(reason))
		}()

		select {
		case err := <-done:
			w.closeErr = err
		case <-time.After(closeGrace):
			// The peer is not completing the handshake. The library tears the
			// connection down on its own timeout; there is nothing to report
			// and no reason to keep the caller waiting for it.
		}
	})
	return w.closeErr
}

// truncateReason trims a close reason to the protocol's limit.
func truncateReason(s string) string {
	const maxCloseReason = 123
	if len(s) <= maxCloseReason {
		return s
	}
	return s[:maxCloseReason-3] + "..."
}

// translateClose maps an orderly peer close onto ErrClosed so callers can
// distinguish "the other side hung up" from a real transport failure.
func translateClose(err error) error {
	var ce websocket.CloseError
	if ok := asCloseError(err, &ce); ok {
		switch ce.Code {
		case websocket.StatusNormalClosure, websocket.StatusGoingAway:
			return ErrClosed
		}
	}
	return err
}

// keepaliveInterval and keepaliveTimeout drive protocol-level liveness.
//
// These live at the protocol level rather than using WebSocket ping frames so
// that a future transport without its own ping gets liveness for free.
const (
	KeepaliveInterval = 30 * time.Second
	KeepaliveTimeout  = 90 * time.Second
)
