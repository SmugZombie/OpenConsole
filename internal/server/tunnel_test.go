package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
	"github.com/SmugZombie/OpenConsole/internal/session"
	"github.com/SmugZombie/OpenConsole/internal/tunnel"
)

// relay is a running relay backed by a real HTTP server and real WebSockets.
type relay struct {
	t        *testing.T
	url      string
	sessions *session.Manager
	bridges  *session.Bridges
}

func newRelay(t *testing.T) *relay {
	t.Helper()

	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	m := session.NewManager(session.Options{TTL: time.Minute})
	t.Cleanup(m.Close)
	b := session.NewBridges(log)

	api := NewAPI(m, b, log, "test", context.Background())
	srv := httptest.NewServer(api.Routes())
	t.Cleanup(srv.Close)

	return &relay{t: t, url: srv.URL, sessions: m, bridges: b}
}

func (r *relay) newSession() *session.Session {
	r.t.Helper()
	s, err := r.sessions.Create(context.Background())
	if err != nil {
		r.t.Fatalf("Create: %v", err)
	}
	return s
}

// dial opens a tunnel and sends OPEN, returning the connection and the relay's
// first answer.
func (r *relay) dial(open protocol.Open) (tunnel.Conn, protocol.Frame) {
	r.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := tunnel.Dial(ctx, r.url+"/api/v1/tunnel", tunnel.DialOptions{})
	if err != nil {
		r.t.Fatalf("Dial: %v", err)
	}
	r.t.Cleanup(func() { conn.Close("test over") })

	if open.Version == 0 {
		open.Version = protocol.Version
	}
	if err := tunnel.SendControl(ctx, conn, protocol.TypeOpen, open); err != nil {
		r.t.Fatalf("send OPEN: %v", err)
	}

	f, err := conn.Recv(ctx)
	if err != nil {
		r.t.Fatalf("recv answer: %v", err)
	}
	return conn, f
}

// attachHost dials as the host and asserts the relay accepted it.
func (r *relay) attachHost(s *session.Session, cols, rows uint16) tunnel.Conn {
	r.t.Helper()
	conn, f := r.dial(protocol.Open{
		SessionID: s.SessionID, Role: protocol.RoleHost, Token: s.HostToken,
		Cols: cols, Rows: rows,
	})
	if f.Type != protocol.TypeOpen {
		r.t.Fatalf("host OPEN answered with %s, want OPEN", f.Type)
	}
	waitUntil(r.t, func() bool {
		b, ok := r.bridges.Get(s.SessionID)
		return ok && b.HostAttached()
	})
	return conn
}

// attachGuest dials as a guest and asserts the relay accepted it.
func (r *relay) attachGuest(s *session.Session) tunnel.Conn {
	r.t.Helper()
	conn, _ := r.attachAs(s, protocol.RoleGuest, s.GuestToken, protocol.RoleGuest)
	return conn
}

// attachAs dials with a chosen role and token, and asserts the access the
// relay granted.
func (r *relay) attachAs(s *session.Session, role protocol.Role, token string, want protocol.Role) (tunnel.Conn, protocol.Open) {
	r.t.Helper()
	conn, f := r.dial(protocol.Open{SessionID: s.SessionID, Role: role, Token: token})
	if f.Type != protocol.TypeOpen {
		r.t.Fatalf("OPEN answered with %s, wanted OPEN", f.Type)
	}
	var ack protocol.Open
	if err := protocol.DecodeControl(f, &ack); err != nil {
		r.t.Fatalf("decode ack: %v", err)
	}
	// The acknowledgement reports what the relay granted, not what was asked
	// for, so a client can tell whether it may type.
	if ack.Role != want {
		r.t.Fatalf("granted role = %q, want %q", ack.Role, want)
	}
	if ack.Token != "" {
		r.t.Fatal("the acknowledgement leaked a credential")
	}
	return conn, ack
}

// recvType waits for the next frame of the given type, skipping others.
func recvType(t *testing.T, c tunnel.Conn, want protocol.Type) protocol.Frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		f, err := c.Recv(ctx)
		if err != nil {
			t.Fatalf("waiting for %s: %v", want, err)
		}
		if f.Type == want {
			return f
		}
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatal("condition not met in time")
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// The end-to-end path: a host's terminal output reaches a guest, and the
// guest's keystrokes reach the host.
func TestTunnelEndToEnd(t *testing.T) {
	r := newRelay(t)
	s := r.newSession()
	ctx := context.Background()

	host := r.attachHost(s, 100, 40)
	guest := r.attachGuest(s)

	// The guest is told the terminal's shape as it joins.
	var size protocol.Resize
	if err := protocol.DecodeControl(recvType(t, guest, protocol.TypeResize), &size); err != nil {
		t.Fatalf("decode resize: %v", err)
	}
	if size.Cols != 100 || size.Rows != 40 {
		t.Fatalf("guest size = %dx%d, want 100x40", size.Cols, size.Rows)
	}

	waitUntil(t, func() bool {
		b, ok := r.bridges.Get(s.SessionID)
		return ok && b.Guests() == 1
	})

	// Host output, including bytes JSON could not carry, reaches the guest.
	out := []byte("$ ls\x1b[0m\x00\xff\r\n")
	if err := host.Send(ctx, protocol.NewData(out)); err != nil {
		t.Fatalf("host send: %v", err)
	}
	if got := recvType(t, guest, protocol.TypeData); string(got.Payload) != string(out) {
		t.Fatalf("guest received %q, want %q", got.Payload, out)
	}

	// Guest keystrokes reach the host.
	if err := guest.Send(ctx, protocol.NewData([]byte("whoami\r"))); err != nil {
		t.Fatalf("guest send: %v", err)
	}
	if got := recvType(t, host, protocol.TypeData); string(got.Payload) != "whoami\r" {
		t.Fatalf("host received %q, want %q", got.Payload, "whoami\r")
	}
}

func TestTunnelGuestReceivesScrollbackOnJoin(t *testing.T) {
	r := newRelay(t)
	s := r.newSession()

	host := r.attachHost(s, 80, 24)
	if err := host.Send(context.Background(), protocol.NewData([]byte("output before joining\r\n"))); err != nil {
		t.Fatalf("host send: %v", err)
	}
	waitUntil(t, func() bool {
		b, _ := r.bridges.Get(s.SessionID)
		return b != nil
	})

	guest := r.attachGuest(s)
	got := recvType(t, guest, protocol.TypeData)
	if string(got.Payload) != "output before joining\r\n" {
		t.Fatalf("scrollback = %q", got.Payload)
	}
}

// A viewer ticket watches the terminal and cannot type into it.
func TestTunnelViewerIsReadOnly(t *testing.T) {
	r := newRelay(t)
	s := r.newSession()
	ctx := context.Background()

	host := r.attachHost(s, 100, 40)

	// Presented as an ordinary join, a viewer token is accepted read-only
	// rather than refused: one link shape works for everyone.
	viewer, _ := r.attachAs(s, protocol.RoleGuest, s.ViewerToken, protocol.RoleViewer)

	waitUntil(t, func() bool {
		b, ok := r.bridges.Get(s.SessionID)
		return ok && b.Guests() == 1
	})

	// Watching works.
	if err := host.Send(ctx, protocol.NewData([]byte("host output\r\n"))); err != nil {
		t.Fatalf("host send: %v", err)
	}
	if got := recvType(t, viewer, protocol.TypeData); string(got.Payload) != "host output\r\n" {
		t.Fatalf("viewer received %q", got.Payload)
	}

	// Typing does not reach the host.
	if err := viewer.Send(ctx, protocol.NewData([]byte("rm -rf /\r"))); err != nil {
		t.Fatalf("viewer send: %v", err)
	}
	assertNothing(t, host, 400*time.Millisecond)
}

// A full ticket can ask for less, which is the point of the -read-only flag.
func TestTunnelVoluntaryDowngradeToViewer(t *testing.T) {
	r := newRelay(t)
	s := r.newSession()
	ctx := context.Background()

	host := r.attachHost(s, 80, 24)
	guest, _ := r.attachAs(s, protocol.RoleViewer, s.GuestToken, protocol.RoleViewer)

	waitUntil(t, func() bool {
		b, ok := r.bridges.Get(s.SessionID)
		return ok && b.Guests() == 1
	})

	if err := guest.Send(ctx, protocol.NewData([]byte("should be ignored"))); err != nil {
		t.Fatalf("send: %v", err)
	}
	assertNothing(t, host, 400*time.Millisecond)
}

// The viewer token must not open a host tunnel.
func TestTunnelViewerCannotHost(t *testing.T) {
	r := newRelay(t)
	s := r.newSession()

	_, f := r.dial(protocol.Open{SessionID: s.SessionID, Role: protocol.RoleHost, Token: s.ViewerToken})
	if f.Type != protocol.TypeError {
		t.Fatalf("answered with %s, want ERROR", f.Type)
	}
}

// assertNothing fails if any frame arrives within d.
func assertNothing(t *testing.T, c tunnel.Conn, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	f, err := c.Recv(ctx)
	if err == nil {
		t.Fatalf("unexpected %s frame: %q", f.Type, f.Payload)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Recv = %v, want a timeout", err)
	}
}

func TestTunnelRejectsBadCredentials(t *testing.T) {
	r := newRelay(t)
	s := r.newSession()
	other := r.newSession()

	tests := []struct {
		name string
		open protocol.Open
	}{
		{"wrong host token", protocol.Open{SessionID: s.SessionID, Role: protocol.RoleHost, Token: other.HostToken}},
		{"wrong guest token", protocol.Open{SessionID: s.SessionID, Role: protocol.RoleGuest, Token: other.GuestToken}},
		{"guest token on host role", protocol.Open{SessionID: s.SessionID, Role: protocol.RoleHost, Token: s.GuestToken}},
		{"host token on guest role", protocol.Open{SessionID: s.SessionID, Role: protocol.RoleGuest, Token: s.HostToken}},
		{"empty token", protocol.Open{SessionID: s.SessionID, Role: protocol.RoleHost}},
		{"unknown session", protocol.Open{SessionID: "aaaaaaaaaaaaaaaaaaaaaaaaaa", Role: protocol.RoleHost, Token: s.HostToken}},
		{"malformed session", protocol.Open{SessionID: "../../etc", Role: protocol.RoleHost, Token: s.HostToken}},
		{"unknown role", protocol.Open{SessionID: s.SessionID, Role: protocol.Role("admin"), Token: s.HostToken}},
		{"viewer token as host", protocol.Open{SessionID: s.SessionID, Role: protocol.RoleHost, Token: s.ViewerToken}},
		{"another session's viewer token", protocol.Open{SessionID: s.SessionID, Role: protocol.RoleGuest, Token: other.ViewerToken}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, f := r.dial(tc.open)
			if f.Type != protocol.TypeError {
				t.Fatalf("answered with %s, want ERROR", f.Type)
			}
			var e protocol.Error
			if err := protocol.DecodeControl(f, &e); err != nil {
				t.Fatal(err)
			}
			// A bad token and an unknown session must be indistinguishable.
			if tc.name != "unknown role" && e.Code != protocol.ErrCodeUnauthorized {
				t.Fatalf("code = %q, want %q", e.Code, protocol.ErrCodeUnauthorized)
			}
			if e.Message == s.HostToken || e.Message == s.GuestToken {
				t.Fatal("error message leaked a token")
			}
		})
	}
}

func TestTunnelRejectsUnsupportedVersion(t *testing.T) {
	r := newRelay(t)
	s := r.newSession()

	_, f := r.dial(protocol.Open{
		Version: protocol.Version + 99, SessionID: s.SessionID,
		Role: protocol.RoleHost, Token: s.HostToken,
	})
	if f.Type != protocol.TypeError {
		t.Fatalf("answered with %s, want ERROR", f.Type)
	}
	var e protocol.Error
	if err := protocol.DecodeControl(f, &e); err != nil {
		t.Fatal(err)
	}
	if e.Code != protocol.ErrCodeVersionUnsupport {
		t.Fatalf("code = %q, want %q", e.Code, protocol.ErrCodeVersionUnsupport)
	}
}

func TestTunnelRequiresOpenFirst(t *testing.T) {
	r := newRelay(t)
	ctx := context.Background()

	conn, err := tunnel.Dial(ctx, r.url+"/api/v1/tunnel", tunnel.DialOptions{})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close("test over")

	// Terminal data before authenticating must be refused.
	if err := conn.Send(ctx, protocol.NewData([]byte("rm -rf /"))); err != nil {
		t.Fatalf("send: %v", err)
	}

	f := recvType(t, conn, protocol.TypeError)
	var e protocol.Error
	if err := protocol.DecodeControl(f, &e); err != nil {
		t.Fatal(err)
	}
	if e.Code != protocol.ErrCodeProtocol {
		t.Fatalf("code = %q, want %q", e.Code, protocol.ErrCodeProtocol)
	}
}

func TestTunnelRefusesSecondHost(t *testing.T) {
	r := newRelay(t)
	s := r.newSession()
	r.attachHost(s, 80, 24)

	// The session record is deleted once a host attaches to it, so a second
	// host is refused at authentication.
	_, f := r.dial(protocol.Open{SessionID: s.SessionID, Role: protocol.RoleHost, Token: s.HostToken})
	if f.Type != protocol.TypeError {
		t.Fatalf("second host answered with %s, want ERROR", f.Type)
	}
}

func TestTunnelGuestWithoutHostIsRefused(t *testing.T) {
	r := newRelay(t)
	s := r.newSession()

	_, f := r.dial(protocol.Open{SessionID: s.SessionID, Role: protocol.RoleGuest, Token: s.GuestToken})
	if f.Type != protocol.TypeError {
		t.Fatalf("answered with %s, want ERROR", f.Type)
	}
	var e protocol.Error
	if err := protocol.DecodeControl(f, &e); err != nil {
		t.Fatal(err)
	}
	if e.Code != protocol.ErrCodeSessionNotFound {
		t.Fatalf("code = %q, want %q", e.Code, protocol.ErrCodeSessionNotFound)
	}
}

func TestTunnelHostExitClosesGuests(t *testing.T) {
	r := newRelay(t)
	s := r.newSession()
	ctx := context.Background()

	host := r.attachHost(s, 80, 24)
	guest := r.attachGuest(s)
	waitUntil(t, func() bool {
		b, ok := r.bridges.Get(s.SessionID)
		return ok && b.Guests() == 1
	})

	// The host announces its exit; the guest must be told, not just cut off.
	code := 0
	if err := tunnel.SendControl(ctx, host, protocol.TypeClose,
		protocol.Close{Reason: "host shell exited", ExitCode: &code}); err != nil {
		t.Fatalf("host close: %v", err)
	}

	f := recvType(t, guest, protocol.TypeClose)
	var c protocol.Close
	if err := protocol.DecodeControl(f, &c); err != nil {
		t.Fatal(err)
	}
	if c.Reason != "host shell exited" {
		t.Fatalf("reason = %q", c.Reason)
	}
}

func TestTunnelHostDisconnectRemovesSession(t *testing.T) {
	r := newRelay(t)
	s := r.newSession()

	host := r.attachHost(s, 80, 24)
	host.Close("host gone")

	// A session whose host has gone cannot be joined, so it is dropped rather
	// than left to idle out its TTL.
	waitUntil(t, func() bool {
		_, ok := r.bridges.Get(s.SessionID)
		return !ok
	})
	if _, err := r.sessions.Get(context.Background(), s.SessionID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("session lookup after host left = %v, want ErrNotFound", err)
	}
}

func TestHealthReportsTunnels(t *testing.T) {
	r := newRelay(t)
	s := r.newSession()

	if got := r.bridges.Len(); got != 0 {
		t.Fatalf("tunnels = %d, want 0", got)
	}
	r.attachHost(s, 80, 24)
	waitUntil(t, func() bool { return r.bridges.Len() == 1 })
}
