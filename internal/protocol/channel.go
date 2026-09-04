package protocol

import (
	"fmt"
	"net"
	"strconv"
)

// Channels multiplex independent streams over one tunnel.
//
// Channel 0 is the terminal and behaves exactly as it did before channels
// existed. Every other channel is a forwarded TCP connection, opened by a guest
// and dialled by the host.
//
// The channel field has been in the binary header since version 1 precisely so
// this could be added without a wire-format break; nothing here changes how an
// existing terminal frame looks on the wire.

// ChannelTerminal is the channel the shared terminal runs on.
const ChannelTerminal ChannelID = ChannelControl

// MaxChannels bounds how many forwarded streams one tunnel may hold open.
//
// Each costs a goroutine, a queue and a socket on the host, so an unbounded
// count would let one guest exhaust the host's file descriptors.
const MaxChannels = 64

// ChannelKind names what a channel carries. Only TCP exists today; naming it
// means a later kind (a file transfer, say) does not have to guess.
type ChannelKind string

// ChannelKindTCP is a forwarded TCP connection.
const ChannelKindTCP ChannelKind = "tcp"

// ChannelOpen is the payload of an OPEN frame on a non-zero channel.
//
// An OPEN on channel 0 is a session attach and uses Open instead; the channel
// number is what distinguishes them.
type ChannelOpen struct {
	Kind ChannelKind `json:"kind"`
	// Host and Port are the address the *host* should dial, resolved on the
	// host's machine. That is the point of forwarding: the guest reaches
	// something only the host can see.
	Host string `json:"host"`
	Port uint16 `json:"port"`
}

// Target renders the dial address.
func (c ChannelOpen) Target() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(int(c.Port)))
}

// Validate checks a channel open request that arrived from the network.
//
// The host name is passed to a dialler, never to a shell, so the danger here is
// not injection but an empty or absurd value reaching the resolver.
func (c ChannelOpen) Validate() error {
	if c.Kind != ChannelKindTCP {
		return fmt.Errorf("%w: unsupported channel kind %q", ErrInvalidFrame, c.Kind)
	}
	if c.Host == "" {
		return fmt.Errorf("%w: channel open has no host", ErrInvalidFrame)
	}
	if len(c.Host) > 255 {
		return fmt.Errorf("%w: channel host is too long", ErrInvalidFrame)
	}
	if c.Port == 0 {
		return fmt.Errorf("%w: channel open has no port", ErrInvalidFrame)
	}
	return nil
}

// IsTerminal reports whether id addresses the shared terminal.
func (id ChannelID) IsTerminal() bool { return id == ChannelTerminal }

// Error codes specific to channels.
const (
	// ErrCodeForwardDenied means the host is not accepting forwards, or not to
	// that address.
	ErrCodeForwardDenied = "forward_denied"
	// ErrCodeForwardFailed means the host could not reach the target.
	ErrCodeForwardFailed = "forward_failed"
	// ErrCodeChannelLimit means too many channels are already open.
	ErrCodeChannelLimit = "channel_limit"
	// ErrCodeUnknownChannel means a frame referenced a channel that is not open.
	ErrCodeUnknownChannel = "unknown_channel"
)
