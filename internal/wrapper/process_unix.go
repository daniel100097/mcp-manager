//go:build !windows

package wrapper

import (
	"os/exec"
	"syscall"
)

func configureProcess(command *exec.Cmd) { command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func terminateProcess(command *exec.Cmd) { _ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM) }
func killProcess(command *exec.Cmd)      { _ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
