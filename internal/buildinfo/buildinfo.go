package buildinfo

import "fmt"

// These values are replaced by the release workflow through -ldflags.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

func String() string {
	return fmt.Sprintf("mcp-manager %s (commit %s, built %s)", Version, Commit, Date)
}
