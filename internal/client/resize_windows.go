//go:build windows

package client

import (
	"os"
	"time"
)

// resizeInterval is how often the local console is measured.
//
// Fast enough that dragging a window edge settles almost as soon as it is
// released, cheap enough to leave running for a whole session: the measurement
// is one console API call.
const resizeInterval = 250 * time.Millisecond

// watchResize reports local terminal size changes on the returned channel, and
// returns a function that stops watching.
//
// Windows has no SIGWINCH — a console resize is not delivered to the program
// running in it at all — so the size is measured on a timer and a change is
// reported when one is seen.
func watchResize(f *os.File) (<-chan struct{}, func()) {
	changed := make(chan struct{}, 1)
	done := make(chan struct{})

	go func() {
		tick := time.NewTicker(resizeInterval)
		defer tick.Stop()
		cols, rows := terminalSize(f)
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				c, r := terminalSize(f)
				if c == cols && r == rows {
					continue
				}
				cols, rows = c, r
				notify(changed)
			}
		}
	}()

	var stopped bool
	return changed, func() {
		if stopped {
			return
		}
		stopped = true
		close(done)
	}
}
