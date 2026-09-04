// Package tunnel moves protocol frames between two endpoints.
//
// It is the only package that knows a transport exists. Everything above it —
// the terminal, the session bridge, the CLI — works in terms of Conn, so
// replacing WebSockets with QUIC, an SSH channel or a raw socket means adding a
// Conn implementation here and changing nothing else.
package tunnel

import (
	"context"
	"errors"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
)

// ErrClosed is returned by Send or Recv on a closed connection.
var ErrClosed = errors.New("tunnel: connection closed")

// Conn is a bidirectional, frame-oriented connection.
//
// Send must be safe for concurrent use, and implementations are responsible
// for serialising the writes themselves. This is not incidental: a relay fans
// terminal output in from every guest at once, and a guest answers a PING from
// its read pump while its write pump is mid-frame. Pushing that coordination
// out to callers would mean every one of them reinventing the same mutex, and
// forgetting it would corrupt the stream rather than fail loudly.
//
// Recv must be called from a single goroutine, which is the natural shape of a
// read pump. It is safe concurrently with Send and Close.
type Conn interface {
	// Send transmits one frame. The frame's payload is not retained.
	// Safe for concurrent use.
	Send(ctx context.Context, f protocol.Frame) error

	// Recv blocks for the next frame. Call it from one goroutine only. The
	// returned payload is owned by the caller and stays valid until the
	// following Recv.
	Recv(ctx context.Context) (protocol.Frame, error)

	// Close shuts the connection down, reporting reason to the peer where the
	// transport supports it. Close is idempotent.
	Close(reason string) error
}

// SendControl marshals v into a control frame of type t and sends it.
func SendControl(ctx context.Context, c Conn, t protocol.Type, v any) error {
	f, err := protocol.NewControl(t, v)
	if err != nil {
		return err
	}
	return c.Send(ctx, f)
}

// SendError reports a fatal condition to the peer on a best-effort basis. The
// error is deliberately swallowed: the caller is already on a failure path and
// is about to close.
func SendError(ctx context.Context, c Conn, code, msg string) {
	_ = SendControl(ctx, c, protocol.TypeError, protocol.Error{Code: code, Message: msg})
}
