package client

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
	"github.com/SmugZombie/OpenConsole/internal/server"
	"github.com/SmugZombie/OpenConsole/internal/session"
)

// A forwarded connection crossing every layer for real: a local listener on the
// guest, a WebSocket to the relay, channel translation in the bridge, and a
// dial on the host. Nothing is stubbed but the terminal itself.
type forwardRig struct {
	t        *testing.T
	sessions *session.Manager
	sess     *session.Session
	guestFwd *guestForwards
	hostFwd  *hostForwards
}

func newForwardRig(t *testing.T, allow Allowlist) *forwardRig {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sessions := session.NewManager(session.Options{TTL: time.Minute})
	t.Cleanup(sessions.Close)
	bridges := session.NewBridges(log)

	api := server.NewAPI(sessions, bridges, log, "test", context.Background())
	srv := httptest.NewServer(api.Routes())
	t.Cleanup(srv.Close)

	sess, err := sessions.Create(context.Background())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	tunnelURL := srv.URL + "/api/v1/tunnel"

	// --- host ---------------------------------------------------------------
	hostConn, _, err := openTunnel(ctx, tunnelURL, protocol.Open{
		SessionID: sess.SessionID, Role: protocol.RoleHost, Token: sess.HostToken,
		Cols: 80, Rows: 24,
	})
	if err != nil {
		t.Fatalf("host openTunnel: %v", err)
	}
	t.Cleanup(func() { hostConn.Close("test over") })

	hostSend := func(f protocol.Frame) { _ = hostConn.Send(ctx, f) }
	hostFwd := newHostForwards(allow, hostSend, log)
	t.Cleanup(hostFwd.closeAll)

	go func() {
		for {
			f, err := hostConn.Recv(ctx)
			if err != nil {
				return
			}
			if !f.Channel.IsTerminal() {
				hostFwd.handle(ctx, f)
			}
		}
	}()

	// --- guest --------------------------------------------------------------
	guestConn, _, err := openTunnel(ctx, tunnelURL, protocol.Open{
		SessionID: sess.SessionID, Role: protocol.RoleGuest, Token: sess.GuestToken,
	})
	if err != nil {
		t.Fatalf("guest openTunnel: %v", err)
	}
	t.Cleanup(func() { guestConn.Close("test over") })

	guestFwd := newGuestForwards(
		func(f protocol.Frame) { _ = guestConn.Send(ctx, f) },
		func(string) {},
	)
	t.Cleanup(guestFwd.Close)

	go func() {
		for {
			f, err := guestConn.Recv(ctx)
			if err != nil {
				return
			}
			if !f.Channel.IsTerminal() {
				guestFwd.handle(f)
			}
		}
	}()

	return &forwardRig{t: t, sessions: sessions, sess: sess, guestFwd: guestFwd, hostFwd: hostFwd}
}

// listen starts a local forward and returns the address to connect to.
func (r *forwardRig) listen(target string, port uint16) string {
	r.t.Helper()
	addr, err := r.guestFwd.Listen(context.Background(), ForwardSpec{
		ListenAddr: "127.0.0.1:0",
		RemoteHost: target,
		RemotePort: port,
	})
	if err != nil {
		r.t.Fatalf("Listen: %v", err)
	}
	return addr
}

func TestForwardEndToEnd(t *testing.T) {
	_, echoPort := echoServer(t)

	allow, err := ParseAllowlist(net.JoinHostPort("127.0.0.1", itoa(int(echoPort))))
	if err != nil {
		t.Fatal(err)
	}
	rig := newForwardRig(t, allow)

	local := rig.listen("127.0.0.1", echoPort)

	conn, err := net.DialTimeout("tcp", local, 5*time.Second)
	if err != nil {
		t.Fatalf("dial the local forward: %v", err)
	}
	defer conn.Close()

	// Bytes go guest -> relay -> host -> echo server and all the way back.
	want := []byte("the quick brown fox jumps over the lazy dog")
	if _, err := conn.Write(want); err != nil {
		t.Fatalf("write: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	got := make([]byte, len(want))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("round trip returned %q, want %q", got, want)
	}
}

// Bulk transfer, to catch framing or ordering mistakes that a short string
// would sail past.
func TestForwardEndToEndBulk(t *testing.T) {
	_, echoPort := echoServer(t)

	allow, err := ParseAllowlist("any")
	if err != nil {
		t.Fatal(err)
	}
	rig := newForwardRig(t, allow)
	local := rig.listen("127.0.0.1", echoPort)

	conn, err := net.DialTimeout("tcp", local, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Larger than one read buffer, so it spans many frames.
	payload := bytes.Repeat([]byte("0123456789abcdef"), 8192) // 128 KiB

	done := make(chan error, 1)
	go func() {
		_, err := conn.Write(payload)
		done <- err
	}()

	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read back %d bytes: %v", len(payload), err)
	}
	if err := <-done; err != nil {
		t.Fatalf("write: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("the bulk round trip came back altered")
	}
}

// The host's refusal has to reach the person who opened the local socket, not
// leave them holding a connection that accepted and then did nothing.
func TestForwardEndToEndRefusedTargetClosesLocalSocket(t *testing.T) {
	_, echoPort := echoServer(t)

	// Allow something else entirely.
	allow, err := ParseAllowlist("127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	rig := newForwardRig(t, allow)
	local := rig.listen("127.0.0.1", echoPort)

	conn, err := net.DialTimeout("tcp", local, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(conn, make([]byte, 1)); err == nil {
		t.Fatal("a refused forward left the local socket open and silent")
	}
}

// The regression this flow control exists for.
//
// A target that produces data far faster than the guest reads it used to fill
// the relay's queue and reset the stream. With windows the host simply waits,
// and every byte arrives.
func TestForwardEndToEndSlowReaderDoesNotResetTheStream(t *testing.T) {
	// A server that pushes hard the moment it is connected to.
	const total = 4 << 20 // 4 MiB, far beyond one window
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	payload := bytes.Repeat([]byte("0123456789abcdef"), total/16)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.Write(payload)
			}()
		}
	}()

	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	var p int
	if _, err := fmtSscan(portStr, &p); err != nil {
		t.Fatal(err)
	}

	allow, err := ParseAllowlist("any")
	if err != nil {
		t.Fatal(err)
	}
	rig := newForwardRig(t, allow)
	local := rig.listen("127.0.0.1", uint16(p))

	conn, err := net.DialTimeout("tcp", local, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Read deliberately slowly, in small chunks with pauses, the way a guest on
	// a poor link behaves.
	got := make([]byte, 0, total)
	buf := make([]byte, 4096)
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	for len(got) < total {
		n, err := conn.Read(buf)
		if n > 0 {
			got = append(got, buf[:n]...)
		}
		if err != nil {
			t.Fatalf("read stalled after %d of %d bytes: %v", len(got), total, err)
		}
		if len(got)%(256<<10) < 4096 {
			time.Sleep(2 * time.Millisecond)
		}
	}

	if !bytes.Equal(got, payload) {
		t.Fatal("the stream arrived altered")
	}
}

// Two connections through one forward must not be spliced together.
func TestForwardEndToEndConcurrentConnections(t *testing.T) {
	_, echoPort := echoServer(t)

	allow, err := ParseAllowlist("any")
	if err != nil {
		t.Fatal(err)
	}
	rig := newForwardRig(t, allow)
	local := rig.listen("127.0.0.1", echoPort)

	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			conn, err := net.DialTimeout("tcp", local, 5*time.Second)
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()

			// A payload unique to this connection, so a crossed stream shows up
			// as the wrong bytes rather than as a hang.
			want := bytes.Repeat([]byte{byte('a' + i)}, 4096)
			if _, err := conn.Write(want); err != nil {
				errs <- err
				return
			}
			conn.SetReadDeadline(time.Now().Add(15 * time.Second))
			got := make([]byte, len(want))
			if _, err := io.ReadFull(conn, got); err != nil {
				errs <- err
				return
			}
			if !bytes.Equal(got, want) {
				errs <- io.ErrUnexpectedEOF
				return
			}
			errs <- nil
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
	}
}
