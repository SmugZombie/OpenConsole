package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestTypeJSONRoundTrip(t *testing.T) {
	for _, tp := range []Type{TypeOpen, TypeData, TypeResize, TypePing, TypePong, TypeClose, TypeError} {
		b, err := json.Marshal(tp)
		if err != nil {
			t.Fatalf("marshal %s: %v", tp, err)
		}
		// The wire carries the name, not the number, so a capture is readable.
		if want := `"` + tp.String() + `"`; string(b) != want {
			t.Fatalf("marshal %s = %s, want %s", tp, b, want)
		}
		var got Type
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal %s: %v", b, err)
		}
		if got != tp {
			t.Fatalf("round trip gave %s, want %s", got, tp)
		}
	}
}

func TestTypeJSONRejectsUnknown(t *testing.T) {
	var tp Type
	for _, in := range []string{`"NOPE"`, `1`, `null`, `{}`} {
		if err := json.Unmarshal([]byte(in), &tp); !errors.Is(err, ErrInvalidFrame) {
			t.Fatalf("unmarshal %s = %v, want ErrInvalidFrame", in, err)
		}
	}
}

func TestControlWireRoundTrip(t *testing.T) {
	in, err := NewControl(TypeResize, Resize{Cols: 120, Rows: 40})
	if err != nil {
		t.Fatal(err)
	}
	b, err := MarshalControl(in)
	if err != nil {
		t.Fatalf("MarshalControl: %v", err)
	}
	if !strings.Contains(string(b), `"type":"RESIZE"`) {
		t.Fatalf("wire form is not self-describing: %s", b)
	}

	out, err := UnmarshalControl(b)
	if err != nil {
		t.Fatalf("UnmarshalControl: %v", err)
	}
	if out.Type != TypeResize {
		t.Fatalf("type = %s", out.Type)
	}
	var r Resize
	if err := DecodeControl(out, &r); err != nil {
		t.Fatal(err)
	}
	if r.Cols != 120 || r.Rows != 40 {
		t.Fatalf("resize = %dx%d", r.Cols, r.Rows)
	}
}

func TestBinaryWireRoundTrip(t *testing.T) {
	// Arbitrary bytes, including a NUL, an escape and invalid UTF-8: exactly
	// what JSON could not carry without base64.
	payload := []byte{0x00, 0x1b, '[', 'A', 0xff, 0xfe, '\n'}
	b, err := MarshalBinary(NewData(payload))
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	if len(b) != BinaryHeaderLen+len(payload) {
		t.Fatalf("encoded length = %d, want %d", len(b), BinaryHeaderLen+len(payload))
	}

	out, err := UnmarshalBinary(b)
	if err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}
	if out.Type != TypeData {
		t.Fatalf("type = %s", out.Type)
	}
	if out.Channel != ChannelControl {
		t.Fatalf("channel = %d", out.Channel)
	}
	if !bytes.Equal(out.Payload, payload) {
		t.Fatalf("payload = %v, want %v", out.Payload, payload)
	}
}

func TestBinaryPreservesChannel(t *testing.T) {
	// Multiplexing is not implemented, but the header must already carry the
	// channel or adding it later would break the wire format.
	f := Frame{Type: TypeData, Channel: 0xDEADBEEF, Payload: []byte("x")}
	b, err := MarshalBinary(f)
	if err != nil {
		t.Fatal(err)
	}
	out, err := UnmarshalBinary(b)
	if err != nil {
		t.Fatal(err)
	}
	if out.Channel != 0xDEADBEEF {
		t.Fatalf("channel = %#x, want 0xDEADBEEF", out.Channel)
	}
}

func TestWireRejectsMixedEncodings(t *testing.T) {
	if _, err := MarshalControl(NewData([]byte("x"))); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("MarshalControl(DATA) = %v, want ErrInvalidFrame", err)
	}
	if _, err := MarshalBinary(Frame{Type: TypePing}); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("MarshalBinary(PING) = %v, want ErrInvalidFrame", err)
	}
	// A peer must not be able to smuggle DATA through the JSON path.
	if _, err := UnmarshalControl([]byte(`{"type":"DATA","payload":"aGk="}`)); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("UnmarshalControl(DATA) = %v, want ErrInvalidFrame", err)
	}
	// ...nor control frames through the binary path.
	if _, err := UnmarshalBinary([]byte{byte(TypePing), 0, 0, 0, 0}); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("UnmarshalBinary(PING) = %v, want ErrInvalidFrame", err)
	}
}

func TestWireRejectsMalformed(t *testing.T) {
	for _, b := range [][]byte{nil, {1}, {1, 2, 3, 4}} {
		if _, err := UnmarshalBinary(b); !errors.Is(err, ErrInvalidFrame) {
			t.Fatalf("UnmarshalBinary(%v) = %v, want ErrInvalidFrame", b, err)
		}
	}
	for _, s := range []string{"", "{", "[]", `{"type":123}`} {
		if _, err := UnmarshalControl([]byte(s)); !errors.Is(err, ErrInvalidFrame) {
			t.Fatalf("UnmarshalControl(%q) = %v, want ErrInvalidFrame", s, err)
		}
	}
}

func TestWireRejectsOversizedPayload(t *testing.T) {
	f := Frame{Type: TypeData, Payload: make([]byte, MaxPayloadLen+1)}
	if _, err := MarshalBinary(f); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("oversized MarshalBinary = %v, want ErrInvalidFrame", err)
	}
	big := make([]byte, MaxPayloadLen+BinaryHeaderLen+1)
	big[0] = byte(TypeData)
	if _, err := UnmarshalBinary(big); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("oversized UnmarshalBinary = %v, want ErrInvalidFrame", err)
	}
}
