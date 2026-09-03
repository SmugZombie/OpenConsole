package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
)

func TestAuthenticate(t *testing.T) {
	m := NewManager(Options{TTL: time.Minute})
	defer m.Close()

	s, err := m.Create(context.Background())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	t.Run("host token", func(t *testing.T) {
		got, err := m.Authenticate(context.Background(), s.SessionID, protocol.RoleHost, s.HostToken)
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		if got.SessionID != s.SessionID {
			t.Fatalf("got session %q", got.SessionID)
		}
	})

	t.Run("guest token", func(t *testing.T) {
		if _, err := m.Authenticate(context.Background(), s.SessionID, protocol.RoleGuest, s.GuestToken); err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
	})

	// The tokens are not interchangeable: a guest ticket must not confer host
	// rights, which is the whole reason there are two.
	t.Run("guest token cannot open a host tunnel", func(t *testing.T) {
		if _, err := m.Authenticate(context.Background(), s.SessionID, protocol.RoleHost, s.GuestToken); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("got %v, want ErrUnauthorized", err)
		}
	})

	t.Run("host token cannot open a guest tunnel", func(t *testing.T) {
		if _, err := m.Authenticate(context.Background(), s.SessionID, protocol.RoleGuest, s.HostToken); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("got %v, want ErrUnauthorized", err)
		}
	})

	bad := []struct {
		name  string
		id    string
		role  protocol.Role
		token string
	}{
		{"wrong token", s.SessionID, protocol.RoleHost, strings.Repeat("a", len(s.HostToken))},
		{"empty token", s.SessionID, protocol.RoleHost, ""},
		{"truncated token", s.SessionID, protocol.RoleHost, s.HostToken[:len(s.HostToken)-1]},
		{"token with suffix", s.SessionID, protocol.RoleHost, s.HostToken + "a"},
		{"unknown session", mustID(t), protocol.RoleHost, s.HostToken},
		{"malformed session", "../etc/passwd", protocol.RoleHost, s.HostToken},
		{"unknown role", s.SessionID, protocol.Role("admin"), s.HostToken},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			got, err := m.Authenticate(context.Background(), tc.id, tc.role, tc.token)
			if got != nil {
				t.Fatal("Authenticate returned a session")
			}
			// Every failure is the same error, so the caller cannot tell a
			// wrong token from a session that does not exist.
			if !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("got %v, want ErrUnauthorized", err)
			}
		})
	}
}

func TestAuthenticateExpiredSession(t *testing.T) {
	clock := newFakeClock()
	m := NewManager(Options{TTL: time.Minute, Now: clock.Now})
	defer m.Close()

	s, err := m.Create(context.Background())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	clock.Advance(2 * time.Minute)

	if _, err := m.Authenticate(context.Background(), s.SessionID, protocol.RoleHost, s.HostToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired session got %v, want ErrUnauthorized", err)
	}
}

func mustID(t *testing.T) string {
	t.Helper()
	id, err := NewID()
	if err != nil {
		t.Fatalf("NewID: %v", err)
	}
	return id
}
