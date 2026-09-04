package session

import (
	"context"
	"crypto/subtle"
	"errors"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
)

// ErrUnauthorized means the supplied token does not match the session.
var ErrUnauthorized = errors.New("session: unauthorized")

// Access is what a credential turned out to be worth.
//
// It is the relay's answer, never the client's request: a connection presents a
// token and is told what that token grants. Nothing a client sends can widen
// it, which is what makes a viewer link safe to hand out.
type Access string

const (
	// AccessHost is the machine sharing its terminal.
	AccessHost Access = "host"
	// AccessGuest may watch and type.
	AccessGuest Access = "guest"
	// AccessViewer may watch only.
	AccessViewer Access = "viewer"
)

// CanWrite reports whether this access level may send input to the terminal.
func (a Access) CanWrite() bool { return a == AccessHost || a == AccessGuest }

// Role maps an access level onto the protocol role reported to the client.
func (a Access) Role() protocol.Role {
	switch a {
	case AccessHost:
		return protocol.RoleHost
	case AccessViewer:
		return protocol.RoleViewer
	default:
		return protocol.RoleGuest
	}
}

// Authenticate looks up a session and works out what a token grants.
//
// Token comparison is constant-time, and every candidate is compared on every
// call. A byte-by-byte comparison, or short-circuiting once one matched, would
// leak through timing — letting an attacker recover a token a byte at a time
// instead of guessing 256 bits.
//
// A wrong token and an unknown session both return ErrUnauthorized, so a caller
// cannot use the error to learn whether a session ID exists.
func (m *Manager) Authenticate(ctx context.Context, id string, role protocol.Role, token string) (*Session, Access, error) {
	s, err := m.Get(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalidID) {
			return nil, "", ErrUnauthorized
		}
		return nil, "", err
	}

	// All three are compared every time; the branch below is on the requested
	// role, which is not a secret.
	isHost := constantTimeEqual(token, s.HostToken)
	isGuest := constantTimeEqual(token, s.GuestToken)
	isViewer := constantTimeEqual(token, s.ViewerToken)

	switch role {
	case protocol.RoleHost:
		// Guest and viewer credentials must never open a host tunnel.
		if isHost {
			return s, AccessHost, nil
		}

	case protocol.RoleGuest:
		// The token decides. A viewer link presented as an ordinary join gets
		// read-only access rather than a refusal, so one link shape works for
		// everyone and the holder is simply told what they got.
		if isGuest {
			return s, AccessGuest, nil
		}
		if isViewer {
			return s, AccessViewer, nil
		}

	case protocol.RoleViewer:
		// A voluntary downgrade: someone holding a full guest token asking to
		// watch without the risk of a stray keystroke.
		if isGuest || isViewer {
			return s, AccessViewer, nil
		}
	}

	return nil, "", ErrUnauthorized
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
