//go:build !windows

package client

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SmugZombie/OpenConsole/internal/terminal"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// output collects everything runShare writes to the local screen.
type output struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (o *output) collect(r *os.File) {
	b := make([]byte, 4096)
	for {
		n, err := r.Read(b)
		if n > 0 {
			o.mu.Lock()
			o.buf.Write(b[:n])
			o.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

func (o *output) wait(t *testing.T, want string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if strings.Contains(o.String(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q on the local screen.\n--- got ---\n%s\n-----------", want, o.String())
}

// Losing the relay must not disturb the shell the host is working in.
//
// This is the regression test for a bug where a dropped tunnel reaped the
// terminal: a network blip would have destroyed the user's session, which is
// the opposite of what the documentation promises.
func TestShellSurvivesRelayFailure(t *testing.T) {
	term, err := terminal.Start(terminal.Options{
		Shell: "/bin/sh",
		Cols:  100,
		Rows:  30,
		Env:   append(os.Environ(), "PS1=$ "),
	})
	if err != nil {
		t.Fatalf("terminal.Start: %v", err)
	}
	defer term.Close()

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdinW.Close()
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdoutR.Close()

	var screen output
	go screen.collect(stdoutR)

	dead := newDeadConn()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := runShare(ctx, term, dead, stdinR, stdoutW, Allowlist{}, discardLogger(), crypter{})
		stdoutW.Close()
		done <- result{code, err}
	}()

	// The shell works before anything goes wrong.
	if _, err := stdinW.WriteString("echo before-failure\n"); err != nil {
		t.Fatal(err)
	}
	screen.wait(t, "before-failure", 10*time.Second)

	// That output was also the first attempt to reach the relay, which fails.
	// The host must be told, on their own screen, without waiting for the next
	// thing the shell happens to print.
	screen.wait(t, "[openconsole] sharing stopped", 10*time.Second)
	screen.wait(t, "your shell is still running", 10*time.Second)

	// The tunnel is dropped so the relay can reclaim the session.
	deadline := time.Now().Add(5 * time.Second)
	for !dead.isClosed() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !dead.isClosed() {
		t.Fatal("the tunnel was not closed after sharing stopped")
	}

	// The point of the whole exercise: the shell is still there.
	if _, err := stdinW.WriteString("echo still-alive-after-failure\n"); err != nil {
		t.Fatal(err)
	}
	screen.wait(t, "still-alive-after-failure", 10*time.Second)

	// And it still exits on the user's terms, with their exit code.
	if _, err := stdinW.WriteString("exit 3\n"); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-done:
		if got.code != 3 {
			t.Fatalf("exit code = %d, want 3 (the shell's own)", got.code)
		}
		if got.err == nil {
			t.Fatal("runShare should report why sharing stopped")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("runShare did not return after the shell exited")
	}
}
