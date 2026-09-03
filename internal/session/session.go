// Package session owns the lifecycle of shared-terminal sessions: identifier
// and credential generation, an in-memory store, and expiry.
//
// The package knows nothing about HTTP, WebSockets or PTYs. That separation is
// deliberate — session management is the piece most likely to grow (persistence,
// quotas, multiple guests), and it should be testable without a server.
package session

import (
	"errors"
	"time"
)

// Errors returned by Manager.
var (
	// ErrNotFound means no live session has that ID. Callers must not
	// distinguish "never existed" from "expired" in responses to untrusted
	// clients, to avoid confirming that an ID was ever valid.
	ErrNotFound = errors.New("session: not found")
	// ErrInvalidID means the supplied identifier is malformed.
	ErrInvalidID = errors.New("session: invalid id")
	// ErrClosed means the manager has been shut down.
	ErrClosed = errors.New("session: manager closed")
)

// Session is a single shared terminal.
//
// The public identifier and the secret credentials are separate fields on
// purpose: SessionID may be logged, printed and put in a URL, while HostToken
// and GuestToken must never be. Nothing in this repository logs the token
// fields, and Session deliberately has no String or MarshalJSON method that
// would expose them by accident — the API layer builds its own response types.
type Session struct {
	// SessionID is the safe public identifier.
	SessionID string
	// HostToken authenticates the host's outbound tunnel connection.
	HostToken string
	// GuestToken authenticates a guest joining the session.
	GuestToken string

	CreatedAt time.Time
	ExpiresAt time.Time
}

// Expired reports whether the session is no longer valid at time now.
func (s *Session) Expired(now time.Time) bool {
	return !now.Before(s.ExpiresAt)
}

// clone returns a copy so callers cannot mutate stored state through the
// pointer they receive.
func (s *Session) clone() *Session {
	c := *s
	return &c
}
