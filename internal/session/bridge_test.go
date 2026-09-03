package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// fakeStream is an in-memory Stream. It exists so the bridge can be tested
// without a transport at all, which is the point of the Stream interface.
type fakeStream struct {
	in     chan protocol.Frame // frames the bridge will Recv
	out    chan protocol.Frame // frames the bridge has Sent
	closed chan struct{}
}

func newFakeStream() *fakeStream {
	return &fakeStream{
		in:     make(chan protocol.Frame, 64),
		out:    make(chan protocol.Frame, 1024),
		closed: make(chan struct{}),
	}
}

func (f *fakeStream) Send(ctx context.Context, fr protocol.Frame) error {
	// Copy the payload: real transports serialise, and a test that shares the
	// slice would hide aliasing bugs.
	fr.Payload = append([]byte(nil), fr.Payload...)
	select {
	case f.out <- fr:
		return nil
	case <-f.closed:
		return errors.New("stream closed")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeStream) Recv(ctx context.Context) (protocol.Frame, error) {
	select {
	case fr := <-f.in:
		return fr, nil
	case <-f.closed:
		return protocol.Frame{}, io.EOF
	case <-ctx.Done():
		return protocol.Frame{}, ctx.Err()
	}
}

func (f *fakeStream) Close(string) error {
	select {
	case <-f.closed:
	default:
		close(f.closed)
	}
	return nil
}

// next waits for the next frame of type t, skipping others.
func (f *fakeStream) next(t *testing.T, want protocol.Type) protocol.Frame {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case fr := <-f.out:
			if fr.Type == want {
				return fr
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a %s frame", want)
		}
	}
}

func (f *fakeStream) expectNothing(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case fr := <-f.out:
		t.Fatalf("unexpected %s frame", fr.Type)
	case <-time.After(d):
	}
}

// startBridge attaches a host and returns the bridge plus the host's stream.
func startBridge(t *testing.T) (*Bridge, *fakeStream, context.CancelFunc) {
	t.Helper()
	b := newBridge("test-session", discardLogger())
	host := newFakeStream()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() { defer close(done); _ = b.ServeHost(ctx, host, 100, 40) }()

	// Wait for the host to be registered before any guest attaches.
	deadline := time.After(2 * time.Second)
	for !b.HostAttached() {
		select {
		case <-deadline:
			t.Fatal("host never attached")
		case <-time.After(time.Millisecond):
		}
	}
	t.Cleanup(func() {
		cancel()
		host.Close("")
		<-done
	})
	return b, host, cancel
}

func TestBridgeHostOutputReachesGuests(t *testing.T) {
	b, host, _ := startBridge(t)

	g1, g2 := newFakeStream(), newFakeStream()
	ctx := context.Background()
	go func() { _ = b.ServeGuest(ctx, g1) }()
	go func() { _ = b.ServeGuest(ctx, g2) }()

	waitGuests(t, b, 2)

	host.in <- protocol.NewData([]byte("hello world"))

	for i, g := range []*fakeStream{g1, g2} {
		f := g.next(t, protocol.TypeData)
		if string(f.Payload) != "hello world" {
			t.Fatalf("guest %d got %q, want %q", i, f.Payload, "hello world")
		}
	}
}

func TestBridgeGuestInputReachesHost(t *testing.T) {
	b, host, _ := startBridge(t)

	g := newFakeStream()
	go func() { _ = b.ServeGuest(context.Background(), g) }()
	waitGuests(t, b, 1)

	g.in <- protocol.NewData([]byte("ls -la\n"))

	f := host.next(t, protocol.TypeData)
	if string(f.Payload) != "ls -la\n" {
		t.Fatalf("host got %q", f.Payload)
	}
}

func TestBridgeGuestGetsSizeAndScrollbackOnJoin(t *testing.T) {
	b, host, _ := startBridge(t)

	// Produce output before anyone joins.
	host.in <- protocol.NewData([]byte("earlier output\n"))
	waitFor(t, func() bool { return b.scrollLen() > 0 })

	g := newFakeStream()
	go func() { _ = b.ServeGuest(context.Background(), g) }()

	// The size comes first so a client can shape its terminal before drawing.
	r := g.next(t, protocol.TypeResize)
	var size protocol.Resize
	if err := protocol.DecodeControl(r, &size); err != nil {
		t.Fatalf("decode resize: %v", err)
	}
	if size.Cols != 100 || size.Rows != 40 {
		t.Fatalf("size = %dx%d, want 100x40", size.Cols, size.Rows)
	}

	d := g.next(t, protocol.TypeData)
	if string(d.Payload) != "earlier output\n" {
		t.Fatalf("scrollback = %q, want %q", d.Payload, "earlier output\n")
	}
}

func TestBridgeResizeFromHostIsBroadcast(t *testing.T) {
	b, host, _ := startBridge(t)

	g := newFakeStream()
	go func() { _ = b.ServeGuest(context.Background(), g) }()
	waitGuests(t, b, 1)
	g.next(t, protocol.TypeResize) // the join-time size

	f, err := protocol.NewControl(protocol.TypeResize, protocol.Resize{Cols: 132, Rows: 50})
	if err != nil {
		t.Fatal(err)
	}
	host.in <- f

	got := g.next(t, protocol.TypeResize)
	var size protocol.Resize
	if err := protocol.DecodeControl(got, &size); err != nil {
		t.Fatal(err)
	}
	if size.Cols != 132 || size.Rows != 50 {
		t.Fatalf("size = %dx%d, want 132x50", size.Cols, size.Rows)
	}
	if c, r := b.Size(); c != 132 || r != 50 {
		t.Fatalf("bridge size = %dx%d, want 132x50", c, r)
	}
}

// A guest must not be able to resize the host's real terminal.
func TestBridgeIgnoresResizeFromGuest(t *testing.T) {
	b, host, _ := startBridge(t)

	g := newFakeStream()
	go func() { _ = b.ServeGuest(context.Background(), g) }()
	waitGuests(t, b, 1)

	f, err := protocol.NewControl(protocol.TypeResize, protocol.Resize{Cols: 1, Rows: 1})
	if err != nil {
		t.Fatal(err)
	}
	g.in <- f

	host.expectNothing(t, 200*time.Millisecond)
	if c, r := b.Size(); c != 100 || r != 40 {
		t.Fatalf("guest changed the host size to %dx%d", c, r)
	}
}

func TestBridgeRefusesSecondHost(t *testing.T) {
	b, _, _ := startBridge(t)

	err := b.ServeHost(context.Background(), newFakeStream(), 80, 24)
	if !errors.Is(err, ErrHostAlreadyAttached) {
		t.Fatalf("second host got %v, want ErrHostAlreadyAttached", err)
	}
}

func TestBridgeRefusesGuestWithoutHost(t *testing.T) {
	b := newBridge("s", discardLogger())
	if err := b.ServeGuest(context.Background(), newFakeStream()); !errors.Is(err, ErrNoHost) {
		t.Fatalf("guest without host got %v, want ErrNoHost", err)
	}
}

func TestBridgeHostDisconnectEndsSession(t *testing.T) {
	b, host, _ := startBridge(t)

	g := newFakeStream()
	guestDone := make(chan error, 1)
	go func() { guestDone <- b.ServeGuest(context.Background(), g) }()
	waitGuests(t, b, 1)

	host.Close("host gone")

	select {
	case <-guestDone:
	case <-time.After(2 * time.Second):
		t.Fatal("guest was not disconnected when the host left")
	}
	if b.HostAttached() {
		t.Fatal("bridge still reports a host")
	}
}

func TestBridgeHostCloseFrameIsForwarded(t *testing.T) {
	b, host, _ := startBridge(t)

	g := newFakeStream()
	go func() { _ = b.ServeGuest(context.Background(), g) }()
	waitGuests(t, b, 1)

	code := 0
	f, err := protocol.NewControl(protocol.TypeClose, protocol.Close{Reason: "exited", ExitCode: &code})
	if err != nil {
		t.Fatal(err)
	}
	host.in <- f

	got := g.next(t, protocol.TypeClose)
	var c protocol.Close
	if err := protocol.DecodeControl(got, &c); err != nil {
		t.Fatal(err)
	}
	if c.Reason != "exited" {
		t.Fatalf("reason = %q", c.Reason)
	}
}

func TestBridgePingIsAnswered(t *testing.T) {
	_, host, _ := startBridge(t)

	host.in <- protocol.Frame{Type: protocol.TypePing, Payload: []byte("probe")}

	f := host.next(t, protocol.TypePong)
	if string(f.Payload) != "probe" {
		t.Fatalf("pong echoed %q, want %q", f.Payload, "probe")
	}
}

// A guest that stops draining must be dropped, not allowed to stall the host.
func TestBridgeDropsSlowGuest(t *testing.T) {
	b, host, _ := startBridge(t)

	slow := newFakeStream()
	slow.out = make(chan protocol.Frame) // unbuffered and never read
	done := make(chan error, 1)
	go func() { done <- b.ServeGuest(context.Background(), slow) }()
	waitGuests(t, b, 1)

	// Overflow the guest's queue by a wide margin.
	for i := 0; i < guestQueueDepth*3; i++ {
		host.in <- protocol.NewData([]byte("output"))
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("slow guest was not dropped")
	}

	// The host must still be serving after the drop.
	host.in <- protocol.Frame{Type: protocol.TypePing, Payload: []byte("alive")}
	if f := host.next(t, protocol.TypePong); string(f.Payload) != "alive" {
		t.Fatal("host stalled after dropping a slow guest")
	}
}

func waitGuests(t *testing.T, b *Bridge, n int) {
	t.Helper()
	waitFor(t, func() bool { return b.Guests() == n })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatal("condition not met in time")
		case <-time.After(time.Millisecond):
		}
	}
}

// scrollLen exposes buffered scrollback for tests.
func (b *Bridge) scrollLen() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.scroll.Len()
}
