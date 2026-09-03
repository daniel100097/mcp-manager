//go:build windows

package wrapper

import "os"

// Exec runs the launch as a child process because Windows cannot replace the
// running process. The wrapper relays the standard streams and returns the
// child's exit code.
func Exec(launch Launch) (int, error) {
	return Run(launch, os.Stdin, os.Stdout, os.Stderr)
}
