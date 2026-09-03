//go:build windows

package client

import "os"

// Windows has no SIGWINCH; size changes would have to be polled. Sharing needs
// a pseudo-terminal, which is not implemented on Windows either, so this is a
// no-op rather than a polling loop nothing can use yet.

func notifyResize(chan<- os.Signal) {}

func stopResize(chan<- os.Signal) {}
