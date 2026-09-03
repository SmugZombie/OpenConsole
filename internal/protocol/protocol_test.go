package protocol

import (
	"bytes"
	"errors"
	"testing"
)

func TestTypeStringAndValid(t *testing.T) {
	all := []Type{TypeOpen, TypeData, TypeResize, TypePing, TypePong, TypeClose, TypeError}
	seen := make(map[string]struct{}, len(all))
	for _, tp := range all {
		if !tp.Valid() {
			t.Fatalf("%d should be valid", uint8(tp))
		}
		s := tp.String()
		if s == "" {
			t.Fatalf("%d has no name", uint8(tp))
		}
		if _, dup := seen[s]; dup {
			t.Fatalf("duplicate type name %q", s)
		}
		seen[s] = struct{}{}
	}
	if Type(0).Valid() || Type(99).Valid() {
		t.Fatal("unknown types must not validate")
	}
}

func TestNewDataCarriesRawBytes(t *testing.T) {
	// Terminal output is arbitrary binary; it must survive untouched.
	payload := []byte{0x00, 0x1b, '[', 'A', 0xff, '\n'}
	f := NewData(payload)
	if f.Type != TypeData {
		t.Fatalf("type = %s, want DATA", f.Type)
	}
	if f.Channel != ChannelControl {
		t.Fatalf("channel = %d, want %d", f.Channel, ChannelControl)
	}
	if !bytes.Equal(f.Payload, payload) {
		t.Fatalf("payload mutated: %v", f.Payload)
	}
}

func TestControlRoundTrip(t *testing.T) {
	in := Open{Version: Version, SessionID: "abc", Role: RoleHost, Token: "secret", Cols: 120, Rows: 40}
	f, err := NewControl(TypeOpen, in)
	if err != nil {
		t.Fatalf("NewControl: %v", err)
	}
	var out Open
	if err := DecodeControl(f, &out); err != nil {
		t.Fatalf("DecodeControl: %v", err)
	}
	if out != in {
		t.Fatalf("round trip mismatch: %+v != %+v", out, in)
	}
}

func TestControlRejectsDataFrames(t *testing.T) {
	if _, err := NewControl(TypeData, Resize{}); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("NewControl(TypeData) = %v, want ErrInvalidFrame", err)
	}
	if err := DecodeControl(NewData(nil), &Resize{}); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("DecodeControl(DATA) = %v, want ErrInvalidFrame", err)
	}
	if _, err := NewControl(Type(99), Resize{}); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("NewControl(99) = %v, want ErrInvalidFrame", err)
	}
}
