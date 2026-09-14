//go:build windows

package wrapper

import "os/exec"

func configureProcess(command *exec.Cmd) {}
func terminateProcess(command *exec.Cmd) { _ = command.Process.Kill() }
func killProcess(command *exec.Cmd)      { _ = command.Process.Kill() }
