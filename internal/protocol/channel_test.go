package protocol

import (
	"errors"
	"strings"
	"testing"
)

func TestChannelOpenTarget(t *testing.T) {
	tests := []struct {
		open ChannelOpen
		want string
	}{
		{ChannelOpen{Host: "localhost", Port: 5432}, "localhost:5432"},
		{ChannelOpen{Host: "127.0.0.1", Port: 80}, "127.0.0.1:80"},
		// An IPv6 literal has to come back bracketed or the dialler cannot
		// tell the address from the port.
		{ChannelOpen{Host: "::1", Port: 8080}, "[::1]:8080"},
	}
	for _, tc := range tests {
		if got := tc.open.Target(); got != tc.want {
			t.Errorf("Target() = %q, want %q", got, tc.want)
		}
	}
}

func TestChannelOpenValidate(t *testing.T) {
	good := ChannelOpen{Kind: ChannelKindTCP, Host: "localhost", Port: 5432}
	if err := good.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	bad := []struct {
		name string
		open ChannelOpen
	}{
		{"no kind", ChannelOpen{Host: "localhost", Port: 1}},
		{"unknown kind", ChannelOpen{Kind: "udp", Host: "localhost", Port: 1}},
		{"no host", ChannelOpen{Kind: ChannelKindTCP, Port: 1}},
		{"absurd host", ChannelOpen{Kind: ChannelKindTCP, Host: strings.Repeat("a", 300), Port: 1}},
		{"no port", ChannelOpen{Kind: ChannelKindTCP, Host: "localhost"}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.open.Validate(); !errors.Is(err, ErrInvalidFrame) {
				t.Fatalf("Validate() = %v, want ErrInvalidFrame", err)
			}
		})
	}
}

func TestChannelOpenRoundTrip(t *testing.T) {
	// A channel open travels as a control frame on a non-zero channel, which
	// is what distinguishes it from a session attach on channel 0.
	want := ChannelOpen{Kind: ChannelKindTCP, Host: "db.internal", Port: 5432}
	f, err := NewControl(TypeOpen, want)
	if err != nil {
		t.Fatal(err)
	}
	f.Channel = 7

	encoded, err := MarshalControl(f)
	if err != nil {
		t.Fatalf("MarshalControl: %v", err)
	}
	got, err := UnmarshalControl(encoded)
	if err != nil {
		t.Fatalf("UnmarshalControl: %v", err)
	}
	if got.Channel != 7 {
		t.Fatalf("channel = %d, want 7", got.Channel)
	}
	var open ChannelOpen
	if err := DecodeControl(got, &open); err != nil {
		t.Fatal(err)
	}
	if open != want {
		t.Fatalf("got %+v, want %+v", open, want)
	}
}

func TestChannelIDIsTerminal(t *testing.T) {
	if !ChannelTerminal.IsTerminal() {
		t.Fatal("channel 0 should be the terminal")
	}
	if ChannelID(1).IsTerminal() {
		t.Fatal("channel 1 should not be the terminal")
	}
}
