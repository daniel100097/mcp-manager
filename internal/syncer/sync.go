package syncer

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/daniel100097/mcp-manager/internal/config"
	"github.com/daniel100097/mcp-manager/internal/fileutil"
	"github.com/daniel100097/mcp-manager/internal/wrapper"
)

type Options struct {
	HomeDir       string
	UserConfigDir string
	InlineSecrets bool
	DryRun        bool
	LookupEnv     func(string) (string, bool)
	// ConfigPath is the central config the caller loaded. Generated wrapper
	// entries embed it as --config when it is not the default location, so
	// that "mcp-manager stdio NAME" finds the same file at launch time.
	ConfigPath string
	// LocalConfigPath is the local overlay the caller applied. Generated
	// wrapper entries embed it as --config-local when it is not the default
	// sibling of the central config.
	LocalConfigPath string
}

// renderSettings holds the per-sync inputs that shape every generated server
// entry.
type renderSettings struct {
	inlineSecrets bool
	lookupEnv     func(string) (string, bool)
	wrapStdio     bool
	// wrapperConfig is the --config value for generated wrapper entries, or
	// empty when the default central config location is in use.
	wrapperConfig string
	// wrapperLocalConfig is the --config-local value for generated wrapper
	// entries, or empty when the overlay sits next to the central config.
	wrapperLocalConfig string
}

type Change struct {
	Path        string
	Agent       config.Agent
	Scope       string
	ServerCount int
	Applied     bool
}

type Result struct {
	Changes   []Change
	Unchanged int
}

type fileFormat int

const (
	formatJSON fileFormat = iota
	formatTOML
)

type target struct {
	path      string
	agent     config.Agent
	scope     string
	projectID string
	key       string
	format    fileFormat
}

type plannedWrite struct {
	target target
	data   []byte
	mode   os.FileMode
	count  int
}

func DefaultOptions() (Options, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Options{}, fmt.Errorf("find home directory: %w", err)
	}
	userConfig, err := os.UserConfigDir()
	if err != nil {
		return Options{}, fmt.Errorf("find user config directory: %w", err)
	}
	return Options{
		HomeDir:       home,
		UserConfigDir: userConfig,
		LookupEnv:     os.LookupEnv,
	}, nil
}

func DefaultConfigPath() (string, error) {
	if override := os.Getenv("MCP_MANAGER_CONFIG"); override != "" {
		absolute, err := filepath.Abs(override)
		if err != nil {
			return "", fmt.Errorf("resolve MCP_MANAGER_CONFIG: %w", err)
		}
		return filepath.Clean(absolute), nil
	}
	userConfig, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find user config directory: %w", err)
	}
	return StandardConfigPath(userConfig), nil
}

// StandardConfigPath returns the central config location below the user
// config directory, ignoring the MCP_MANAGER_CONFIG override.
func StandardConfigPath(userConfigDir string) string {
	return filepath.Join(userConfigDir, "mcp-manager", "config.json")
}

// DefaultLocalConfigPath returns the local overlay for the given central
// config: MCP_MANAGER_CONFIG_LOCAL when set, otherwise the central path with
// a .local.json suffix.
func DefaultLocalConfigPath(configPath string) (string, error) {
	if override := os.Getenv("MCP_MANAGER_CONFIG_LOCAL"); override != "" {
		absolute, err := filepath.Abs(override)
		if err != nil {
			return "", fmt.Errorf("resolve MCP_MANAGER_CONFIG_LOCAL: %w", err)
		}
		return filepath.Clean(absolute), nil
	}
	return config.LocalPathFor(configPath), nil
}

func Sync(cfg *config.Config, options Options) (Result, error) {
	if cfg == nil {
		return Result{}, errors.New("config must not be nil")
	}
	if options.HomeDir == "" || options.UserConfigDir == "" {
		return Result{}, errors.New("home and user config directories must not be empty")
	}
	if options.LookupEnv == nil {
		options.LookupEnv = os.LookupEnv
	}

	targets, err := buildTargets(cfg, options)
	if err != nil {
		return Result{}, err
	}
	if err := rejectTargetCollisions(targets); err != nil {
		return Result{}, err
	}
	settings, err := newRenderSettings(cfg, options)
	if err != nil {
		return Result{}, err
	}

	plans := make([]plannedWrite, 0, len(targets))
	result := Result{}
	for _, destination := range targets {
		servers, err := renderServers(cfg, destination.agent, destination.projectID, settings)
		if err != nil {
			return Result{}, fmt.Errorf("generate %s MCP configuration for %s: %w", destination.agent, destination.scope, err)
		}
		plan, changed, err := prepareWrite(destination, servers)
		if err != nil {
			return Result{}, err
		}
		if !changed {
			result.Unchanged++
			continue
		}
		plans = append(plans, plan)
	}

	if !options.DryRun {
		for _, plan := range plans {
			if err := fileutil.WriteAtomic(plan.target.path, plan.data, plan.mode); err != nil {
				return Result{}, fmt.Errorf("write %q: %w", plan.target.path, err)
			}
		}
	}

	for _, plan := range plans {
		result.Changes = append(result.Changes, Change{
			Path:        plan.target.path,
			Agent:       plan.target.agent,
			Scope:       plan.target.scope,
			ServerCount: plan.count,
			Applied:     !options.DryRun,
		})
	}
	return result, nil
}

func newRenderSettings(cfg *config.Config, options Options) (renderSettings, error) {
	settings := renderSettings{
		inlineSecrets: options.InlineSecrets,
		lookupEnv:     options.LookupEnv,
		wrapStdio:     cfg.Options.WrapStdio(),
	}
	if !settings.wrapStdio {
		return settings, nil
	}
	configPath := filepath.Clean(StandardConfigPath(options.UserConfigDir))
	if options.ConfigPath != "" {
		absolute, err := filepath.Abs(options.ConfigPath)
		if err != nil {
			return renderSettings{}, fmt.Errorf("resolve central config path %q: %w", options.ConfigPath, err)
		}
		if absolute = filepath.Clean(absolute); absolute != configPath {
			configPath = absolute
			settings.wrapperConfig = absolute
		}
	}
	if options.LocalConfigPath != "" {
		absolute, err := filepath.Abs(options.LocalConfigPath)
		if err != nil {
			return renderSettings{}, fmt.Errorf("resolve local config path %q: %w", options.LocalConfigPath, err)
		}
		if absolute = filepath.Clean(absolute); absolute != filepath.Clean(config.LocalPathFor(configPath)) {
			settings.wrapperLocalConfig = absolute
		}
	}
	return settings, nil
}

func buildTargets(cfg *config.Config, options Options) ([]target, error) {
	targets := []target{
		{
			path: filepath.Join(options.HomeDir, ".codex", "config.toml"), agent: config.AgentCodex,
			scope: "global", key: "mcp_servers", format: formatTOML,
		},
		{
			path: filepath.Join(options.HomeDir, ".claude.json"), agent: config.AgentClaude,
			scope: "global", key: "mcpServers", format: formatJSON,
		},
		{
			path: filepath.Join(options.UserConfigDir, "opencode", "opencode.json"), agent: config.AgentOpenCode,
			scope: "global", key: "mcp", format: formatJSON,
		},
	}

	projectIDs := make([]string, 0, len(cfg.Projects))
	explicitRoots := make(map[string]bool, len(cfg.Projects))
	for id := range cfg.Projects {
		projectIDs = append(projectIDs, id)
		explicitRoots[filepath.Clean(cfg.Projects[id].Path)] = true
	}
	sort.Strings(projectIDs)
	discoveredOwners := map[string]string{}
	for _, id := range projectIDs {
		project := cfg.Projects[id]
		roots := []string{project.Path}
		if project.IncludeWorktrees {
			worktrees, err := discoverWorktrees(project.Path)
			if err != nil {
				return nil, fmt.Errorf("discover worktrees for project %q: %w", id, err)
			}
			for _, root := range worktrees {
				// An explicitly registered path always uses its own scope.
				if explicitRoots[root] {
					continue
				}
				if previous, exists := discoveredOwners[root]; exists {
					return nil, fmt.Errorf("worktree %q is discovered by projects %q and %q; register it explicitly or enable discovery for only one project in this repository", root, previous, id)
				}
				discoveredOwners[root] = id
				roots = append(roots, root)
			}
		}
		for _, root := range roots {
			targets = append(targets,
				target{
					path: filepath.Join(root, ".codex", "config.toml"), agent: config.AgentCodex,
					scope: id, projectID: id, key: "mcp_servers", format: formatTOML,
				},
				target{
					path: filepath.Join(root, ".mcp.json"), agent: config.AgentClaude,
					scope: id, projectID: id, key: "mcpServers", format: formatJSON,
				},
				target{
					path: filepath.Join(root, "opencode.json"), agent: config.AgentOpenCode,
					scope: id, projectID: id, key: "mcp", format: formatJSON,
				},
			)
		}
	}
	return targets, nil
}

func rejectTargetCollisions(targets []target) error {
	seen := make(map[string]target, len(targets))
	for _, current := range targets {
		absolute, err := filepath.Abs(current.path)
		if err != nil {
			return fmt.Errorf("resolve target path %q: %w", current.path, err)
		}
		clean := filepath.Clean(absolute)
		if previous, duplicate := seen[clean]; duplicate {
			return fmt.Errorf(
				"target collision at %q between %s/%s and %s/%s",
				clean, previous.agent, previous.scope, current.agent, current.scope,
			)
		}
		seen[clean] = current
	}
	return nil
}

func renderServers(cfg *config.Config, agent config.Agent, projectID string, settings renderSettings) (map[string]any, error) {
	var names []string
	var disabledAgents map[string][]config.Agent
	if projectID == "" {
		names = append(names, cfg.Global.MCPs...)
		disabledAgents = cfg.Global.DisabledAgents
	} else {
		project, exists := cfg.Projects[projectID]
		if !exists {
			return nil, fmt.Errorf("unknown project %q", projectID)
		}
		names = append(names, project.MCPs...)
		disabledAgents = project.DisabledAgents
	}
	sort.Strings(names)

	servers := make(map[string]any)
	for _, name := range names {
		mcp, exists := cfg.MCPs[name]
		if !exists {
			return nil, fmt.Errorf("scope references unknown MCP %q", name)
		}
		if agentDisabled(disabledAgents[name], agent) {
			continue
		}
		server, err := renderServer(name, mcp, agent, settings)
		if err != nil {
			return nil, err
		}
		servers[name] = server
	}
	return servers, nil
}

// wrapStdio replaces the real command line of a stdio MCP with the
// mcp-manager wrapper. Literal env values stay in the central config because
// the wrapper applies them at launch; envFrom is kept so that each agent still
// forwards those variables to the wrapper process.
func wrapStdio(name string, mcp config.MCP, settings renderSettings) config.MCP {
	if !settings.wrapStdio || mcp.Type != "stdio" {
		return mcp
	}
	wrapped := mcp
	wrapped.Command = wrapper.Command
	wrapped.Args = wrapper.Args(name, settings.wrapperConfig, settings.wrapperLocalConfig)
	wrapped.Env = nil
	return wrapped
}

func agentDisabled(disabledAgents []config.Agent, agent config.Agent) bool {
	for _, disabled := range disabledAgents {
		if disabled == agent {
			return true
		}
	}
	return false
}

func renderServer(
	name string,
	mcp config.MCP,
	agent config.Agent,
	settings renderSettings,
) (map[string]any, error) {
	mcp = wrapStdio(name, mcp, settings)
	environment := cloneMap(mcp.Env)
	headers := cloneMap(mcp.Headers)

	if settings.inlineSecrets {
		for _, variable := range mcp.EnvFrom {
			value, exists := settings.lookupEnv(variable)
			if !exists {
				return nil, fmt.Errorf("MCP %q requires unset environment variable %q for --inline-secrets", name, variable)
			}
			environment[variable] = value
		}
		for header, variable := range mcp.HeadersFrom {
			value, exists := settings.lookupEnv(variable)
			if !exists {
				return nil, fmt.Errorf("MCP %q requires unset environment variable %q for --inline-secrets", name, variable)
			}
			headers[header] = value
		}
	}

	switch agent {
	case config.AgentCodex:
		return renderCodex(mcp, environment, headers, settings.inlineSecrets), nil
	case config.AgentClaude:
		return renderClaude(mcp, environment, headers, settings.inlineSecrets), nil
	case config.AgentOpenCode:
		return renderOpenCode(mcp, environment, headers, settings.inlineSecrets), nil
	default:
		return nil, fmt.Errorf("unsupported agent %q", agent)
	}
}

func renderCodex(mcp config.MCP, environment, headers map[string]string, inline bool) map[string]any {
	server := map[string]any{}
	if mcp.Type == "stdio" {
		server["command"] = mcp.Command
		if len(mcp.Args) > 0 {
			server["args"] = mcp.Args
		}
		if len(environment) > 0 {
			server["env"] = environment
		}
		if !inline && len(mcp.EnvFrom) > 0 {
			server["env_vars"] = sortedStrings(mcp.EnvFrom)
		}
		return server
	}

	server["url"] = mcp.URL
	if len(headers) > 0 {
		server["http_headers"] = headers
	}
	if !inline && len(mcp.HeadersFrom) > 0 {
		server["env_http_headers"] = mcp.HeadersFrom
	}
	return server
}

func renderClaude(mcp config.MCP, environment, headers map[string]string, inline bool) map[string]any {
	server := map[string]any{"type": mcp.Type}
	if mcp.Type == "stdio" {
		server["command"] = mcp.Command
		if len(mcp.Args) > 0 {
			server["args"] = mcp.Args
		}
		if !inline {
			for _, variable := range mcp.EnvFrom {
				environment[variable] = "${" + variable + "}"
			}
		}
		if len(environment) > 0 {
			server["env"] = environment
		}
		return server
	}

	server["url"] = mcp.URL
	if !inline {
		for header, variable := range mcp.HeadersFrom {
			headers[header] = "${" + variable + "}"
		}
	}
	if len(headers) > 0 {
		server["headers"] = headers
	}
	return server
}

func renderOpenCode(mcp config.MCP, environment, headers map[string]string, inline bool) map[string]any {
	server := map[string]any{"enabled": true}
	if mcp.Type == "stdio" {
		server["type"] = "local"
		command := make([]string, 0, len(mcp.Args)+1)
		command = append(command, mcp.Command)
		command = append(command, mcp.Args...)
		server["command"] = command
		if !inline {
			for _, variable := range mcp.EnvFrom {
				environment[variable] = "{env:" + variable + "}"
			}
		}
		if len(environment) > 0 {
			server["environment"] = environment
		}
		return server
	}

	server["type"] = "remote"
	server["url"] = mcp.URL
	if !inline {
		for header, variable := range mcp.HeadersFrom {
			headers[header] = "{env:" + variable + "}"
		}
	}
	if len(headers) > 0 {
		server["headers"] = headers
	}
	return server
}

func prepareWrite(destination target, servers map[string]any) (plannedWrite, bool, error) {
	existing, mode, exists, err := readDestination(destination.path)
	if err != nil {
		return plannedWrite{}, false, err
	}
	if !exists && len(servers) == 0 {
		return plannedWrite{}, false, nil
	}

	var generated []byte
	switch destination.format {
	case formatJSON:
		generated, err = mergeJSON(existing, destination.key, servers)
	case formatTOML:
		generated, err = mergeTOML(existing, destination.key, servers)
	default:
		err = errors.New("unknown destination format")
	}
	if err != nil {
		return plannedWrite{}, false, fmt.Errorf("parse %s config %q: %w", destination.agent, destination.path, err)
	}
	if bytes.Equal(existing, generated) {
		return plannedWrite{}, false, nil
	}
	return plannedWrite{target: destination, data: generated, mode: mode, count: len(servers)}, true, nil
}

func readDestination(path string) ([]byte, os.FileMode, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0o600, false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("inspect destination %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, 0, false, fmt.Errorf("destination %q is a symlink; refusing to replace it", path)
	}
	if !info.Mode().IsRegular() {
		return nil, 0, false, fmt.Errorf("destination %q is not a regular file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, false, fmt.Errorf("read destination %q: %w", path, err)
	}
	return data, info.Mode().Perm(), true, nil
}

func cloneMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func sortedStrings(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}
