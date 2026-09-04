//go:build !windows

package client

import (
	"os"
	"os/signal"
	"syscall"
)

// watchResize reports local terminal size changes on the returned channel, and
// returns a function that stops watching.
//
// Unix delivers SIGWINCH when the window changes, so nothing has to poll. The
// file is not needed: the signal says that some size changed, and the caller
// measures for itself.
func watchResize(*os.File) (<-chan struct{}, func()) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGWINCH)

	changed := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-sig:
				notify(changed)
			case <-done:
				return
			}
		}
	}()

	var stopped bool
	return changed, func() {
		if stopped {
			return
		}
		stopped = true
		signal.Stop(sig)
		close(done)
	}
}
