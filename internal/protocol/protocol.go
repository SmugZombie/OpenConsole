// Package protocol defines the OpenConsole wire protocol: the message types
// exchanged between a host CLI, the relay server, and a joining guest.
//
// The protocol is deliberately transport-agnostic. Frames are described here in
// terms of a type, an optional channel identifier and a payload; how those
// frames reach the other side (WebSocket today, potentially QUIC or a raw TCP
// stream later) is the responsibility of the tunnel layer. Nothing in this
// package imports a transport.
//
// Two encodings coexist by design:
//
//   - Terminal payloads (DATA) are carried as raw bytes. Terminal traffic is
//     already a byte stream and is latency sensitive, so wrapping it in JSON and
//     base64 would cost roughly 33% bandwidth plus an encode/decode step on
//     every keystroke.
//   - Control messages (OPEN, RESIZE, ERROR, ...) are carried as JSON. They are
//     infrequent, and a self-describing encoding makes the protocol far easier
//     to extend and debug.
//
// See docs/protocol.md for the framing rules and the reasoning behind them.
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Version is the protocol version advertised in an OPEN message. It is bumped
// whenever a change is not backwards compatible.
const Version = 1

// Type identifies the kind of a frame. Values are stable across versions; new
// types are appended, existing values are never reused.
type Type uint8

const (
	// TypeOpen initiates a logical stream. Sent by a host registering its
	// tunnel, or by a guest attaching to a session.
	TypeOpen Type = 1
	// TypeData carries opaque terminal bytes in either direction.
	TypeData Type = 2
	// TypeResize reports a new terminal window size.
	TypeResize Type = 3
	// TypePing is a liveness probe. The peer must answer with TypePong.
	TypePing Type = 4
	// TypePong answers a TypePing, echoing its payload.
	TypePong Type = 5
	// TypeClose signals an orderly shutdown of the stream.
	TypeClose Type = 6
	// TypeError reports a fatal condition; the sender may close afterwards.
	TypeError Type = 7
)

// String implements fmt.Stringer so log output stays readable.
func (t Type) String() string {
	switch t {
	case TypeOpen:
		return "OPEN"
	case TypeData:
		return "DATA"
	case TypeResize:
		return "RESIZE"
	case TypePing:
		return "PING"
	case TypePong:
		return "PONG"
	case TypeClose:
		return "CLOSE"
	case TypeError:
		return "ERROR"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", uint8(t))
	}
}

// Valid reports whether t is a type defined by this protocol version.
func (t Type) Valid() bool {
	return t >= TypeOpen && t <= TypeError
}

// ChannelID identifies a logical stream inside one tunnel.
//
// Multiplexing is NOT implemented yet: every frame currently uses
// ChannelControl. The field exists in the frame header from day one so that
// adding channels later (for TCP forwarding, file transfer, a second terminal)
// does not require a wire-format break.
type ChannelID uint32

// ChannelControl is the implicit channel used before multiplexing exists.
const ChannelControl ChannelID = 0

// Frame is the unit of transfer. Exactly one of the two payload
// interpretations applies, selected by Type:
//
//	TypeData -> Payload is raw terminal bytes.
//	otherwise -> Payload is a JSON-encoded control message.
type Frame struct {
	Type    Type
	Channel ChannelID
	Payload []byte
}

// Role distinguishes the two kinds of participant in a session.
type Role string

const (
	// RoleHost is the machine sharing its terminal.
	RoleHost Role = "host"
	// RoleGuest is a participant attaching to a shared terminal.
	RoleGuest Role = "guest"
	// RoleViewer may watch a terminal but not type into it.
	//
	// A client sends this to ask for read-only access even when it holds a
	// credential that would allow more — useful for looking over someone's
	// shoulder without the risk of a stray keystroke. What a connection
	// actually gets is decided by the relay from the token, and reported back
	// in the OPEN acknowledgement.
	RoleViewer Role = "viewer"
)

// Open is the JSON body of a TypeOpen frame.
//
// Token is a bearer credential. It is deliberately carried in the payload
// rather than in a URL so it does not end up in server access logs, browser
// history or Referer headers.
type Open struct {
	Version   int    `json:"version"`
	SessionID string `json:"session_id"`
	Role      Role   `json:"role"`
	Token     string `json:"token"`
	// Cols and Rows are the initial terminal size, if known.
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
}

// Resize is the JSON body of a TypeResize frame.
type Resize struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// Close is the JSON body of a TypeClose frame.
type Close struct {
	Reason string `json:"reason,omitempty"`
	// ExitCode is the shell's exit status when the host terminal ended.
	ExitCode *int `json:"exit_code,omitempty"`
}

// Error is the JSON body of a TypeError frame. Code is a short stable token
// meant for programmatic handling; Message is human readable and must never
// contain a credential.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// Well-known error codes.
const (
	ErrCodeUnauthorized     = "unauthorized"
	ErrCodeSessionNotFound  = "session_not_found"
	ErrCodeSessionExpired   = "session_expired"
	ErrCodeProtocol         = "protocol_error"
	ErrCodeInternal         = "internal_error"
	ErrCodeVersionUnsupport = "unsupported_version"
)

// ErrInvalidFrame is returned when a frame cannot be interpreted.
var ErrInvalidFrame = errors.New("protocol: invalid frame")

// NewData builds a DATA frame over the control channel. The payload is not
// copied; callers must not retain the slice after sending.
func NewData(b []byte) Frame {
	return Frame{Type: TypeData, Channel: ChannelControl, Payload: b}
}

// NewControl marshals v as the JSON payload of a control frame of type t.
// Passing TypeData is a programming error and returns ErrInvalidFrame.
func NewControl(t Type, v any) (Frame, error) {
	if t == TypeData {
		return Frame{}, fmt.Errorf("%w: DATA frames carry raw bytes, not JSON", ErrInvalidFrame)
	}
	if !t.Valid() {
		return Frame{}, fmt.Errorf("%w: unknown type %d", ErrInvalidFrame, uint8(t))
	}
	payload, err := json.Marshal(v)
	if err != nil {
		return Frame{}, fmt.Errorf("protocol: marshal %s: %w", t, err)
	}
	return Frame{Type: t, Channel: ChannelControl, Payload: payload}, nil
}

// DecodeControl unmarshals a control frame's JSON payload into v.
func DecodeControl(f Frame, v any) error {
	if f.Type == TypeData {
		return fmt.Errorf("%w: DATA frames carry raw bytes, not JSON", ErrInvalidFrame)
	}
	if err := json.Unmarshal(f.Payload, v); err != nil {
		return fmt.Errorf("protocol: unmarshal %s: %w", f.Type, err)
	}
	return nil
}
