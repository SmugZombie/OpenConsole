package client

import (
	"context"
	"errors"
	"sync"
)

// Flow control for one direction of one forwarded channel.
//
// Without it, a sender reads from its local socket as fast as the kernel will
// give it bytes and pushes them into the tunnel, where they queue up for a
// receiver that may be far slower. Something then has to give: drop the bytes
// and corrupt the stream, block and stall the terminal, or reset the channel.
// The answer is not to send them in the first place.
//
// A window is a byte credit. The receiver grants more as it consumes; the
// sender waits when the credit runs out. Credits are increments rather than
// absolute levels, so the two ends do not have to agree on a shared count.

// errWindowClosed means the channel went away while a sender was waiting.
var errWindowClosed = errors.New("forward: channel closed")

type window struct {
	mu     sync.Mutex
	avail  int
	closed bool
	// wait is closed and replaced whenever credit arrives, which lets a waiter
	// block on a channel and so honour a context at the same time. A
	// sync.Cond cannot be woken by a cancelled context.
	wait chan struct{}
}

func newWindow(initial int) *window {
	return &window{avail: initial, wait: make(chan struct{})}
}

// reserve blocks until n bytes of credit are available and takes them.
//
// n must not exceed the initial window, or there would be nothing to wait for;
// callers read in chunks far smaller than that.
func (w *window) reserve(ctx context.Context, n int) error {
	if n <= 0 {
		return nil
	}
	for {
		w.mu.Lock()
		if w.closed {
			w.mu.Unlock()
			return errWindowClosed
		}
		if w.avail >= n {
			w.avail -= n
			w.mu.Unlock()
			return nil
		}
		wait := w.wait
		w.mu.Unlock()

		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// grant adds credit and wakes anyone waiting.
func (w *window) grant(n int) {
	if n <= 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.avail += n
	close(w.wait)
	w.wait = make(chan struct{})
}

// close releases every waiter.
func (w *window) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.closed = true
	close(w.wait)
	w.wait = make(chan struct{})
}

// available reports the current credit, for tests.
func (w *window) available() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.avail
}
