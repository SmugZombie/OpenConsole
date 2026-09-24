//go:build !windows

package client

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// shellEnv is a clean environment for a shell whose home is a fresh
// directory, so the test sees its own rc files rather than the developer's.
func shellEnv(t *testing.T, files map[string]string) (home string, env []string) {
	t.Helper()
	home = t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(home, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home, []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "TERM=xterm"}
}

func lookShell(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s is not installed", name)
	}
	return path
}

func TestZshPromptIsMarkedAfterTheUsersConfig(t *testing.T) {
	zsh := lookShell(t, "zsh")
	// The precmd stands in for a theme that rebuilds the prompt before every
	// command; the marker has to survive that, not just the first prompt.
	home, env := shellEnv(t, map[string]string{
		".zshrc": "precmd() { PROMPT='user-prompt> ' }\n",
	})

	marked := PromptMarker + "user-prompt> "
	out := startMarkedUntil(t, zsh, env,
		"echo \"zd=${ZDOTDIR-unset} hist=$HISTFILE\"\n",
		"a marked prompt after the command's output",
		func(out string) bool {
			_, after, ok := strings.Cut(out, "\r\nzd=")
			return ok && strings.Contains(after, marked)
		})

	// Anything the user's own startup consults must look as it would have
	// without openconsole in the way.
	if !strings.Contains(out, "zd=unset") {
		t.Errorf("ZDOTDIR leaked into the session:\n%q", out)
	}
	if !strings.Contains(out, "hist="+filepath.Join(home, ".zsh_history")) &&
		!strings.Contains(out, "hist=\r") {
		t.Errorf("history is not kept in the user's home:\n%q", out)
	}
}

func TestZshKeepsTheUsersZDOTDIR(t *testing.T) {
	zsh := lookShell(t, "zsh")
	home, env := shellEnv(t, nil)
	zd := filepath.Join(home, "zdot")
	if err := os.Mkdir(zd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(zd, ".zshrc"), []byte("PROMPT='from-zdotdir> '\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env = append(env, "ZDOTDIR="+zd)

	out := startMarked(t, zsh, env, "echo \"zd=$ZDOTDIR\"\n", "zd="+zd)
	if !strings.Contains(out, PromptMarker+"from-zdotdir> ") {
		t.Errorf("the .zshrc in the user's ZDOTDIR was not used, or not marked:\n%q", out)
	}
}

func TestBashPromptIsMarkedAfterTheUsersConfig(t *testing.T) {
	bash := lookShell(t, "bash")
	_, env := shellEnv(t, map[string]string{
		".bashrc": "PROMPT_COMMAND='PS1=\"user-prompt> \";'\n",
	})

	startMarked(t, bash, env, "", PromptMarker+"user-prompt> ")
}

func TestShPromptIsMarked(t *testing.T) {
	_, env := shellEnv(t, nil)
	startMarked(t, "/bin/sh", env, "", PromptMarker+"$ ")
}

// A prompt that starts with a blank line would otherwise leave the marker
// alone on that line, above the prompt it belongs to.
func TestMarkerGoesAfterLeadingBlankLines(t *testing.T) {
	t.Run("zsh", func(t *testing.T) {
		zsh := lookShell(t, "zsh")
		_, env := shellEnv(t, map[string]string{".zshrc": "PROMPT=$'\\n\\nuser-prompt> '\n"})
		startMarked(t, zsh, env, "", "\n"+PromptMarker+"user-prompt> ")
	})
	t.Run("bash newline", func(t *testing.T) {
		bash := lookShell(t, "bash")
		_, env := shellEnv(t, map[string]string{".bashrc": "PS1=$'\\nuser-prompt> '\n"})
		startMarked(t, bash, env, "", "\n"+PromptMarker+"user-prompt> ")
	})
	t.Run("bash escape", func(t *testing.T) {
		bash := lookShell(t, "bash")
		_, env := shellEnv(t, map[string]string{".bashrc": "PS1='\\n\\nuser-prompt> '\n"})
		startMarked(t, bash, env, "", "\n"+PromptMarker+"user-prompt> ")
	})
}
