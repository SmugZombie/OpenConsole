//go:build windows

package client

import (
	"os"
	"testing"
	"time"
)

func TestWatchResizeIsQuietWhileTheWindowIsUnchanged(t *testing.T) {
	changed, stop := watchResize(os.Stdout)

	// Polling must report changes, not ticks: a watcher that fired every
	// interval would put a resize frame on the wire four times a second for
	// the length of every session.
	select {
	case <-changed:
		t.Fatal("a change was reported although the window did not change")
	case <-time.After(3 * resizeInterval):
	}

	stop()
	stop() // must not panic
}
