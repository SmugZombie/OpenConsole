// Package e2e encrypts terminal traffic between a host and its guests so the
// relay cannot read or forge it.
//
// # What the relay knows
//
// Everything the relay holds today — the session ID and all three tokens — it
// was given, because it uses them to authenticate connections. None of them can
// therefore be the encryption key. So the host generates one more secret that
// the relay is never told: it exists only in the ticket, which travels
// out-of-band to whoever is joining, and in the URL fragment, which browsers do
// not transmit.
//
// There is no key exchange, and so nothing for the relay to interpose on. It
// routes ciphertext, matches nothing, and cannot inject a keystroke it can make
// the host accept.
//
// # Read-only, enforced by arithmetic
//
// Two keys are derived from the root, one per direction. A full guest gets the
// root and can derive both. A viewer is given only the host-to-guest key, so it
// can read the terminal and cannot produce input the host will accept —
// read-only stops depending on the relay behaving.
//
// # What is still visible
//
// Frame types, channel numbers, message sizes and timing. A relay can see that
// a session is busy, how big each burst is, and when someone joined. It can
// also drop or reorder frames, which shows up as a broken session rather than a
// silent lie. Hiding traffic shape would need padding and cover traffic, which
// is not attempted.
package e2e

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"

	"crypto/sha256"
	"golang.org/x/crypto/hkdf"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
)

// KeySize is the length of the root key and of each derived key.
const KeySize = 32

// Direction labels a key's purpose. They are distinct so a frame captured in
// one direction cannot be replayed in the other.
const (
	infoHostToGuest = "openconsole/v1 host-to-guest"
	infoGuestToHost = "openconsole/v1 guest-to-host"
)

// Errors returned by this package.
var (
	// ErrNoKey means this participant holds no key for that direction. A
	// viewer asking to encrypt input is the expected case.
	ErrNoKey = errors.New("e2e: no key for this direction")
	// ErrDecrypt means a frame did not authenticate. It is deliberately
	// uninformative: a caller must not be able to tell a corrupted frame from
	// a forged one.
	ErrDecrypt = errors.New("e2e: could not decrypt")
	// ErrBadKey means a key was the wrong length.
	ErrBadKey = errors.New("e2e: malformed key")
)

// NewRootKey returns a fresh session key.
//
// crypto/rand, like every other secret here. This one protects the contents of
// somebody's terminal, so a predictable value would hand the relay everything
// the rest of this package exists to withhold.
func NewRootKey() ([]byte, error) {
	key := make([]byte, KeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("e2e: generating a session key: %w", err)
	}
	return key, nil
}

// Session holds the AEADs for one shared terminal.
//
// A nil Session means the session is not encrypted, and every method is a
// no-op that passes bytes through. That keeps the call sites free of
// conditionals, which is where a mistake would silently send plaintext.
type Session struct {
	hostToGuest cipher.AEAD
	// guestToHost is nil for a viewer, which is what makes read-only
	// cryptographic rather than a rule the relay is trusted to apply.
	guestToHost cipher.AEAD
}

// FromRootKey derives both directions. Hosts and full guests use this.
//
// The session ID is the HKDF salt, so the same root key used for two sessions
// would still produce different traffic keys.
func FromRootKey(sessionID string, root []byte) (*Session, error) {
	if len(root) != KeySize {
		return nil, ErrBadKey
	}
	out, err := aeadFrom(sessionID, root, infoHostToGuest)
	if err != nil {
		return nil, err
	}
	in, err := aeadFrom(sessionID, root, infoGuestToHost)
	if err != nil {
		return nil, err
	}
	return &Session{hostToGuest: out, guestToHost: in}, nil
}

// FromViewerKey builds a read-only session from the host-to-guest key alone.
func FromViewerKey(viewerKey []byte) (*Session, error) {
	if len(viewerKey) != KeySize {
		return nil, ErrBadKey
	}
	out, err := newAEAD(viewerKey)
	if err != nil {
		return nil, err
	}
	return &Session{hostToGuest: out}, nil
}

// ViewerKey derives the key a viewer is given: enough to read the terminal,
// not enough to write to it.
func ViewerKey(sessionID string, root []byte) ([]byte, error) {
	if len(root) != KeySize {
		return nil, ErrBadKey
	}
	return derive(sessionID, root, infoHostToGuest)
}

// Enabled reports whether traffic is encrypted. A nil Session is not.
func (s *Session) Enabled() bool { return s != nil }

// CanWrite reports whether this participant can produce input the host will
// accept.
func (s *Session) CanWrite() bool { return s != nil && s.guestToHost != nil }

// SealHostToGuest encrypts terminal output.
func (s *Session) SealHostToGuest(channel protocol.ChannelID, plaintext []byte) ([]byte, error) {
	if s == nil {
		return plaintext, nil
	}
	return seal(s.hostToGuest, channel, plaintext)
}

// OpenHostToGuest decrypts terminal output.
func (s *Session) OpenHostToGuest(channel protocol.ChannelID, sealed []byte) ([]byte, error) {
	if s == nil {
		return sealed, nil
	}
	return open(s.hostToGuest, channel, sealed)
}

// SealGuestToHost encrypts input. A viewer has no key for this.
func (s *Session) SealGuestToHost(channel protocol.ChannelID, plaintext []byte) ([]byte, error) {
	if s == nil {
		return plaintext, nil
	}
	if s.guestToHost == nil {
		return nil, ErrNoKey
	}
	return seal(s.guestToHost, channel, plaintext)
}

// OpenGuestToHost decrypts input.
func (s *Session) OpenGuestToHost(channel protocol.ChannelID, sealed []byte) ([]byte, error) {
	if s == nil {
		return sealed, nil
	}
	if s.guestToHost == nil {
		return nil, ErrNoKey
	}
	return open(s.guestToHost, channel, sealed)
}

// Overhead is how many bytes sealing adds: a nonce and a tag.
func Overhead() int { return nonceSize + tagSize }

const (
	nonceSize = 12 // GCM standard nonce
	tagSize   = 16
)

// seal produces nonce || ciphertext || tag.
//
// The nonce is random rather than a counter because several guests share the
// guest-to-host key and would otherwise have to agree on who uses which
// counter. With a 96-bit random nonce the chance of a repeat across the
// millions of frames a terminal might produce is far below anything else that
// could go wrong here.
func seal(aead cipher.AEAD, channel protocol.ChannelID, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("e2e: generating a nonce: %w", err)
	}
	out := make([]byte, 0, nonceSize+len(plaintext)+tagSize)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, plaintext, channelAAD(channel)), nil
}

// open reverses seal.
func open(aead cipher.AEAD, channel protocol.ChannelID, sealed []byte) ([]byte, error) {
	if len(sealed) < nonceSize+tagSize {
		return nil, ErrDecrypt
	}
	plaintext, err := aead.Open(nil, sealed[:nonceSize], sealed[nonceSize:], channelAAD(channel))
	if err != nil {
		// Never wrap: the underlying error says nothing useful and callers
		// must not branch on why authentication failed.
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

// channelAAD binds a frame to its channel, so a relay cannot take bytes from a
// forwarded connection and pass them off as terminal input, or the reverse.
func channelAAD(channel protocol.ChannelID) []byte {
	return []byte{
		byte(channel >> 24), byte(channel >> 16), byte(channel >> 8), byte(channel),
	}
}

// aeadFrom derives a key and wraps it.
func aeadFrom(sessionID string, root []byte, info string) (cipher.AEAD, error) {
	key, err := derive(sessionID, root, info)
	if err != nil {
		return nil, err
	}
	return newAEAD(key)
}

// derive runs HKDF-SHA256 over the root key.
func derive(sessionID string, root []byte, info string) ([]byte, error) {
	key := make([]byte, KeySize)
	r := hkdf.New(sha256.New, root, []byte(sessionID), []byte(info))
	if _, err := io.ReadFull(r, key); err != nil {
		return nil, fmt.Errorf("e2e: deriving a key: %w", err)
	}
	return key, nil
}

// newAEAD wraps a key in AES-256-GCM.
//
// AES-GCM rather than something newer because it is the one modern AEAD
// available natively on both sides: crypto/cipher here, and WebCrypto in the
// browser. Shipping a JavaScript cipher implementation to get a different
// primitive would trade a vetted one for an unvetted one.
func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, ErrBadKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("e2e: %w", err)
	}
	return cipher.NewGCM(block)
}
