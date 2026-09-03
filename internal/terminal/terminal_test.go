//go:build !windows

package terminal

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// readUntil reads from t until want appears or the deadline passes.
func readUntil(t *testing.T, term *Terminal, want string, timeout time.Duration) string {
	t.Helper()

	var buf bytes.Buffer
	found := make(chan struct{})
	go func() {
		p := make([]byte, 4096)
		for {
			n, err := term.Read(p)
			if n > 0 {
				buf.Write(p[:n])
				if strings.Contains(buf.String(), want) {
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
		t.Fatalf("timed out waiting for %q; got %q", want, buf.String())
	}
	return buf.String()
}

func TestStartRunsAShellAndEchoesOutput(t *testing.T) {
	term, err := Start(Options{Shell: "/bin/sh", Cols: 100, Rows: 30})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer term.Close()

	if term.Pid() <= 0 {
		t.Fatal("Pid should be set")
	}
	if !strings.HasSuffix(term.Command(), "sh") {
		t.Fatalf("Command = %q", term.Command())
	}

	if _, err := term.Write([]byte("echo openconsole-works\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	readUntil(t, term, "openconsole-works", 10*time.Second)
}

func TestExitCodeIsReported(t *testing.T) {
	term, err := Start(Options{Shell: "/bin/sh", Args: []string{"-c", "exit 7"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer term.Close()

	code, err := term.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	// A non-zero exit is the user's business, not an error.
	if code != 7 {
		t.Fatalf("exit code = %d, want 7", code)
	}

	// Wait is idempotent: every caller sees the same result.
	code2, err := term.Wait()
	if code2 != 7 || err != nil {
		t.Fatalf("second Wait = (%d, %v)", code2, err)
	}

	select {
	case <-term.Done():
	case <-time.After(time.Second):
		t.Fatal("Done was not closed after Wait")
	}
}

func TestReadReturnsEOFAfterShellExits(t *testing.T) {
	term, err := Start(Options{Shell: "/bin/sh", Args: []string{"-c", "exit 0"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer term.Close()

	// Drain until the pty reports the far end is gone. Linux surfaces this as
	// EIO and macOS as EOF; both must reach the caller as io.EOF.
	deadline := time.Now().Add(10 * time.Second)
	p := make([]byte, 4096)
	for {
		if time.Now().After(deadline) {
			t.Fatal("never saw EOF after the shell exited")
		}
		_, err := term.Read(p)
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			return
		}
		t.Fatalf("Read = %v, want io.EOF", err)
	}
}

func TestResize(t *testing.T) {
	term, err := Start(Options{Shell: "/bin/sh", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer term.Close()

	if err := term.Resize(132, 50); err != nil {
		t.Fatalf("Resize: %v", err)
	}

	// The shell must see the new size, which is what proves SIGWINCH and the
	// ioctl actually reached the child rather than just the master.
	if _, err := term.Write([]byte("stty size\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := readUntil(t, term, "50 132", 10*time.Second)
	if !strings.Contains(out, "50 132") {
		t.Fatalf("stty size did not report 50x132: %q", out)
	}
}

func TestUnknownShellFailsClearly(t *testing.T) {
	_, err := Start(Options{Shell: "definitely-not-a-real-shell-xyz"})
	if err == nil {
		t.Fatal("expected an error for a missing shell")
	}
	if !strings.Contains(err.Error(), "definitely-not-a-real-shell-xyz") {
		t.Fatalf("error should name the shell: %v", err)
	}
}

func TestShellResolution(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	if got := (Options{}).shell(); got != "/bin/zsh" {
		t.Fatalf("shell() = %q, want $SHELL", got)
	}
	if got := (Options{Shell: "/bin/bash"}).shell(); got != "/bin/bash" {
		t.Fatalf("explicit shell should win, got %q", got)
	}
	t.Setenv("SHELL", "")
	if got := (Options{}).shell(); got != DefaultShell {
		t.Fatalf("shell() = %q, want %q", got, DefaultShell)
	}
}

func TestEnvIsPassedToShell(t *testing.T) {
	term, err := Start(Options{
		Shell: "/bin/sh",
		Env:   append(os.Environ(), "OPENCONSOLE_TEST_VAR=hello-from-env"),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer term.Close()

	if _, err := term.Write([]byte("echo $OPENCONSOLE_TEST_VAR\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	readUntil(t, term, "hello-from-env", 10*time.Second)
}

func TestCloseIsIdempotent(t *testing.T) {
	term, err := Start(Options{Shell: "/bin/sh"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := term.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	term.Close() // must not panic
}
