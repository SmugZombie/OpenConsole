package client

import (
	"fmt"

	"github.com/SmugZombie/OpenConsole/internal/e2e"
	"github.com/SmugZombie/OpenConsole/internal/protocol"
)

// crypter seals and opens DATA frames at the edge of the tunnel.
//
// It exists so encryption happens in exactly one place on each side, rather
// than at every point that happens to produce a frame. Terminal output, guest
// keystrokes and forwarded TCP bytes all become DATA frames in different parts
// of this package; remembering to encrypt at each of them is precisely the kind
// of thing that gets forgotten once, silently, and ships plaintext.
//
// A zero crypter passes everything through, which is the unencrypted session.
type crypter struct {
	sess *e2e.Session
	seal func(protocol.ChannelID, []byte) ([]byte, error)
	open func(protocol.ChannelID, []byte) ([]byte, error)
}

// hostCrypter encrypts what the host sends and decrypts what guests send.
func hostCrypter(s *e2e.Session) crypter {
	if !s.Enabled() {
		return crypter{}
	}
	return crypter{sess: s, seal: s.SealHostToGuest, open: s.OpenGuestToHost}
}

// guestCrypter is the mirror image.
func guestCrypter(s *e2e.Session) crypter {
	if !s.Enabled() {
		return crypter{}
	}
	return crypter{sess: s, seal: s.SealGuestToHost, open: s.OpenHostToGuest}
}

// enabled reports whether frames are being encrypted.
func (c crypter) enabled() bool { return c.sess.Enabled() }

// outbound seals a frame on its way to the tunnel. Frames that are not DATA
// pass through: the relay has to read their type and channel to route them.
func (c crypter) outbound(f protocol.Frame) (protocol.Frame, error) {
	if !c.enabled() || f.Type != protocol.TypeData {
		return f, nil
	}
	sealed, err := c.seal(f.Channel, f.Payload)
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("encrypting terminal data: %w", err)
	}
	f.Payload = sealed
	return f, nil
}

// inbound opens a frame arriving from the tunnel.
func (c crypter) inbound(f protocol.Frame) (protocol.Frame, error) {
	if !c.enabled() || f.Type != protocol.TypeData {
		return f, nil
	}
	plain, err := c.open(f.Channel, f.Payload)
	if err != nil {
		// This is what a tampering or substituting relay looks like from
		// here, so it is worth saying plainly rather than as a decode error.
		return protocol.Frame{}, fmt.Errorf(
			"a frame did not decrypt; the relay may be interfering, or the link may be corrupt: %w", err)
	}
	f.Payload = plain
	return f, nil
}
