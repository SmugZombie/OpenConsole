//go:build !windows

package client

import (
	"os"
	"os/signal"
	"syscall"
)

// notifyResize delivers a signal on ch whenever the local terminal is resized.
func notifyResize(ch chan<- os.Signal) {
	signal.Notify(ch, syscall.SIGWINCH)
}

// stopResize undoes notifyResize.
func stopResize(ch chan<- os.Signal) {
	signal.Stop(ch)
}
