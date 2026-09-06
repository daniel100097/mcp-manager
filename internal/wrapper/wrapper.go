// Package wrapper implements the "mcp-manager stdio NAME" launcher. Generated
// agent configs reference stdio MCPs through this wrapper so that the real
// command, arguments, and environment are resolved from the central config
// every time an agent starts the server.
package wrapper

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/daniel100097/mcp-manager/internal/config"
)

// Command is the executable name written into generated agent configs. Agents
// resolve it through their PATH.
const Command = "mcp-manager"

// Subcommand is the mcp-manager subcommand that launches a stdio MCP.
const Subcommand = "stdio"

// Launch describes a fully resolved stdio MCP process.
type Launch struct {
	// Path is the resolved executable path.
	Path string
	// Args is the full argument vector, including the program name.
	Args []string
	// Env is the complete child environment in KEY=value form.
	Env []string
}

// ConfigFlag and LocalConfigFlag are the long options the stdio subcommand
// accepts for the central config and its local overlay.
const (
	ConfigFlag      = "config"
	LocalConfigFlag = "config-local"
)

// Args returns the arguments that make mcp-manager launch the named MCP. Empty
// configPath and localConfigPath values select the wrapper's default
// locations at launch time.
func Args(name, configPath, localConfigPath string) []string {
	args := []string{Subcommand}
	if configPath != "" {
		args = append(args, "--"+ConfigFlag, configPath)
	}
	if localConfigPath != "" {
		args = append(args, "--"+LocalConfigFlag, localConfigPath)
	}
	return append(args, name)
}

// Parse recognizes a command line produced by Args and returns the wrapped MCP
// name. It accepts the bare command name or any path whose final element is
// mcp-manager, optionally with a Windows .exe suffix, followed by the stdio
// subcommand, any --config and --config-local options, and exactly one MCP
// name.
func Parse(command string, args []string) (string, bool) {
	if !isWrapperCommand(command) || len(args) == 0 || args[0] != Subcommand {
		return "", false
	}
	rest := args[1:]
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		option := strings.TrimPrefix(strings.TrimPrefix(rest[0], "-"), "-")
		flagName, _, inline := strings.Cut(option, "=")
		if flagName != ConfigFlag && flagName != LocalConfigFlag {
			return "", false
		}
		if inline {
			rest = rest[1:]
			continue
		}
		if len(rest) < 2 {
			return "", false
		}
		rest = rest[2:]
	}
	if len(rest) != 1 {
		return "", false
	}
	return rest[0], true
}

func isWrapperCommand(command string) bool {
	base := filepath.Base(strings.TrimSpace(command))
	if index := strings.LastIndex(base, `\`); index >= 0 {
		base = base[index+1:]
	}
	if strings.EqualFold(filepath.Ext(base), ".exe") {
		base = base[:len(base)-len(".exe")]
	}
	return base == Command
}

// Prepare resolves the named stdio MCP into a Launch. The child environment
// starts from environ, requires every envFrom variable to be present, and
// applies the literal env values from the central config on top.
func Prepare(cfg *config.Config, name string, environ []string) (Launch, error) {
	if cfg == nil {
		return Launch{}, errors.New("config must not be nil")
	}
	mcp, exists := cfg.MCPs[name]
	if !exists {
		return Launch{}, fmt.Errorf("MCP %q does not exist", name)
	}
	if mcp.Type != "stdio" {
		return Launch{}, fmt.Errorf("MCP %q uses the %s transport; only stdio MCPs can be launched", name, mcp.Type)
	}

	env := append([]string(nil), environ...)
	required := append([]string(nil), mcp.EnvFrom...)
	sort.Strings(required)
	for _, variable := range required {
		if _, present := lookupEnv(env, variable); !present {
			return Launch{}, fmt.Errorf("MCP %q requires environment variable %q, which is not set", name, variable)
		}
	}
	keys := make([]string, 0, len(mcp.Env))
	for key := range mcp.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = setEnv(env, key, mcp.Env[key])
	}

	path, err := exec.LookPath(mcp.Command)
	if err != nil {
		return Launch{}, fmt.Errorf("MCP %q: locate command %q: %w", name, mcp.Command, err)
	}
	args := make([]string, 0, len(mcp.Args)+1)
	args = append(args, mcp.Command)
	args = append(args, mcp.Args...)
	return Launch{Path: path, Args: args, Env: env}, nil
}

// Run starts the launch as a child process, connects the given streams, waits
// for it to exit, and returns its exit code. It is used on platforms without
// exec and by tests.
func Run(launch Launch, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if launch.Path == "" || len(launch.Args) == 0 {
		return 1, errors.New("launch must have a path and arguments")
	}
	command := exec.Command(launch.Path)
	command.Args = launch.Args
	command.Env = launch.Env
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	if err == nil {
		return 0, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		if code := exitError.ExitCode(); code >= 0 {
			return code, nil
		}
		return 1, nil
	}
	return 1, fmt.Errorf("run %q: %w", launch.Path, err)
}

func lookupEnv(environ []string, key string) (string, bool) {
	prefix := key + "="
	for index := len(environ) - 1; index >= 0; index-- {
		if strings.HasPrefix(environ[index], prefix) {
			return environ[index][len(prefix):], true
		}
	}
	return "", false
}

func setEnv(environ []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}
