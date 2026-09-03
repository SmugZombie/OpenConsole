package session

import (
	"context"
	"crypto/subtle"
	"errors"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
)

// ErrUnauthorized means the supplied token does not match the session.
var ErrUnauthorized = errors.New("session: unauthorized")

// Authenticate looks up a session and checks a role's token against it.
//
// Token comparison is constant-time. A byte-by-byte comparison would leak the
// correct prefix through timing, letting an attacker recover a token one byte
// at a time instead of guessing 256 bits.
//
// A wrong token and an unknown session both return ErrUnauthorized, so a caller
// cannot use the error to learn whether a session ID exists.
func (m *Manager) Authenticate(ctx context.Context, id string, role protocol.Role, token string) (*Session, error) {
	s, err := m.Get(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalidID) {
			return nil, ErrUnauthorized
		}
		return nil, err
	}

	var want string
	switch role {
	case protocol.RoleHost:
		want = s.HostToken
	case protocol.RoleGuest:
		want = s.GuestToken
	default:
		return nil, ErrUnauthorized
	}

	if subtle.ConstantTimeCompare([]byte(token), []byte(want)) != 1 {
		return nil, ErrUnauthorized
	}
	return s, nil
}
