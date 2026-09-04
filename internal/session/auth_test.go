package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
)

func TestAuthenticateGrantsByToken(t *testing.T) {
	m := NewManager(Options{TTL: time.Minute})
	defer m.Close()

	s, err := m.Create(context.Background())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	tests := []struct {
		name  string
		role  protocol.Role
		token string
		want  Access
	}{
		{"host token as host", protocol.RoleHost, s.HostToken, AccessHost},
		{"guest token as guest", protocol.RoleGuest, s.GuestToken, AccessGuest},
		// A viewer link presented as an ordinary join is accepted read-only
		// rather than refused, so one link shape works for everyone.
		{"viewer token as guest", protocol.RoleGuest, s.ViewerToken, AccessViewer},
		// A voluntary downgrade by someone holding a full ticket.
		{"guest token asking to view", protocol.RoleViewer, s.GuestToken, AccessViewer},
		{"viewer token asking to view", protocol.RoleViewer, s.ViewerToken, AccessViewer},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, access, err := m.Authenticate(context.Background(), s.SessionID, tc.role, tc.token)
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if got.SessionID != s.SessionID {
				t.Fatalf("got session %q", got.SessionID)
			}
			if access != tc.want {
				t.Fatalf("access = %q, want %q", access, tc.want)
			}
		})
	}
}

// The whole point of a separate viewer token: it must never confer a keyboard.
func TestAuthenticateViewerCannotWriteOrHost(t *testing.T) {
	m := NewManager(Options{TTL: time.Minute})
	defer m.Close()

	s, err := m.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	_, access, err := m.Authenticate(context.Background(), s.SessionID, protocol.RoleGuest, s.ViewerToken)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if access.CanWrite() {
		t.Fatal("the viewer token granted write access")
	}
	if access.Role() != protocol.RoleViewer {
		t.Fatalf("role = %q, want viewer", access.Role())
	}

	// And it is not a host credential either.
	if _, _, err := m.Authenticate(context.Background(), s.SessionID, protocol.RoleHost, s.ViewerToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("viewer token as host = %v, want ErrUnauthorized", err)
	}
}

func TestAuthenticateTokensAreNotInterchangeable(t *testing.T) {
	m := NewManager(Options{TTL: time.Minute})
	defer m.Close()

	s, err := m.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	other, err := m.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if s.GuestToken == s.ViewerToken || s.HostToken == s.ViewerToken {
		t.Fatal("session tokens must all differ")
	}

	bad := []struct {
		name  string
		id    string
		role  protocol.Role
		token string
	}{
		{"guest token as host", s.SessionID, protocol.RoleHost, s.GuestToken},
		{"host token as guest", s.SessionID, protocol.RoleGuest, s.HostToken},
		{"host token as viewer", s.SessionID, protocol.RoleViewer, s.HostToken},
		{"another session's guest token", s.SessionID, protocol.RoleGuest, other.GuestToken},
		{"another session's viewer token", s.SessionID, protocol.RoleViewer, other.ViewerToken},
		{"wrong token", s.SessionID, protocol.RoleGuest, strings.Repeat("a", len(s.GuestToken))},
		{"empty token", s.SessionID, protocol.RoleGuest, ""},
		{"truncated token", s.SessionID, protocol.RoleGuest, s.GuestToken[:len(s.GuestToken)-1]},
		{"token with suffix", s.SessionID, protocol.RoleGuest, s.GuestToken + "a"},
		{"unknown session", mustID(t), protocol.RoleGuest, s.GuestToken},
		{"malformed session", "../etc/passwd", protocol.RoleGuest, s.GuestToken},
		{"unknown role", s.SessionID, protocol.Role("admin"), s.GuestToken},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			got, access, err := m.Authenticate(context.Background(), tc.id, tc.role, tc.token)
			if got != nil {
				t.Fatal("Authenticate returned a session")
			}
			if access != "" {
				t.Fatalf("access = %q, want none", access)
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

	if _, _, err := m.Authenticate(context.Background(), s.SessionID, protocol.RoleHost, s.HostToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired session got %v, want ErrUnauthorized", err)
	}
}

func TestAccessCanWrite(t *testing.T) {
	if !AccessHost.CanWrite() || !AccessGuest.CanWrite() {
		t.Fatal("host and guest must be able to write")
	}
	if AccessViewer.CanWrite() {
		t.Fatal("a viewer must not be able to write")
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
