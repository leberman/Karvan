//go:build windows

package cli

import (
	"os/exec"
	"syscall"
)

// detach starts the child without a console so it outlives Claude Code.
func detach(cmd *exec.Cmd) {
	const detachedProcess = 0x00000008
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess}
}
