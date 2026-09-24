package client

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// PromptMarker is put in front of the shared shell's prompt, so anyone who
// can see the screen — the host, or someone looking over their shoulder — can
// tell at a glance that the terminal is being shared.
const PromptMarker = "(>|<)"

// promptSetup is how to start a shell so that its prompt carries
// PromptMarker.
type promptSetup struct {
	Args []string
	Env  []string
	// Cleanup removes anything written to disk for the shell. It must only
	// run once the shell has exited, since the shell reads it at startup.
	Cleanup func()
}

// markPrompt works out how to start shell with PromptMarker in its prompt.
//
// A prompt is the user's own configuration, usually rebuilt by their rc files
// or a theme on every command, so setting PS1 in the environment would simply
// be overwritten. Instead each shell is started so that it loads the user's
// configuration as normal and then runs a small hook that puts the marker
// back whenever the prompt is redrawn without it.
//
// A shell this does not know is started unchanged: an unmarked prompt is
// better than a broken one.
func markPrompt(shell string, env []string) (promptSetup, error) {
	none := promptSetup{Env: env, Cleanup: func() {}}

	switch shellName(shell) {
	case "zsh":
		return zshPrompt(env)
	case "bash":
		return bashPrompt(env)
	case "sh", "dash", "ash", "ksh", "mksh":
		// These have no hook to run after the user's configuration, but they
		// take PS1 from the environment when nothing overrides it.
		ps1, ok := lookupEnv(env, "PS1")
		if !ok {
			ps1 = "$ "
		}
		return promptSetup{Env: setEnv(env, "PS1", PromptMarker+ps1), Cleanup: func() {}}, nil
	case "cmd":
		// cmd.exe reads its prompt from %PROMPT% and has no configuration
		// that would override it. $P$G is its built-in default.
		prompt, ok := lookupEnv(env, "PROMPT")
		if !ok {
			prompt = "$P$G"
		}
		return promptSetup{Env: setEnv(env, "PROMPT", PromptMarker+prompt), Cleanup: func() {}}, nil
	case "powershell", "pwsh":
		// -Command runs after the profile, so this wraps whatever prompt the
		// profile or a theme defined; -NoExit keeps the shell interactive.
		script := "${function:global:_openconsole_prompt} = ${function:prompt}; " +
			"function global:prompt { '" + PromptMarker + "' + (_openconsole_prompt) }"
		return promptSetup{Args: []string{"-NoExit", "-Command", script}, Env: env, Cleanup: func() {}}, nil
	}
	return none, nil
}

// shellName reduces a shell path to the name that identifies which shell it
// is: /usr/local/bin/zsh and C:\Windows\System32\CMD.EXE become zsh and cmd.
func shellName(shell string) string {
	// Split on both separators: filepath.Base on Unix leaves a Windows path
	// whole, and the other way round.
	if i := strings.LastIndexAny(shell, `/\`); i >= 0 {
		shell = shell[i+1:]
	}
	return strings.TrimSuffix(strings.ToLower(shell), ".exe")
}

// zshPrompt points ZDOTDIR at a directory whose startup files load the user's
// own and then install a precmd hook.
//
// zsh reads .zshenv and .zshrc from $ZDOTDIR, so this is the one place a hook
// can go that runs after the user's configuration without editing it. The
// user's ZDOTDIR, if any, rides along in OPENCONSOLE_ZDOTDIR and is restored
// before their .zshrc runs, so anything that consults it — including nested
// shells — sees what it would have seen anyway.
func zshPrompt(env []string) (promptSetup, error) {
	dir, err := os.MkdirTemp("", "openconsole-zsh-")
	if err != nil {
		return promptSetup{}, err
	}
	cleanup := func() { os.RemoveAll(dir) }

	// Shared by both files: switch ZDOTDIR back to the user's, or unset it if
	// they had none.
	const restore = `if [[ -n ${OPENCONSOLE_ZDOTDIR+x} ]]; then ZDOTDIR=$OPENCONSOLE_ZDOTDIR; else unset ZDOTDIR; fi
`
	zshenv := `# Written by openconsole: load the user's .zshenv, then keep zsh reading
# startup files from here so the .zshrc alongside this one runs.
_openconsole_dir=$ZDOTDIR
` + restore + `[[ -f ${ZDOTDIR:-$HOME}/.zshenv ]] && source ${ZDOTDIR:-$HOME}/.zshenv
# Their .zshenv may have chosen a ZDOTDIR; that is the one to restore later.
if [[ -n ${ZDOTDIR+x} ]]; then export OPENCONSOLE_ZDOTDIR=$ZDOTDIR; else unset OPENCONSOLE_ZDOTDIR; fi
ZDOTDIR=$_openconsole_dir
unset _openconsole_dir
`
	zshrc := `# Written by openconsole: load the user's .zshrc, then mark the prompt.
_openconsole_dir=$ZDOTDIR
` + restore + `unset OPENCONSOLE_ZDOTDIR
# The system zshrc has already run with ZDOTDIR pointing here, and settles
# some paths from it — on macOS, where history is kept. Point them back.
for _openconsole_v in HISTFILE SHELL_SESSION_DIR SHELL_SESSION_FILE; do
  if [[ ${(P)_openconsole_v} == $_openconsole_dir/* ]]; then
    typeset -g $_openconsole_v="${ZDOTDIR:-$HOME}/${${(P)_openconsole_v}#$_openconsole_dir/}"
  fi
done
unset _openconsole_v _openconsole_dir
[[ -f ${ZDOTDIR:-$HOME}/.zshrc ]] && source ${ZDOTDIR:-$HOME}/.zshrc
# Added last, so it runs after any theme's own precmd hook has rebuilt PROMPT.
# A prompt that opens with blank lines gets the marker after them, on the
# line people actually read.
_openconsole_prompt() {
  [[ $PROMPT == *'` + PromptMarker + `'* ]] && return
  local lead=
  while [[ $PROMPT == $'\n'* ]]; do lead+=$'\n'; PROMPT=${PROMPT#?}; done
  PROMPT=$lead'` + PromptMarker + `'$PROMPT
}
autoload -Uz add-zsh-hook && add-zsh-hook precmd _openconsole_prompt
`
	for name, body := range map[string]string{".zshenv": zshenv, ".zshrc": zshrc} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			cleanup()
			return promptSetup{}, err
		}
	}

	if zd, ok := lookupEnv(env, "ZDOTDIR"); ok {
		env = setEnv(env, "OPENCONSOLE_ZDOTDIR", zd)
	} else {
		env = unsetEnv(env, "OPENCONSOLE_ZDOTDIR")
	}
	return promptSetup{Env: setEnv(env, "ZDOTDIR", dir), Cleanup: cleanup}, nil
}

// bashPrompt starts bash with an rcfile that loads ~/.bashrc and then adds a
// PROMPT_COMMAND hook.
//
// --rcfile replaces ~/.bashrc rather than adding to it, which is why this one
// sources it first. The system-wide bashrc, where a distribution has one, is
// read either way.
func bashPrompt(env []string) (promptSetup, error) {
	f, err := os.CreateTemp("", "openconsole-bashrc-")
	if err != nil {
		return promptSetup{}, err
	}
	cleanup := func() { os.Remove(f.Name()) }

	rc := `# Written by openconsole: load the user's .bashrc, then mark the prompt.
[ -f ~/.bashrc ] && . ~/.bashrc
# A prompt that opens with blank lines, as a newline or a \n escape, gets the
# marker after them, on the line people actually read.
_openconsole_prompt() {
  case $PS1 in *'` + PromptMarker + `'*) return ;; esac
  local lead= rest=$PS1
  while :; do
    case $rest in
      $'\n'*) lead=$lead$'\n'; rest=${rest#?} ;;
      '\n'*) lead=$lead'\n'; rest=${rest#??} ;;
      *) break ;;
    esac
  done
  PS1=$lead'` + PromptMarker + `'$rest
}
# Last, so it runs after anything else that rebuilds PS1. A newline rather
# than ';' because an existing PROMPT_COMMAND may already end in one.
PROMPT_COMMAND="${PROMPT_COMMAND:+$PROMPT_COMMAND
}_openconsole_prompt"
`
	_, err = f.WriteString(rc)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		cleanup()
		return promptSetup{}, fmt.Errorf("writing bash rcfile: %w", err)
	}
	return promptSetup{Args: []string{"--rcfile", f.Name()}, Env: env, Cleanup: cleanup}, nil
}

// lookupEnv finds key in env. The last entry wins, as it does for exec.
func lookupEnv(env []string, key string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && envKeyIs(k, key) {
			return v, true
		}
	}
	return "", false
}

// setEnv returns env with key set to value, replacing any existing entry.
func setEnv(env []string, key, value string) []string {
	return append(unsetEnv(env, key), key+"="+value)
}

// unsetEnv returns env without any entry for key.
func unsetEnv(env []string, key string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); !envKeyIs(k, key) {
			out = append(out, kv)
		}
	}
	return out
}

// envKeyIs compares environment variable names the way the platform does:
// Windows ignores case, so %Prompt% and %PROMPT% are the same variable.
func envKeyIs(k, key string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(k, key)
	}
	return k == key
}
