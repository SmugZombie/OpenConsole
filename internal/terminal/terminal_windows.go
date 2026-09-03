//go:build windows

package terminal

// Windows has ConPTY, but wiring it up is a separate piece of work with its own
// process-creation path. Rather than fail to compile, the package builds and
// reports the limitation, so the relay and the rest of the CLI stay buildable
// and testable on Windows.

// Start reports that pseudo-terminals are unavailable on this platform.
func Start(Options) (*Terminal, error) { return nil, ErrUnsupported }

// Resize reports that pseudo-terminals are unavailable on this platform.
func (t *Terminal) Resize(cols, rows uint16) error { return ErrUnsupported }

// Close reports that pseudo-terminals are unavailable on this platform.
func (t *Terminal) Close() error { return ErrUnsupported }

func isTerminalClosed(error) bool { return false }
