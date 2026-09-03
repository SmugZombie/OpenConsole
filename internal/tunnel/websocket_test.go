package tunnel

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
)

// pair returns a connected client and server Conn over a real WebSocket.
func pair(t *testing.T) (client, server Conn) {
	t.Helper()

	accepted := make(chan Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(w, r)
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		accepted <- c
		// Hold the handler open; closing it would tear down the connection.
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := Dial(ctx, srv.URL, DialOptions{})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { client.Close("test over") })

	select {
	case server = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("server never accepted the connection")
	}
	t.Cleanup(func() { server.Close("test over") })

	return client, server
}

func TestWebSocketDataRoundTrip(t *testing.T) {
	client, server := pair(t)
	ctx := context.Background()

	// Binary-clean: NUL, escapes and invalid UTF-8 must survive, since this is
	// the whole reason DATA is not JSON.
	payload := []byte{0x00, 0x1b, '[', '2', 'J', 0xff, 0xfe, '\r', '\n'}
	if err := client.Send(ctx, protocol.NewData(payload)); err != nil {
		t.Fatalf("Send: %v", err)
	}

	f, err := server.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if f.Type != protocol.TypeData {
		t.Fatalf("type = %s, want DATA", f.Type)
	}
	if !bytes.Equal(f.Payload, payload) {
		t.Fatalf("payload = %v, want %v", f.Payload, payload)
	}
}

func TestWebSocketControlRoundTrip(t *testing.T) {
	client, server := pair(t)
	ctx := context.Background()

	want := protocol.Open{
		Version:   protocol.Version,
		SessionID: "abc123",
		Role:      protocol.RoleGuest,
		Token:     "secret-token",
		Cols:      120,
		Rows:      40,
	}
	if err := SendControl(ctx, client, protocol.TypeOpen, want); err != nil {
		t.Fatalf("SendControl: %v", err)
	}

	f, err := server.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	var got protocol.Open
	if err := protocol.DecodeControl(f, &got); err != nil {
		t.Fatalf("DecodeControl: %v", err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestWebSocketBidirectional(t *testing.T) {
	client, server := pair(t)
	ctx := context.Background()

	if err := server.Send(ctx, protocol.NewData([]byte("from server"))); err != nil {
		t.Fatalf("server Send: %v", err)
	}
	f, err := client.Recv(ctx)
	if err != nil {
		t.Fatalf("client Recv: %v", err)
	}
	if string(f.Payload) != "from server" {
		t.Fatalf("payload = %q", f.Payload)
	}
}

func TestWebSocketCloseIsReportedAsErrClosed(t *testing.T) {
	client, server := pair(t)
	client.Close("done")

	_, err := server.Recv(context.Background())
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Recv after peer close = %v, want ErrClosed", err)
	}
}

func TestWebSocketRecvHonoursContext(t *testing.T) {
	client, _ := pair(t)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if _, err := client.Recv(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Recv = %v, want DeadlineExceeded", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	client, _ := pair(t)
	client.Close("first")
	client.Close("second") // must not panic
}

func TestTruncateReason(t *testing.T) {
	// RFC 6455 caps the close reason; an oversized one would make the library
	// refuse to send the close frame at all.
	short := "host exited"
	if got := truncateReason(short); got != short {
		t.Fatalf("truncateReason(%q) = %q", short, got)
	}
	long := strings.Repeat("x", 300)
	got := truncateReason(long)
	if len(got) > 123 {
		t.Fatalf("truncated reason is %d bytes, want <= 123", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("truncated reason should be marked as cut: %q", got)
	}
}

// Close must not block the caller when the peer never answers the handshake.
func TestCloseDoesNotBlockOnUnresponsivePeer(t *testing.T) {
	client, _ := pair(t) // the server side is parked, never reading

	done := make(chan struct{})
	go func() { defer close(done); client.Close("bye") }()

	select {
	case <-done:
	case <-time.After(closeGrace + 2*time.Second):
		t.Fatal("Close blocked on an unresponsive peer")
	}
}

func TestNormalizeWebSocketURL(t *testing.T) {
	tests := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "http://localhost:8080/api/v1/tunnel", want: "ws://localhost:8080/api/v1/tunnel"},
		{in: "https://console.example.com/api/v1/tunnel", want: "wss://console.example.com/api/v1/tunnel"},
		{in: "ws://localhost:8080/x", want: "ws://localhost:8080/x"},
		{in: "wss://localhost/x", want: "wss://localhost/x"},
		{in: "ftp://localhost/x", wantErr: true},
		{in: "/no-host", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tc := range tests {
		got, err := normalizeWebSocketURL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("normalizeWebSocketURL(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeWebSocketURL(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalizeWebSocketURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
