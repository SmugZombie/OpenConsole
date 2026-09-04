package terminal

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// readUntil reads from t until want appears or the deadline passes.
//
// A terminal is not a stream of tidy messages: a shell's banner, its prompt
// and the echo of what was typed all arrive interleaved with the answer, and
// how much of it appears is the shell's business. Waiting for a substring is
// the only assertion that holds on every platform.
func readUntil(t *testing.T, term *Terminal, want string, timeout time.Duration) string {
	t.Helper()

	var (
		mu    sync.Mutex
		buf   bytes.Buffer
		found = make(chan struct{})
	)
	go func() {
		p := make([]byte, 4096)
		for {
			n, err := term.Read(p)
			if n > 0 {
				mu.Lock()
				buf.Write(p[:n])
				done := strings.Contains(buf.String(), want)
				mu.Unlock()
				if done {
					close(found)
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	select {
	case <-found:
	case <-time.After(timeout):
		mu.Lock()
		got := buf.String()
		mu.Unlock()
		t.Fatalf("timed out waiting for %q; got %q", want, got)
	}
	mu.Lock()
	defer mu.Unlock()
	return buf.String()
}
