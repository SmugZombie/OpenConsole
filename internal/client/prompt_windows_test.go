//go:build windows

package client

import (
	"os"
	"os/exec"
	"testing"
)

func TestCmdPromptIsMarked(t *testing.T) {
	env := unsetEnv(os.Environ(), "PROMPT")
	// $P$G draws the drive, so the marker is followed by it: (>|<)C:\...>
	startMarked(t, "cmd.exe", env, "", PromptMarker+`C:\`)
}

func TestPowerShellPromptIsMarked(t *testing.T) {
	for _, name := range []string{"powershell.exe", "pwsh.exe"} {
		t.Run(name, func(t *testing.T) {
			if _, err := exec.LookPath(name); err != nil {
				t.Skipf("%s is not installed", name)
			}
			startMarked(t, name, os.Environ(), "", PromptMarker+"PS ")
		})
	}
}
