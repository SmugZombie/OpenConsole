//go:build windows

package terminal

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Windows is slower to start a shell than Unix is — cmd.exe has a console to
// negotiate with conhost, and PowerShell has a runtime to load — so these
// waits are longer than their Unix counterparts rather than tighter.
const (
	shellTimeout      = 20 * time.Second
	powerShellTimeout = 40 * time.Second
)

func TestStartRunsCmdAndEchoesOutput(t *testing.T) {
	term, err := Start(Options{Shell: "cmd.exe", Cols: 100, Rows: 30})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer term.Close()

	if term.Pid() <= 0 {
		t.Fatal("Pid should be set")
	}
	if !strings.HasSuffix(strings.ToLower(term.Command()), "cmd.exe") {
		t.Fatalf("Command = %q", term.Command())
	}

	if _, err := term.Write([]byte("echo openconsole-works\r\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	readUntil(t, term, "openconsole-works", shellTimeout)
}

func TestStartRunsPowerShell(t *testing.T) {
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		t.Skip("powershell.exe is not on PATH")
	}

	term, err := Start(Options{
		Shell: "powershell.exe",
		Args:  []string{"-NoLogo", "-NoProfile"},
		Cols:  100, Rows: 30,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer term.Close()

	// The answer is assembled by the shell, so finding it proves PowerShell
	// ran the line rather than the terminal merely echoing what was typed.
	if _, err := term.Write([]byte("[string]::Concat('open','console-works')\r\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	readUntil(t, term, "openconsole-works", powerShellTimeout)
}

func TestExitCodeIsReported(t *testing.T) {
	term, err := Start(Options{Shell: "cmd.exe", Args: []string{"/c", "exit", "7"}})
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
	term, err := Start(Options{Shell: "cmd.exe", Args: []string{"/c", "exit", "0"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer term.Close()

	// A pseudo-console holds the output pipe open on its own account, so this
	// only ends if closing the console when the shell exits actually breaks
	// the pipe. Without that, this test hangs rather than fails — which is
	// the point of having it.
	deadline := time.Now().Add(shellTimeout)
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
	term, err := Start(Options{Shell: "cmd.exe", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer term.Close()

	if err := term.Resize(132, 50); err != nil {
		t.Fatalf("Resize: %v", err)
	}

	// `mode con` reports the size the shell believes it has, which is what
	// proves the resize reached the console and not just this process. Only
	// the number is checked: the labels around it are translated.
	if _, err := term.Write([]byte("mode con\r\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := readUntil(t, term, "132", shellTimeout)
	if !strings.Contains(out, "132") {
		t.Fatalf("mode con did not report the new width: %q", out)
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
	t.Setenv("COMSPEC", `C:\Windows\System32\cmd.exe`)
	if got := (Options{}).shell(); got != `C:\Windows\System32\cmd.exe` {
		t.Fatalf("shell() = %q, want %%COMSPEC%%", got)
	}
	if got := (Options{Shell: "pwsh.exe"}).shell(); got != "pwsh.exe" {
		t.Fatalf("explicit shell should win, got %q", got)
	}
	// $SHELL is ignored here: on Windows it is usually an MSYS path that
	// CreateProcess cannot run.
	t.Setenv("SHELL", "/usr/bin/bash")
	if got := (Options{}).shell(); got != `C:\Windows\System32\cmd.exe` {
		t.Fatalf("shell() = %q, want %%COMSPEC%% regardless of $SHELL", got)
	}
	t.Setenv("COMSPEC", "")
	if got := (Options{}).shell(); got != DefaultShell {
		t.Fatalf("shell() = %q, want %q", got, DefaultShell)
	}
}

func TestEnvIsPassedToShell(t *testing.T) {
	term, err := Start(Options{
		Shell: "cmd.exe",
		Env:   append(os.Environ(), "OPENCONSOLE_TEST_VAR=hello-from-env"),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer term.Close()

	if _, err := term.Write([]byte("echo %OPENCONSOLE_TEST_VAR%\r\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	readUntil(t, term, "hello-from-env", shellTimeout)
}

func TestCloseEndsAShellThatIsStillRunning(t *testing.T) {
	term, err := Start(Options{Shell: "cmd.exe"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Closing takes the console away, which a shell treats as a hangup. If it
	// does not leave, Close terminates it — either way this must return, and
	// well inside the grace period plus a margin.
	done := make(chan error, 1)
	go func() { done <- term.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(hangupGrace + shellTimeout):
		t.Fatal("Close did not return")
	}

	if _, err := term.Wait(); err != nil {
		t.Fatalf("Wait after Close: %v", err)
	}
	term.Close() // must not panic
}
