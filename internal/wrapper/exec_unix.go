//go:build !windows

package wrapper

import (
	"fmt"
	"syscall"
)

// Exec replaces the current process with the launch so that the agent talks to
// the MCP server directly and signals reach it without a relay. It only
// returns when the replacement fails.
func Exec(launch Launch) (int, error) {
	if err := syscall.Exec(launch.Path, launch.Args, launch.Env); err != nil {
		return 1, fmt.Errorf("exec %q: %w", launch.Path, err)
	}
	return 0, nil
}
