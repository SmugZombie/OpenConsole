package protocol

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
)

// The wire format. Two encodings, selected by frame type:
//
//	DATA     -> binary: [type:uint8][channel:uint32 BE][payload...]
//	otherwise -> JSON envelope: {"type":"OPEN","channel":0,"payload":{...}}
//
// A transport that distinguishes binary from textual messages (WebSocket does)
// can carry them directly. A raw byte-stream transport would additionally need
// a length prefix; that is the transport's job, not the protocol's.
//
// The 5-byte binary header is deliberate overhead. Channel IDs are not used
// yet, but a DATA frame that cannot say which channel it belongs to would make
// multiplexing a wire-format break later. Five bytes per frame is a rounding
// error next to that.

// BinaryHeaderLen is the size of the fixed header on a binary frame.
const BinaryHeaderLen = 5

// MaxPayloadLen bounds a single frame's payload. Terminal reads are a few KiB
// at most; anything larger is a bug or an attempt to exhaust memory.
const MaxPayloadLen = 1 << 20 // 1 MiB

// typeNames maps types to their JSON representation. Control frames are meant
// to be readable in a packet capture, so the wire carries "OPEN", not 1.
var typeNames = map[Type]string{
	TypeOpen:   "OPEN",
	TypeData:   "DATA",
	TypeResize: "RESIZE",
	TypePing:   "PING",
	TypePong:   "PONG",
	TypeClose:  "CLOSE",
	TypeError:  "ERROR",
}

var typeValues = func() map[string]Type {
	m := make(map[string]Type, len(typeNames))
	for t, n := range typeNames {
		m[n] = t
	}
	return m
}()

// MarshalJSON renders a Type as its stable name.
func (t Type) MarshalJSON() ([]byte, error) {
	n, ok := typeNames[t]
	if !ok {
		return nil, fmt.Errorf("%w: unknown type %d", ErrInvalidFrame, uint8(t))
	}
	return json.Marshal(n)
}

// UnmarshalJSON parses a Type from its name.
func (t *Type) UnmarshalJSON(b []byte) error {
	var n string
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("%w: type must be a string", ErrInvalidFrame)
	}
	v, ok := typeValues[n]
	if !ok {
		return fmt.Errorf("%w: unknown type %q", ErrInvalidFrame, n)
	}
	*t = v
	return nil
}

// envelope is the JSON shape of a control frame on the wire.
type envelope struct {
	Type    Type            `json:"type"`
	Channel ChannelID       `json:"channel,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// MarshalControl encodes a control frame as its JSON envelope. Passing a DATA
// frame is a programming error.
func MarshalControl(f Frame) ([]byte, error) {
	if f.Type == TypeData {
		return nil, fmt.Errorf("%w: DATA frames use the binary encoding", ErrInvalidFrame)
	}
	if !f.Type.Valid() {
		return nil, fmt.Errorf("%w: unknown type %d", ErrInvalidFrame, uint8(f.Type))
	}
	return json.Marshal(envelope{Type: f.Type, Channel: f.Channel, Payload: f.Payload})
}

// UnmarshalControl decodes a JSON envelope into a Frame.
func UnmarshalControl(b []byte) (Frame, error) {
	if len(b) > MaxPayloadLen {
		return Frame{}, fmt.Errorf("%w: control message too large (%d bytes)", ErrInvalidFrame, len(b))
	}
	var e envelope
	if err := json.Unmarshal(b, &e); err != nil {
		return Frame{}, fmt.Errorf("%w: %s", ErrInvalidFrame, err)
	}
	if e.Type == TypeData {
		// A peer must not smuggle terminal bytes through the JSON path; it
		// would bypass the binary size accounting and the type's contract.
		return Frame{}, fmt.Errorf("%w: DATA is not valid in a control message", ErrInvalidFrame)
	}
	return Frame{Type: e.Type, Channel: e.Channel, Payload: e.Payload}, nil
}

// MarshalBinary encodes a DATA frame as header plus raw payload.
func MarshalBinary(f Frame) ([]byte, error) {
	if f.Type != TypeData {
		return nil, fmt.Errorf("%w: only DATA frames use the binary encoding", ErrInvalidFrame)
	}
	if len(f.Payload) > MaxPayloadLen {
		return nil, fmt.Errorf("%w: payload too large (%d bytes)", ErrInvalidFrame, len(f.Payload))
	}
	b := make([]byte, BinaryHeaderLen+len(f.Payload))
	b[0] = byte(f.Type)
	binary.BigEndian.PutUint32(b[1:5], uint32(f.Channel))
	copy(b[BinaryHeaderLen:], f.Payload)
	return b, nil
}

// UnmarshalBinary decodes a binary frame. The returned payload aliases b, so
// callers that retain it must copy first.
func UnmarshalBinary(b []byte) (Frame, error) {
	if len(b) < BinaryHeaderLen {
		return Frame{}, fmt.Errorf("%w: binary frame is %d bytes, need at least %d",
			ErrInvalidFrame, len(b), BinaryHeaderLen)
	}
	if len(b)-BinaryHeaderLen > MaxPayloadLen {
		return Frame{}, fmt.Errorf("%w: payload too large (%d bytes)", ErrInvalidFrame, len(b)-BinaryHeaderLen)
	}
	t := Type(b[0])
	if t != TypeData {
		// Control frames must arrive as JSON; accepting them here would give
		// a peer two ways to say the same thing.
		return Frame{}, fmt.Errorf("%w: binary frame type %d, want DATA", ErrInvalidFrame, b[0])
	}
	return Frame{
		Type:    t,
		Channel: ChannelID(binary.BigEndian.Uint32(b[1:5])),
		Payload: b[BinaryHeaderLen:],
	}, nil
}
