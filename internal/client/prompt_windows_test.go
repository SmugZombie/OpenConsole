//go:build windows

package client

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCmdPromptIsMarked(t *testing.T) {
	env := unsetEnv(os.Environ(), "PROMPT")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// $P$G draws the working directory, so the marker is followed by its
	// drive: (>|<)D:\...>
	startMarked(t, "cmd.exe", env, "", PromptMarker+filepath.VolumeName(wd)+`\`)
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
