package client

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
	"github.com/SmugZombie/OpenConsole/internal/tunnel"
)

// deadConn is a tunnel that refuses every write, standing in for a relay that
// has gone away mid-session.
type deadConn struct {
	mu     sync.Mutex
	closed bool
	block  chan struct{}
}

func newDeadConn() *deadConn { return &deadConn{block: make(chan struct{})} }

func (d *deadConn) Send(context.Context, protocol.Frame) error {
	return errors.New("connection reset by peer")
}

func (d *deadConn) Recv(ctx context.Context) (protocol.Frame, error) {
	select {
	case <-d.block:
		return protocol.Frame{}, tunnel.ErrClosed
	case <-ctx.Done():
		return protocol.Frame{}, ctx.Err()
	}
}

func (d *deadConn) Close(string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.closed {
		d.closed = true
		close(d.block)
	}
	return nil
}

func (d *deadConn) isClosed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

func TestSenderFailIsObservableImmediately(t *testing.T) {
	s := newSender(newDeadConn(), 4)

	if s.hasFailed() {
		t.Fatal("a fresh sender reports failure")
	}
	select {
	case <-s.Failed():
		t.Fatal("Failed() is closed before anything went wrong")
	default:
	}

	s.fail(errors.New("relay went away"))

	// The signal must be a broadcast, not a flag that only a polling reader
	// notices: an idle shell produces no reads to poll on.
	select {
	case <-s.Failed():
	case <-time.After(time.Second):
		t.Fatal("Failed() was not closed after a failure")
	}
	if !s.hasFailed() {
		t.Fatal("hasFailed is false after a failure")
	}
	if s.err() == nil {
		t.Fatal("err() is nil after a failure")
	}
}

func TestSenderRecordsOnlyTheFirstFailure(t *testing.T) {
	s := newSender(newDeadConn(), 4)
	s.fail(errors.New("first"))
	s.fail(errors.New("second"))
	if got := s.err(); got == nil || got.Error() != "first" {
		t.Fatalf("err() = %v, want the first failure", got)
	}
	s.fail(nil) // must not panic or close anything
}

// Benign endings are not failures worth reporting to a user.
func TestSenderIgnoresOrdinaryDisconnects(t *testing.T) {
	for _, err := range []error{tunnel.ErrClosed, context.Canceled} {
		s := newSender(newDeadConn(), 4)
		s.fail(err)
		if s.err() != nil {
			t.Fatalf("err() = %v, want nil for %v", s.err(), err)
		}
	}
}

// A full queue must fail sharing rather than block, or the host's own terminal
// would freeze because a relay stopped reading.
func TestSenderQueueFullFailsInsteadOfBlocking(t *testing.T) {
	s := newSender(newDeadConn(), 2)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			s.send(protocol.NewData([]byte("terminal output")))
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("send blocked when the queue filled")
	}
	if !s.hasFailed() {
		t.Fatal("overflowing the queue did not stop sharing")
	}
	if got := s.err(); !errors.Is(got, errRelayTooSlow) {
		t.Fatalf("err() = %v, want errRelayTooSlow", got)
	}
}

// Once sharing has stopped, queueing more output is a no-op rather than a
// second failure or a blocked write.
// What lands on a working terminal has to be readable; the raw transport error
// is kept for the summary printed after the shell exits.
func TestShareFailureMessageIsReadable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"queue full", errRelayTooSlow, errRelayTooSlow.Error()},
		{"timeout", context.DeadlineExceeded, "the relay stopped responding"},
		{
			"raw transport error",
			errors.New("tunnel: recv: failed to get reader: failed to read frame header: EOF"),
			"lost contact with the relay",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shareFailureMessage(tc.err); got != tc.want {
				t.Fatalf("shareFailureMessage(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestSenderSendIsNoOpAfterFailure(t *testing.T) {
	s := newSender(newDeadConn(), 1)
	s.fail(errors.New("gone"))

	for i := 0; i < 10; i++ {
		s.send(protocol.NewData([]byte("ignored")))
	}
	if got := s.err(); got == nil || got.Error() != "gone" {
		t.Fatalf("err() = %v, want the original failure", got)
	}
}
