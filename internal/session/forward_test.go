package session

import (
	"context"
	"testing"
	"time"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
)

// openChannel sends a channel-open request from a guest.
func openChannel(t *testing.T, g *fakeStream, ch protocol.ChannelID, target string, port uint16) {
	t.Helper()
	f, err := protocol.NewControl(protocol.TypeOpen, protocol.ChannelOpen{
		Kind: protocol.ChannelKindTCP, Host: target, Port: port,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.Channel = ch
	g.in <- f
}

// nextOn waits for a frame of a given type on a given channel.
func (f *fakeStream) nextOn(t *testing.T, want protocol.Type, ch protocol.ChannelID) protocol.Frame {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case fr := <-f.out:
			if fr.Type == want && fr.Channel == ch {
				return fr
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s on channel %d", want, ch)
		}
	}
}

func TestForwardTranslatesChannelIDs(t *testing.T) {
	b, host, _ := startBridge(t)

	g := newFakeStream()
	go func() { _ = b.ServeGuest(context.Background(), g, GuestOptions{Access: AccessGuest}) }()
	waitGuests(t, b, 1)

	// The guest picks channel 1; the relay must renumber it, because another
	// guest will pick channel 1 too.
	openChannel(t, g, 1, "localhost", 5432)

	opened := host.next(t, protocol.TypeOpen)
	if opened.Channel.IsTerminal() {
		t.Fatal("a forward was opened on the terminal channel")
	}
	hostChan := opened.Channel

	var req protocol.ChannelOpen
	if err := protocol.DecodeControl(opened, &req); err != nil {
		t.Fatal(err)
	}
	if req.Target() != "localhost:5432" {
		t.Fatalf("target = %q", req.Target())
	}

	// Guest to host, renumbered.
	g.in <- protocol.Frame{Type: protocol.TypeData, Channel: 1, Payload: []byte("SELECT 1")}
	got := host.nextOn(t, protocol.TypeData, hostChan)
	if string(got.Payload) != "SELECT 1" {
		t.Fatalf("host received %q", got.Payload)
	}

	// Host back to guest, renumbered to the guest's own ID.
	host.in <- protocol.Frame{Type: protocol.TypeData, Channel: hostChan, Payload: []byte("1 row")}
	back := g.nextOn(t, protocol.TypeData, 1)
	if string(back.Payload) != "1 row" {
		t.Fatalf("guest received %q", back.Payload)
	}
}

// Two guests both picking channel 1 must not cross streams. This is the whole
// reason the relay keeps two numbering spaces.
func TestForwardKeepsGuestsApart(t *testing.T) {
	b, host, _ := startBridge(t)

	g1, g2 := newFakeStream(), newFakeStream()
	go func() { _ = b.ServeGuest(context.Background(), g1, GuestOptions{Access: AccessGuest}) }()
	go func() { _ = b.ServeGuest(context.Background(), g2, GuestOptions{Access: AccessGuest}) }()
	waitGuests(t, b, 2)
	// Drain the size each guest is sent on join, so the assertions below are
	// about forwarded traffic only.
	g1.next(t, protocol.TypeResize)
	g2.next(t, protocol.TypeResize)

	openChannel(t, g1, 1, "localhost", 1111)
	first := host.next(t, protocol.TypeOpen)
	openChannel(t, g2, 1, "localhost", 2222)
	second := host.next(t, protocol.TypeOpen)

	if first.Channel == second.Channel {
		t.Fatalf("both guests were given host channel %d", first.Channel)
	}

	// Reply on the first stream only.
	host.in <- protocol.Frame{Type: protocol.TypeData, Channel: first.Channel, Payload: []byte("for g1")}

	got := g1.nextOn(t, protocol.TypeData, 1)
	if string(got.Payload) != "for g1" {
		t.Fatalf("g1 received %q", got.Payload)
	}
	// The other guest must see nothing at all.
	g2.expectNothing(t, 300*time.Millisecond)
}

// A forwarded connection reaches whatever the host can reach, which is a far
// larger capability than typing. A viewer must not get one.
func TestForwardDeniedToViewer(t *testing.T) {
	b, host, _ := startBridge(t)

	viewer := newFakeStream()
	go func() { _ = b.ServeGuest(context.Background(), viewer, GuestOptions{Access: AccessViewer}) }()
	waitGuests(t, b, 1)
	viewer.next(t, protocol.TypeResize)

	openChannel(t, viewer, 1, "localhost", 5432)

	f := viewer.nextOn(t, protocol.TypeError, 1)
	var e protocol.Error
	if err := protocol.DecodeControl(f, &e); err != nil {
		t.Fatal(err)
	}
	if e.Code != protocol.ErrCodeForwardDenied {
		t.Fatalf("code = %q, want %q", e.Code, protocol.ErrCodeForwardDenied)
	}
	// And nothing reached the host.
	host.expectNothing(t, 300*time.Millisecond)
}

func TestForwardRejectsMalformedOpen(t *testing.T) {
	b, host, _ := startBridge(t)

	g := newFakeStream()
	go func() { _ = b.ServeGuest(context.Background(), g, GuestOptions{Access: AccessGuest}) }()
	waitGuests(t, b, 1)
	g.next(t, protocol.TypeResize)

	// No port, so nothing sensible to dial.
	f, err := protocol.NewControl(protocol.TypeOpen, protocol.ChannelOpen{
		Kind: protocol.ChannelKindTCP, Host: "localhost",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.Channel = 3
	g.in <- f

	g.nextOn(t, protocol.TypeError, 3)
	host.expectNothing(t, 300*time.Millisecond)
}

func TestForwardEnforcesChannelLimit(t *testing.T) {
	b, host, _ := startBridge(t)

	g := newFakeStream()
	go func() { _ = b.ServeGuest(context.Background(), g, GuestOptions{Access: AccessGuest}) }()
	waitGuests(t, b, 1)
	g.next(t, protocol.TypeResize)

	// Each forward costs the host a socket and a goroutine, so one guest must
	// not be able to exhaust it.
	for i := 1; i <= protocol.MaxChannels; i++ {
		openChannel(t, g, protocol.ChannelID(i), "localhost", 9000)
		host.next(t, protocol.TypeOpen)
	}

	openChannel(t, g, protocol.ChannelID(protocol.MaxChannels+1), "localhost", 9000)
	f := g.nextOn(t, protocol.TypeError, protocol.ChannelID(protocol.MaxChannels+1))
	var e protocol.Error
	if err := protocol.DecodeControl(f, &e); err != nil {
		t.Fatal(err)
	}
	if e.Code != protocol.ErrCodeChannelLimit {
		t.Fatalf("code = %q, want %q", e.Code, protocol.ErrCodeChannelLimit)
	}
}

// A forward the relay gives up on must be closed to both ends, not simply
// dropped.
//
// The failure this prevents is silence: the guest keeps a socket open that
// never delivers another byte and never ends, and the host holds the target
// connection open for a stream nobody is reading. Both wait, and only the
// relay knows why.
func TestRelayTellsBothEndsWhenItGivesUpOnAForward(t *testing.T) {
	b, host, _ := startBridge(t)

	// A guest whose stream accepts almost nothing, so its writer stalls and
	// the relay's queue behind it fills. Flow control normally prevents this;
	// the point here is what happens when something has gone wrong anyway.
	g := &fakeStream{
		in:     make(chan protocol.Frame, 64),
		out:    make(chan protocol.Frame, 1),
		closed: make(chan struct{}),
	}
	go func() { _ = b.ServeGuest(context.Background(), g, GuestOptions{Access: AccessGuest}) }()
	waitGuests(t, b, 1)

	openChannel(t, g, 1, "localhost", 5432)
	hostChan := host.next(t, protocol.TypeOpen).Channel

	// Overrun the queue. The guest is not draining, so these pile up behind a
	// writer that cannot move.
	for i := 0; i < forwardQueueDepth*2; i++ {
		host.in <- protocol.Frame{Type: protocol.TypeData, Channel: hostChan, Payload: []byte("bulk")}
	}

	// The host is told, so it can drop the target socket.
	closed := host.nextOn(t, protocol.TypeClose, hostChan)
	var c protocol.Close
	if err := protocol.DecodeControl(closed, &c); err != nil {
		t.Fatal(err)
	}
	if c.Reason == "" {
		t.Error("the host was told the forward ended, but not why")
	}

	// And the guest is told too, once its stream can take anything again.
	// Draining is what a guest coming back to life looks like.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case fr := <-g.out:
			if fr.Type == protocol.TypeClose && fr.Channel == 1 {
				return
			}
		case <-deadline:
			t.Fatal("the guest was never told its forward had ended")
		}
	}
}

// A CLOSE the relay has already accepted must still reach the guest when the
// forward is stopped in the same breath.
//
// The queue and the stop become ready together, so one round of this passes by
// luck about half the time — which is exactly how the bug survived. Twenty-five
// rounds do not.
func TestForwardCloseIsNotLostWhenTheForwardStops(t *testing.T) {
	b, host, _ := startBridge(t)

	g := newFakeStream()
	go func() { _ = b.ServeGuest(context.Background(), g, GuestOptions{Access: AccessGuest}) }()
	waitGuests(t, b, 1)

	for i := 1; i <= 25; i++ {
		guestChan := protocol.ChannelID(i)
		openChannel(t, g, guestChan, "localhost", 5432)
		hostChan := host.next(t, protocol.TypeOpen).Channel

		closeFrame := mustControl(protocol.TypeClose, protocol.Close{Reason: "target closed"})
		closeFrame.Channel = hostChan
		host.in <- closeFrame

		g.nextOn(t, protocol.TypeClose, guestChan)
	}
}

func TestForwardClosePropagates(t *testing.T) {
	b, host, _ := startBridge(t)

	g := newFakeStream()
	go func() { _ = b.ServeGuest(context.Background(), g, GuestOptions{Access: AccessGuest}) }()
	waitGuests(t, b, 1)

	openChannel(t, g, 1, "localhost", 5432)
	hostChan := host.next(t, protocol.TypeOpen).Channel

	// Host closes the far end; the guest is told on its own channel number.
	closeFrame := mustControl(protocol.TypeClose, protocol.Close{Reason: "target closed"})
	closeFrame.Channel = hostChan
	host.in <- closeFrame

	g.nextOn(t, protocol.TypeClose, 1)

	// The mapping is gone, so further host frames are dropped rather than
	// landing on a reused channel.
	host.in <- protocol.Frame{Type: protocol.TypeData, Channel: hostChan, Payload: []byte("late")}
	g.expectNothing(t, 300*time.Millisecond)
}

// When a guest leaves, the host still has a real socket open for each forward
// and needs telling.
func TestForwardClosedWhenGuestLeaves(t *testing.T) {
	b, host, _ := startBridge(t)

	g := newFakeStream()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = b.ServeGuest(context.Background(), g, GuestOptions{Access: AccessGuest})
	}()
	waitGuests(t, b, 1)

	openChannel(t, g, 1, "localhost", 5432)
	hostChan := host.next(t, protocol.TypeOpen).Channel

	g.Close("guest went away")
	<-done

	closed := host.nextOn(t, protocol.TypeClose, hostChan)
	var c protocol.Close
	if err := protocol.DecodeControl(closed, &c); err != nil {
		t.Fatal(err)
	}
	if c.Reason == "" {
		t.Fatal("the host should be told why the forward closed")
	}
}

// Channel IDs are never reused, so a late frame from a closed stream cannot
// land on a new one.
func TestForwardChannelIDsAreNotReused(t *testing.T) {
	b, host, _ := startBridge(t)

	g := newFakeStream()
	go func() { _ = b.ServeGuest(context.Background(), g, GuestOptions{Access: AccessGuest}) }()
	waitGuests(t, b, 1)

	openChannel(t, g, 1, "localhost", 1)
	first := host.next(t, protocol.TypeOpen).Channel

	closeFrame := mustControl(protocol.TypeClose, protocol.Close{})
	closeFrame.Channel = 1
	g.in <- closeFrame
	host.nextOn(t, protocol.TypeClose, first)

	openChannel(t, g, 1, "localhost", 2)
	second := host.next(t, protocol.TypeOpen).Channel

	if first == second {
		t.Fatalf("host channel %d was reused", first)
	}
}
