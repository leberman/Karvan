//go:build !windows

package cli

import (
	"os/exec"
	"syscall"
)

// detach starts the child in its own session so it outlives Claude Code.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
