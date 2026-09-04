//go:build !windows

package client

import (
	"os"
	"syscall"
	"testing"
	"time"
)

func TestWatchResizeReportsAWindowChange(t *testing.T) {
	changed, stop := watchResize(os.Stdout)
	defer stop()

	// SIGWINCH is ignored by default, so raising it here disturbs nothing but
	// the watcher under test.
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatalf("raising SIGWINCH: %v", err)
	}

	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("a window change was not reported")
	}

	// Stopping has to be complete: a host that has finished sharing should not
	// be left with a goroutine watching its terminal.
	stop()
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatalf("raising SIGWINCH: %v", err)
	}
	select {
	case <-changed:
		t.Fatal("a window change was reported after watching stopped")
	case <-time.After(200 * time.Millisecond):
	}

	stop() // must not panic
}

func TestNotifyDoesNotBlockWhenAChangeIsAlreadyPending(t *testing.T) {
	ch := make(chan struct{}, 1)
	// The second and third would block if a change were queued rather than
	// coalesced, and blocking here would stall a signal handler.
	notify(ch)
	notify(ch)
	notify(ch)
	if len(ch) != 1 {
		t.Fatalf("pending changes = %d, want 1", len(ch))
	}
}
