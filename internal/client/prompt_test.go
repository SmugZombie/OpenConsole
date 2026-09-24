package client

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SmugZombie/OpenConsole/internal/terminal"
)

func TestShellName(t *testing.T) {
	for in, want := range map[string]string{
		"/bin/zsh":                               "zsh",
		"/usr/local/bin/bash":                    "bash",
		"zsh":                                    "zsh",
		`C:\Windows\System32\CMD.EXE`:            "cmd",
		`C:\Program Files\PowerShell\7\pwsh.exe`: "pwsh",
		"powershell":                             "powershell",
	} {
		if got := shellName(in); got != want {
			t.Errorf("shellName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnknownShellIsStartedUnchanged(t *testing.T) {
	env := []string{"HOME=/home/x", "PS1=keep> "}
	p, err := markPrompt("/usr/bin/nu", env)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Cleanup()
	if len(p.Args) != 0 {
		t.Errorf("Args = %q, want none", p.Args)
	}
	if strings.Join(p.Env, "\n") != strings.Join(env, "\n") {
		t.Errorf("Env = %q, want %q", p.Env, env)
	}
}

func TestSetEnvReplacesRatherThanDuplicates(t *testing.T) {
	env := setEnv([]string{"A=1", "PS1=old", "B=2"}, "PS1", "new")
	if got, _ := lookupEnv(env, "PS1"); got != "new" {
		t.Errorf("PS1 = %q, want new", got)
	}
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "PS1=") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("PS1 appears %d times in %q", n, env)
	}
}

// startMarked starts shell the way Share does, types input, and returns
// everything it prints until want appears.
func startMarked(t *testing.T, shell string, env []string, input, want string) string {
	t.Helper()
	return startMarkedUntil(t, shell, env, input, want, func(out string) bool {
		return strings.Contains(out, want)
	})
}

// startMarkedUntil is startMarked for output that no single substring
// captures; desc says what done is waiting for.
func startMarkedUntil(t *testing.T, shell string, env []string, input, desc string, done func(string) bool) string {
	t.Helper()
	p, err := markPrompt(shell, env)
	if err != nil {
		t.Fatalf("markPrompt: %v", err)
	}
	t.Cleanup(p.Cleanup)

	term, err := terminal.Start(terminal.Options{Shell: shell, Args: p.Args, Env: p.Env, Cols: 120, Rows: 30})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { term.Close() })

	if input != "" {
		if _, err := term.Write([]byte(input)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	return readTerminalUntil(t, term, desc, done, 15*time.Second)
}

// readTerminalUntil reads from term until done accepts what has arrived or
// the deadline passes.
func readTerminalUntil(t *testing.T, term *terminal.Terminal, desc string, done func(string) bool, timeout time.Duration) string {
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
				ok := done(buf.String())
				mu.Unlock()
				if ok {
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
		defer mu.Unlock()
		t.Fatalf("timed out waiting for %s; got %q", desc, buf.String())
	}
	mu.Lock()
	defer mu.Unlock()
	return buf.String()
}
