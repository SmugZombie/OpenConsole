package client

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestWindowReserveTakesCredit(t *testing.T) {
	w := newWindow(100)
	if err := w.reserve(context.Background(), 40); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if got := w.available(); got != 60 {
		t.Fatalf("available = %d, want 60", got)
	}
	if err := w.reserve(context.Background(), 0); err != nil {
		t.Fatalf("a zero reserve should be free: %v", err)
	}
}

// The point of the whole exercise: a sender waits rather than outrunning the
// receiver.
func TestWindowReserveBlocksUntilCredit(t *testing.T) {
	w := newWindow(10)
	if err := w.reserve(context.Background(), 10); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- w.reserve(context.Background(), 5) }()

	select {
	case <-done:
		t.Fatal("reserve returned with no credit available")
	case <-time.After(100 * time.Millisecond):
	}

	w.grant(5)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reserve after grant: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a grant did not wake the waiter")
	}
}

// A waiter must not be stuck forever when the session ends.
func TestWindowReserveHonoursContext(t *testing.T) {
	w := newWindow(0)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if err := w.reserve(ctx, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reserve = %v, want DeadlineExceeded", err)
	}
}

func TestWindowCloseReleasesWaiters(t *testing.T) {
	w := newWindow(0)

	done := make(chan error, 1)
	go func() { done <- w.reserve(context.Background(), 1) }()
	time.Sleep(20 * time.Millisecond)

	w.close()

	select {
	case err := <-done:
		if !errors.Is(err, errWindowClosed) {
			t.Fatalf("reserve = %v, want errWindowClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not release the waiter")
	}

	// And a later grant does not resurrect it.
	w.grant(100)
	if err := w.reserve(context.Background(), 1); !errors.Is(err, errWindowClosed) {
		t.Fatalf("reserve after close = %v", err)
	}
}

// Credits are increments, so concurrent grants must all count.
func TestWindowGrantsAccumulate(t *testing.T) {
	w := newWindow(0)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.grant(10)
		}()
	}
	wg.Wait()

	if got := w.available(); got != 1000 {
		t.Fatalf("available = %d, want 1000", got)
	}
}

func TestWindowConcurrentSendersShareCredit(t *testing.T) {
	const total = 10000
	w := newWindow(0)

	var wg sync.WaitGroup
	taken := make(chan int, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := w.reserve(context.Background(), 100); err != nil {
				return
			}
			taken <- 100
		}()
	}

	// Exactly enough credit for everyone, granted in dribs.
	go func() {
		for i := 0; i < total/10; i++ {
			w.grant(10)
		}
	}()

	wg.Wait()
	close(taken)

	sum := 0
	for n := range taken {
		sum += n
	}
	// No sender may take credit that was never granted.
	if sum > total {
		t.Fatalf("senders took %d bytes, only %d were granted", sum, total)
	}
	if got := w.available(); got < 0 {
		t.Fatalf("available went negative: %d", got)
	}
}
