package session

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
)

// idBytes is the entropy behind a public session identifier. 16 bytes (128
// bits) makes a session ID unguessable, which matters because the ID is the
// only thing a guest needs in order to attempt a join.
const idBytes = 16

// tokenBytes is the entropy behind a host/guest credential. Tokens grant
// actual access, so they get 256 bits.
const tokenBytes = 32

// idEncoding is unpadded, lowercase-friendly base32. Base32 is chosen over
// base64 because session IDs are meant to be read aloud, typed by hand and
// eventually used as an SSH username (`ssh <session>@host`), where '+' and '/'
// would be awkward.
var idEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// IDLength is the exact character length of a generated session ID.
var IDLength = idEncoding.EncodedLen(idBytes)

// NewID returns a cryptographically random public session identifier.
//
// crypto/rand is mandatory here: a predictable ID would let an attacker
// enumerate live sessions. math/rand must never be used for any value in this
// package.
func NewID() (string, error) {
	b := make([]byte, idBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("session: generate id: %w", err)
	}
	return strings.ToLower(idEncoding.EncodeToString(b)), nil
}

// NewToken returns a cryptographically random bearer credential.
func NewToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("session: generate token: %w", err)
	}
	return strings.ToLower(idEncoding.EncodeToString(b)), nil
}

// ValidID reports whether s has the exact shape produced by NewID.
//
// Every externally supplied identifier is checked with this before it reaches
// the session store, so that path traversal, oversized keys and injection
// attempts are rejected at the edge rather than deeper in the system.
func ValidID(s string) bool {
	if len(s) != IDLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isLower := c >= 'a' && c <= 'z'
		isDigit := c >= '2' && c <= '7' // base32 alphabet digits
		if !isLower && !isDigit {
			return false
		}
	}
	return true
}
