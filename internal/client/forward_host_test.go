package client

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
)

// echoServer stands in for whatever the host can reach.
func echoServer(t *testing.T) (addr string, port uint16) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	var p int
	if _, err := fmtSscan(portStr, &p); err != nil {
		t.Fatal(err)
	}
	return host, uint16(p)
}

func fmtSscan(s string, p *int) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, io.ErrUnexpectedEOF
		}
		n = n*10 + int(c-'0')
	}
	*p = n
	return 1, nil
}

// collector captures frames the host would send back to the relay.
type collector struct {
	mu     sync.Mutex
	frames []protocol.Frame
	ch     chan protocol.Frame
}

func newCollector() *collector {
	return &collector{ch: make(chan protocol.Frame, 256)}
}

func (c *collector) send(f protocol.Frame) {
	c.mu.Lock()
	c.frames = append(c.frames, f)
	c.mu.Unlock()
	select {
	case c.ch <- f:
	default:
	}
}

func (c *collector) next(t *testing.T, want protocol.Type) protocol.Frame {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f := <-c.ch:
			if f.Type == want {
				return f
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", want)
		}
	}
}

func openFrame(t *testing.T, ch protocol.ChannelID, host string, port uint16) protocol.Frame {
	t.Helper()
	f, err := protocol.NewControl(protocol.TypeOpen, protocol.ChannelOpen{
		Kind: protocol.ChannelKindTCP, Host: host, Port: port,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.Channel = ch
	return f
}

func TestHostForwardDialsAllowedTarget(t *testing.T) {
	_, port := echoServer(t)

	allow, err := ParseAllowlist(net.JoinHostPort("127.0.0.1", itoa(int(port))))
	if err != nil {
		t.Fatal(err)
	}

	c := newCollector()
	h := newHostForwards(allow, c.send, discardLogger())
	defer h.closeAll()

	h.handle(context.Background(), openFrame(t, 1, "127.0.0.1", port))

	// The host confirms the connection before any bytes flow.
	if got := c.next(t, protocol.TypeOpen); got.Channel != 1 {
		t.Fatalf("ack on channel %d, want 1", got.Channel)
	}

	h.handle(context.Background(), protocol.Frame{
		Type: protocol.TypeData, Channel: 1, Payload: []byte("hello forward"),
	})

	echoed := c.next(t, protocol.TypeData)
	if string(echoed.Payload) != "hello forward" {
		t.Fatalf("echoed %q", echoed.Payload)
	}
	if echoed.Channel != 1 {
		t.Fatalf("echo came back on channel %d", echoed.Channel)
	}
}

// The allowlist is the whole security boundary for this feature.
func TestHostForwardRefusesUnlistedTarget(t *testing.T) {
	_, port := echoServer(t)

	allow, err := ParseAllowlist("localhost:1")
	if err != nil {
		t.Fatal(err)
	}

	c := newCollector()
	h := newHostForwards(allow, c.send, discardLogger())
	defer h.closeAll()

	h.handle(context.Background(), openFrame(t, 1, "127.0.0.1", port))

	f := c.next(t, protocol.TypeError)
	var e protocol.Error
	if err := protocol.DecodeControl(f, &e); err != nil {
		t.Fatal(err)
	}
	if e.Code != protocol.ErrCodeForwardDenied {
		t.Fatalf("code = %q, want %q", e.Code, protocol.ErrCodeForwardDenied)
	}
	if h.count() != 0 {
		t.Fatal("a refused forward left a socket open")
	}
}

// With no -allow-forward, nothing is reachable at all.
func TestHostForwardDeniedByDefault(t *testing.T) {
	_, port := echoServer(t)

	c := newCollector()
	h := newHostForwards(Allowlist{}, c.send, discardLogger())
	defer h.closeAll()

	h.handle(context.Background(), openFrame(t, 1, "127.0.0.1", port))

	f := c.next(t, protocol.TypeError)
	var e protocol.Error
	if err := protocol.DecodeControl(f, &e); err != nil {
		t.Fatal(err)
	}
	if e.Code != protocol.ErrCodeForwardDenied {
		t.Fatalf("code = %q, want %q", e.Code, protocol.ErrCodeForwardDenied)
	}
}

func TestHostForwardReportsUnreachableTarget(t *testing.T) {
	// Port 1 on loopback: allowed by policy, but nothing is listening.
	allow, err := ParseAllowlist("127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}

	c := newCollector()
	h := newHostForwards(allow, c.send, discardLogger())
	defer h.closeAll()

	h.handle(context.Background(), openFrame(t, 1, "127.0.0.1", 1))

	f := c.next(t, protocol.TypeError)
	var e protocol.Error
	if err := protocol.DecodeControl(f, &e); err != nil {
		t.Fatal(err)
	}
	// Refused because it could not be reached, not because it was forbidden:
	// the difference matters when someone is debugging their own setup.
	if e.Code != protocol.ErrCodeForwardFailed {
		t.Fatalf("code = %q, want %q", e.Code, protocol.ErrCodeForwardFailed)
	}
}

func TestHostForwardClosesOnGuestClose(t *testing.T) {
	_, port := echoServer(t)
	allow, err := ParseAllowlist("any")
	if err != nil {
		t.Fatal(err)
	}

	c := newCollector()
	h := newHostForwards(allow, c.send, discardLogger())
	defer h.closeAll()

	h.handle(context.Background(), openFrame(t, 1, "127.0.0.1", port))
	c.next(t, protocol.TypeOpen)
	if h.count() != 1 {
		t.Fatalf("open forwards = %d, want 1", h.count())
	}

	closeFrame, err := protocol.NewControl(protocol.TypeClose, protocol.Close{})
	if err != nil {
		t.Fatal(err)
	}
	closeFrame.Channel = 1
	h.handle(context.Background(), closeFrame)

	deadline := time.Now().Add(2 * time.Second)
	for h.count() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if h.count() != 0 {
		t.Fatal("the socket was not closed when the guest closed the channel")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
