// Package importer reads agent-native MCP configuration and merges its
// transport definitions into the central mcp-manager configuration.
package importer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/tidwall/jsonc"

	"github.com/daniel100097/mcp-manager/internal/config"
	"github.com/daniel100097/mcp-manager/internal/wrapper"
)

var (
	identifierPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	claudeEnvReference   = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)
	openCodeEnvReference = regexp.MustCompile(
		`^\{env:([A-Za-z_][A-Za-z0-9_]*)\}$`,
	)
)

// Options selects the native agent configuration and its scope. Leave both
// ProjectID and ProjectPath empty to import the global configuration. Set both
// to import a project configuration; the project is registered when needed.
type Options struct {
	Agent         config.Agent
	HomeDir       string
	UserConfigDir string
	ProjectID     string
	ProjectPath   string
}

// Result describes a successful in-memory import. Import never writes the
// central configuration file.
type Result struct {
	SourcePath        string
	Agent             config.Agent
	Scope             string
	Imported          int
	Skipped           int
	Added             int
	Activated         int
	Unchanged         int
	ProjectRegistered bool
}

// DefaultOptions returns options rooted at the current user's standard agent
// configuration directories. The caller still selects Agent and, optionally,
// a project.
func DefaultOptions() (Options, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Options{}, fmt.Errorf("find home directory: %w", err)
	}
	userConfig, err := os.UserConfigDir()
	if err != nil {
		return Options{}, fmt.Errorf("find user config directory: %w", err)
	}
	return Options{HomeDir: home, UserConfigDir: userConfig}, nil
}

// Import reads one native agent configuration and returns a merged copy of
// existing. A nil pointer or a zero Config is treated as an empty version 2
// configuration. On every error, existing remains unchanged.
func Import(existing *config.Config, options Options) (*config.Config, Result, error) {
	selection, err := selectSource(existing, options)
	if err != nil {
		return nil, Result{}, err
	}

	data, err := os.ReadFile(selection.path)
	if err != nil {
		return nil, Result{}, fmt.Errorf(
			"read %s %s MCP config %q: %w",
			options.Agent, selection.scope, selection.path, err,
		)
	}

	servers, skipped, err := parse(options.Agent, data)
	if err != nil {
		return nil, Result{}, fmt.Errorf(
			"parse %s %s MCP config %q: %w",
			options.Agent, selection.scope, selection.path, err,
		)
	}

	merged := cloneConfig(existing)
	if merged.Version == 0 {
		merged.Version = config.CurrentVersion
	}
	if merged.Projects == nil {
		merged.Projects = map[string]config.Project{}
	}
	if merged.MCPs == nil {
		merged.MCPs = map[string]config.MCP{}
	}
	if merged.Global.MCPs == nil {
		merged.Global.MCPs = []string{}
	}
	if merged.Global.DisabledAgents == nil {
		merged.Global.DisabledAgents = map[string][]config.Agent{}
	}

	result := Result{
		SourcePath: selection.path,
		Agent:      options.Agent,
		Scope:      selection.scope,
		Imported:   len(servers),
		Skipped:    skipped,
	}
	if selection.projectID != "" {
		project, exists := merged.Projects[selection.projectID]
		result.ProjectRegistered = !exists
		if !exists {
			project = config.Project{
				Path:           selection.projectPath,
				MCPs:           []string{},
				DisabledAgents: map[string][]config.Agent{},
			}
		} else {
			if project.MCPs == nil {
				project.MCPs = []string{}
			}
			if project.DisabledAgents == nil {
				project.DisabledAgents = map[string][]config.Agent{}
			}
		}
		merged.Projects[selection.projectID] = project
	}

	names := sortedKeys(servers)
	for _, name := range names {
		incoming, err := unwrapManaged(merged, name, servers[name])
		if err != nil {
			return nil, Result{}, err
		}
		current, exists := merged.MCPs[name]
		if !exists {
			merged.MCPs[name] = incoming
			activate(merged, name, selection.projectID)
			result.Added++
			continue
		}
		if !sameTransport(current, incoming) {
			return nil, Result{}, fmt.Errorf(
				"import MCP %q: transport conflicts with the existing central definition",
				name,
			)
		}
		if activate(merged, name, selection.projectID) {
			result.Activated++
		} else {
			result.Unchanged++
		}
	}

	validated, err := validateMerged(merged, options.HomeDir)
	if err != nil {
		return nil, Result{}, fmt.Errorf("validate imported configuration: %w", err)
	}
	return validated, result, nil
}

// unwrapManaged replaces an entry that launches "mcp-manager stdio NAME" with
// the central definition it refers to. Generated wrapper entries would
// otherwise be imported as literal mcp-manager commands and conflict with the
// real transport.
func unwrapManaged(central *config.Config, name string, incoming config.MCP) (config.MCP, error) {
	if incoming.Type != "stdio" {
		return incoming, nil
	}
	wrapped, ok := wrapper.Parse(incoming.Command, incoming.Args)
	if !ok {
		return incoming, nil
	}
	if wrapped != name {
		return config.MCP{}, fmt.Errorf(
			"import MCP %q: entry is an mcp-manager wrapper for a differently named MCP %q",
			name, wrapped,
		)
	}
	definition, exists := central.MCPs[name]
	if !exists {
		return config.MCP{}, fmt.Errorf(
			"import MCP %q: entry is an mcp-manager wrapper, but the central config does not define %q",
			name, name,
		)
	}
	return definition, nil
}

type sourceSelection struct {
	path        string
	scope       string
	projectID   string
	projectPath string
}

func selectSource(existing *config.Config, options Options) (sourceSelection, error) {
	switch options.Agent {
	case config.AgentCodex, config.AgentClaude, config.AgentOpenCode:
	default:
		return sourceSelection{}, fmt.Errorf("unsupported import agent %q", options.Agent)
	}

	hasProjectID := options.ProjectID != ""
	hasProjectPath := options.ProjectPath != ""
	if hasProjectID != hasProjectPath {
		return sourceSelection{}, errors.New("project ID and project path must be provided together")
	}

	selection := sourceSelection{scope: "global"}
	if hasProjectID {
		if !identifierPattern.MatchString(options.ProjectID) {
			return sourceSelection{}, fmt.Errorf(
				"project ID %q must match %s", options.ProjectID, identifierPattern,
			)
		}
		projectPath, err := canonicalProjectPath(options.ProjectPath)
		if err != nil {
			return sourceSelection{}, fmt.Errorf("project %q: %w", options.ProjectID, err)
		}
		if err := ensureProjectRegistration(existing, options.ProjectID, projectPath); err != nil {
			return sourceSelection{}, err
		}
		selection.scope = options.ProjectID
		selection.projectID = options.ProjectID
		selection.projectPath = projectPath
	}

	switch options.Agent {
	case config.AgentCodex:
		root := options.HomeDir
		if selection.projectPath != "" {
			root = selection.projectPath
		} else if root == "" {
			return sourceSelection{}, errors.New("home directory must not be empty for a global Codex import")
		}
		selection.path = filepath.Join(root, ".codex", "config.toml")
	case config.AgentClaude:
		if selection.projectPath != "" {
			selection.path = filepath.Join(selection.projectPath, ".mcp.json")
		} else {
			if options.HomeDir == "" {
				return sourceSelection{}, errors.New("home directory must not be empty for a global Claude import")
			}
			selection.path = filepath.Join(options.HomeDir, ".claude.json")
		}
	case config.AgentOpenCode:
		if selection.projectPath != "" {
			selection.path = filepath.Join(selection.projectPath, "opencode.json")
		} else {
			if options.UserConfigDir == "" {
				return sourceSelection{}, errors.New("user config directory must not be empty for a global OpenCode import")
			}
			selection.path = filepath.Join(options.UserConfigDir, "opencode", "opencode.json")
		}
	}
	return selection, nil
}

func canonicalProjectPath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path %q must be absolute", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("inspect path %q: %w", path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path %q is not a directory", path)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}
	absolute, err := filepath.Abs(canonical)
	if err != nil {
		return "", fmt.Errorf("make path %q absolute: %w", path, err)
	}
	return filepath.Clean(absolute), nil
}

func ensureProjectRegistration(existing *config.Config, id, path string) error {
	if existing == nil {
		return nil
	}
	if registered, exists := existing.Projects[id]; exists {
		canonical, err := canonicalProjectPath(registered.Path)
		if err != nil {
			return fmt.Errorf("registered project %q: %w", id, err)
		}
		if canonical != path {
			return fmt.Errorf(
				"project %q is already registered at %q, not %q", id, canonical, path,
			)
		}
	}
	for otherID, registered := range existing.Projects {
		if otherID == id {
			continue
		}
		canonical, err := canonicalProjectPath(registered.Path)
		if err != nil {
			return fmt.Errorf("registered project %q: %w", otherID, err)
		}
		if canonical == path {
			return fmt.Errorf("project path %q is already registered as %q", path, otherID)
		}
	}
	return nil
}

func parse(agent config.Agent, data []byte) (map[string]config.MCP, int, error) {
	switch agent {
	case config.AgentCodex:
		return parseCodex(data)
	case config.AgentClaude:
		return parseClaude(data)
	case config.AgentOpenCode:
		return parseOpenCode(data)
	default:
		return nil, 0, fmt.Errorf("unsupported agent %q", agent)
	}
}

func parseCodex(data []byte) (map[string]config.MCP, int, error) {
	document := map[string]any{}
	if err := toml.Unmarshal(data, &document); err != nil {
		return nil, 0, err
	}
	section, exists := document["mcp_servers"]
	if !exists {
		return map[string]config.MCP{}, 0, nil
	}
	servers, ok := asObject(section)
	if !ok {
		return nil, 0, errors.New("mcp_servers must be a TOML table")
	}

	result := make(map[string]config.MCP, len(servers))
	skipped := 0
	for _, name := range sortedKeys(servers) {
		entry, ok := asObject(servers[name])
		if !ok {
			return nil, 0, fmt.Errorf("MCP %q must be a TOML table", name)
		}
		disabled, err := disabledEntry(name, entry)
		if err != nil {
			return nil, 0, err
		}
		if disabled {
			skipped++
			continue
		}
		mcp, err := parseCodexEntry(name, entry)
		if err != nil {
			return nil, 0, err
		}
		result[name] = mcp
	}
	return result, skipped, nil
}

func parseCodexEntry(name string, entry map[string]any) (config.MCP, error) {
	_, hasCommand := entry["command"]
	_, hasURL := entry["url"]
	if hasCommand == hasURL {
		return config.MCP{}, fmt.Errorf(
			"MCP %q must define exactly one of command (stdio) or url (http)", name,
		)
	}

	if hasCommand {
		if err := rejectFields(name, "stdio", entry, "url", "http_headers", "env_http_headers", "bearer_token_env_var"); err != nil {
			return config.MCP{}, err
		}
		command, err := requiredString(entry, "command")
		if err != nil {
			return config.MCP{}, entryError(name, err)
		}
		args, err := optionalStrings(entry, "args")
		if err != nil {
			return config.MCP{}, entryError(name, err)
		}
		environment, err := optionalStringMap(entry, "env")
		if err != nil {
			return config.MCP{}, entryError(name, err)
		}
		envFrom, err := optionalStrings(entry, "env_vars")
		if err != nil {
			return config.MCP{}, entryError(name, err)
		}
		sort.Strings(envFrom)
		return config.MCP{Type: "stdio", Command: command, Args: args, Env: environment, EnvFrom: envFrom}, nil
	}

	if err := rejectFields(name, "http", entry, "command", "args", "env", "env_vars"); err != nil {
		return config.MCP{}, err
	}
	url, err := requiredString(entry, "url")
	if err != nil {
		return config.MCP{}, entryError(name, err)
	}
	headers, err := optionalStringMap(entry, "http_headers")
	if err != nil {
		return config.MCP{}, entryError(name, err)
	}
	headersFrom, err := optionalStringMap(entry, "env_http_headers")
	if err != nil {
		return config.MCP{}, entryError(name, err)
	}
	if _, exists := entry["bearer_token_env_var"]; exists {
		return config.MCP{}, fmt.Errorf(
			"MCP %q uses unsupported Codex field %q", name, "bearer_token_env_var",
		)
	}
	return config.MCP{Type: "http", URL: url, Headers: headers, HeadersFrom: headersFrom}, nil
}

func parseClaude(data []byte) (map[string]config.MCP, int, error) {
	document, err := parseJSONCObject(data)
	if err != nil {
		return nil, 0, err
	}
	section, exists := document["mcpServers"]
	if !exists {
		return map[string]config.MCP{}, 0, nil
	}
	servers, ok := asObject(section)
	if !ok {
		return nil, 0, errors.New("mcpServers must be a JSON object")
	}

	result := make(map[string]config.MCP, len(servers))
	skipped := 0
	for _, name := range sortedKeys(servers) {
		entry, ok := asObject(servers[name])
		if !ok {
			return nil, 0, fmt.Errorf("MCP %q must be a JSON object", name)
		}
		disabled, err := disabledEntry(name, entry)
		if err != nil {
			return nil, 0, err
		}
		if disabled {
			skipped++
			continue
		}
		mcp, err := parseClaudeEntry(name, entry)
		if err != nil {
			return nil, 0, err
		}
		result[name] = mcp
	}
	return result, skipped, nil
}

func parseClaudeEntry(name string, entry map[string]any) (config.MCP, error) {
	typeName, hasType, err := optionalString(entry, "type")
	if err != nil {
		return config.MCP{}, entryError(name, err)
	}
	if !hasType {
		_, hasCommand := entry["command"]
		_, hasURL := entry["url"]
		if hasURL && !hasCommand {
			return config.MCP{}, fmt.Errorf(
				"MCP %q with a url must explicitly set type %q", name, "http",
			)
		}
		if !hasCommand || hasURL {
			return config.MCP{}, fmt.Errorf(
				"MCP %q without a type must define a command-only stdio transport", name,
			)
		}
		typeName = "stdio"
	}

	switch typeName {
	case "stdio":
		if err := rejectFields(name, "stdio", entry, "url", "headers"); err != nil {
			return config.MCP{}, err
		}
		command, err := requiredString(entry, "command")
		if err != nil {
			return config.MCP{}, entryError(name, err)
		}
		args, err := optionalStrings(entry, "args")
		if err != nil {
			return config.MCP{}, entryError(name, err)
		}
		nativeEnv, err := optionalStringMap(entry, "env")
		if err != nil {
			return config.MCP{}, entryError(name, err)
		}
		environment, envFrom, err := convertEnvironment(name, nativeEnv, claudeEnvReference, "${VAR}")
		if err != nil {
			return config.MCP{}, err
		}
		return config.MCP{Type: "stdio", Command: command, Args: args, Env: environment, EnvFrom: envFrom}, nil
	case "http":
		if err := rejectFields(name, "http", entry, "command", "args", "env"); err != nil {
			return config.MCP{}, err
		}
		url, err := requiredString(entry, "url")
		if err != nil {
			return config.MCP{}, entryError(name, err)
		}
		nativeHeaders, err := optionalStringMap(entry, "headers")
		if err != nil {
			return config.MCP{}, entryError(name, err)
		}
		headers, headersFrom := convertHeaders(nativeHeaders, claudeEnvReference)
		return config.MCP{Type: "http", URL: url, Headers: headers, HeadersFrom: headersFrom}, nil
	default:
		return config.MCP{}, fmt.Errorf("MCP %q has unsupported Claude transport type %q", name, typeName)
	}
}

func parseOpenCode(data []byte) (map[string]config.MCP, int, error) {
	document, err := parseJSONCObject(data)
	if err != nil {
		return nil, 0, err
	}
	section, exists := document["mcp"]
	if !exists {
		return map[string]config.MCP{}, 0, nil
	}
	servers, ok := asObject(section)
	if !ok {
		return nil, 0, errors.New("mcp must be a JSON object")
	}

	result := make(map[string]config.MCP, len(servers))
	skipped := 0
	for _, name := range sortedKeys(servers) {
		entry, ok := asObject(servers[name])
		if !ok {
			return nil, 0, fmt.Errorf("MCP %q must be a JSON object", name)
		}
		disabled, err := disabledEntry(name, entry)
		if err != nil {
			return nil, 0, err
		}
		if disabled {
			skipped++
			continue
		}
		mcp, err := parseOpenCodeEntry(name, entry)
		if err != nil {
			return nil, 0, err
		}
		result[name] = mcp
	}
	return result, skipped, nil
}

func disabledEntry(name string, entry map[string]any) (bool, error) {
	raw, exists := entry["enabled"]
	if !exists {
		return false, nil
	}
	enabled, ok := raw.(bool)
	if !ok {
		return false, fmt.Errorf("MCP %q: field %q must be a boolean, got %T", name, "enabled", raw)
	}
	return !enabled, nil
}

func parseOpenCodeEntry(name string, entry map[string]any) (config.MCP, error) {
	typeName, err := requiredString(entry, "type")
	if err != nil {
		return config.MCP{}, entryError(name, err)
	}
	switch typeName {
	case "local":
		if err := rejectFields(name, "local", entry, "url", "headers"); err != nil {
			return config.MCP{}, err
		}
		command, err := requiredStrings(entry, "command")
		if err != nil {
			return config.MCP{}, entryError(name, err)
		}
		if len(command) == 0 {
			return config.MCP{}, fmt.Errorf("MCP %q field %q must not be empty", name, "command")
		}
		nativeEnv, err := optionalStringMap(entry, "environment")
		if err != nil {
			return config.MCP{}, entryError(name, err)
		}
		environment, envFrom, err := convertEnvironment(name, nativeEnv, openCodeEnvReference, "{env:VAR}")
		if err != nil {
			return config.MCP{}, err
		}
		return config.MCP{
			Type: "stdio", Command: command[0], Args: command[1:],
			Env: environment, EnvFrom: envFrom,
		}, nil
	case "remote":
		if err := rejectFields(name, "remote", entry, "command", "environment"); err != nil {
			return config.MCP{}, err
		}
		url, err := requiredString(entry, "url")
		if err != nil {
			return config.MCP{}, entryError(name, err)
		}
		nativeHeaders, err := optionalStringMap(entry, "headers")
		if err != nil {
			return config.MCP{}, entryError(name, err)
		}
		headers, headersFrom := convertHeaders(nativeHeaders, openCodeEnvReference)
		return config.MCP{Type: "http", URL: url, Headers: headers, HeadersFrom: headersFrom}, nil
	default:
		return config.MCP{}, fmt.Errorf("MCP %q has unsupported OpenCode transport type %q", name, typeName)
	}
}

func parseJSONCObject(data []byte) (map[string]any, error) {
	standardized := jsonc.ToJSON(data)
	if err := config.ValidateJSON(standardized); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(standardized))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := requireEOF(decoder); err != nil {
		return nil, err
	}
	document, ok := asObject(value)
	if !ok {
		return nil, errors.New("top-level configuration must be a JSON object")
	}
	return document, nil
}

func convertEnvironment(
	name string,
	native map[string]string,
	pattern *regexp.Regexp,
	syntax string,
) (map[string]string, []string, error) {
	literal := make(map[string]string)
	var from []string
	for _, key := range sortedKeys(native) {
		value := native[key]
		match := pattern.FindStringSubmatch(value)
		if match == nil {
			literal[key] = value
			continue
		}
		variable := match[1]
		if key != variable {
			return nil, nil, fmt.Errorf(
				"MCP %q environment %q references %q using %s; envFrom cannot represent differently named variables",
				name, key, variable, syntax,
			)
		}
		from = append(from, variable)
	}
	if len(literal) == 0 {
		literal = nil
	}
	return literal, from, nil
}

func convertHeaders(native map[string]string, pattern *regexp.Regexp) (map[string]string, map[string]string) {
	literal := make(map[string]string)
	from := make(map[string]string)
	for key, value := range native {
		match := pattern.FindStringSubmatch(value)
		if match == nil {
			literal[key] = value
		} else {
			from[key] = match[1]
		}
	}
	if len(literal) == 0 {
		literal = nil
	}
	if len(from) == 0 {
		from = nil
	}
	return literal, from
}

func rejectFields(name, transport string, entry map[string]any, fields ...string) error {
	for _, field := range fields {
		if _, exists := entry[field]; exists {
			return fmt.Errorf("MCP %q %s transport cannot set %q", name, transport, field)
		}
	}
	return nil
}

func entryError(name string, err error) error {
	return fmt.Errorf("MCP %q: %w", name, err)
}

func requiredString(entry map[string]any, key string) (string, error) {
	value, exists, err := optionalString(entry, key)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("missing required string field %q", key)
	}
	return value, nil
}

func optionalString(entry map[string]any, key string) (string, bool, error) {
	raw, exists := entry[key]
	if !exists {
		return "", false, nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", true, fmt.Errorf("field %q must be a string, got %T", key, raw)
	}
	return value, true, nil
}

func requiredStrings(entry map[string]any, key string) ([]string, error) {
	values, exists, err := stringsField(entry, key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("missing required string array field %q", key)
	}
	return values, nil
}

func optionalStrings(entry map[string]any, key string) ([]string, error) {
	values, _, err := stringsField(entry, key)
	return values, err
}

func stringsField(entry map[string]any, key string) ([]string, bool, error) {
	raw, exists := entry[key]
	if !exists {
		return nil, false, nil
	}
	switch values := raw.(type) {
	case []string:
		return append([]string(nil), values...), true, nil
	case []any:
		result := make([]string, len(values))
		for index, rawValue := range values {
			value, ok := rawValue.(string)
			if !ok {
				return nil, true, fmt.Errorf("field %q item %d must be a string, got %T", key, index, rawValue)
			}
			result[index] = value
		}
		return result, true, nil
	default:
		return nil, true, fmt.Errorf("field %q must be an array of strings, got %T", key, raw)
	}
}

func optionalStringMap(entry map[string]any, key string) (map[string]string, error) {
	raw, exists := entry[key]
	if !exists {
		return nil, nil
	}
	values, ok := asObject(raw)
	if !ok {
		return nil, fmt.Errorf("field %q must be an object of strings, got %T", key, raw)
	}
	result := make(map[string]string, len(values))
	for _, name := range sortedKeys(values) {
		value, ok := values[name].(string)
		if !ok {
			return nil, fmt.Errorf("field %q value %q must be a string, got %T", key, name, values[name])
		}
		result[name] = value
	}
	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
}

func asObject(value any) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	return object, ok
}

func activate(cfg *config.Config, name, projectID string) bool {
	if projectID == "" {
		return appendSortedUnique(&cfg.Global.MCPs, name)
	}
	project := cfg.Projects[projectID]
	changed := appendSortedUnique(&project.MCPs, name)
	cfg.Projects[projectID] = project
	return changed
}

func appendSortedUnique(values *[]string, value string) bool {
	if *values == nil {
		*values = []string{}
	}
	sort.Strings(*values)
	for _, existing := range *values {
		if existing == value {
			return false
		}
	}
	*values = append(*values, value)
	sort.Strings(*values)
	return true
}

func sameTransport(left, right config.MCP) bool {
	return left.Type == right.Type &&
		left.Command == right.Command &&
		equalStrings(left.Args, right.Args) &&
		equalStringMap(left.Env, right.Env, false) &&
		equalStringSet(left.EnvFrom, right.EnvFrom) &&
		left.URL == right.URL &&
		equalStringMap(left.Headers, right.Headers, true) &&
		equalStringMap(left.HeadersFrom, right.HeadersFrom, true)
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func equalStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	counts := make(map[string]int, len(left))
	for _, value := range left {
		counts[value]++
	}
	for _, value := range right {
		counts[value]--
		if counts[value] < 0 {
			return false
		}
	}
	return true
}

func equalStringMap(left, right map[string]string, foldKeys bool) bool {
	if len(left) != len(right) {
		return false
	}
	if !foldKeys {
		for key, value := range left {
			rightValue, exists := right[key]
			if !exists || rightValue != value {
				return false
			}
		}
		return true
	}
	folded := make(map[string]string, len(left))
	for key, value := range left {
		folded[strings.ToLower(key)] = value
	}
	for key, value := range right {
		leftValue, exists := folded[strings.ToLower(key)]
		if !exists || leftValue != value {
			return false
		}
	}
	return true
}

func cloneConfig(source *config.Config) *config.Config {
	if source == nil {
		return &config.Config{
			Version: config.CurrentVersion,
			Global: config.Scope{
				MCPs: []string{}, DisabledAgents: map[string][]config.Agent{},
			},
			Projects: map[string]config.Project{},
			MCPs:     map[string]config.MCP{},
		}
	}
	result := &config.Config{
		Schema: source.Schema, Version: source.Version, Options: source.Options,
		Global:   cloneScope(source.Global),
		Projects: make(map[string]config.Project, len(source.Projects)),
		MCPs:     make(map[string]config.MCP, len(source.MCPs)),
	}
	for id, project := range source.Projects {
		project.MCPs = cloneSlice(project.MCPs)
		project.DisabledAgents = cloneDisabledAgents(project.DisabledAgents)
		result.Projects[id] = project
	}
	for name, mcp := range source.MCPs {
		mcp.Args = cloneSlice(mcp.Args)
		mcp.Env = cloneStringMap(mcp.Env)
		mcp.EnvFrom = cloneSlice(mcp.EnvFrom)
		mcp.Headers = cloneStringMap(mcp.Headers)
		mcp.HeadersFrom = cloneStringMap(mcp.HeadersFrom)
		result.MCPs[name] = mcp
	}
	return result
}

func cloneScope(source config.Scope) config.Scope {
	return config.Scope{
		MCPs:           cloneSlice(source.MCPs),
		DisabledAgents: cloneDisabledAgents(source.DisabledAgents),
	}
}

func cloneDisabledAgents(source map[string][]config.Agent) map[string][]config.Agent {
	if source == nil {
		return nil
	}
	result := make(map[string][]config.Agent, len(source))
	for name, agents := range source {
		result[name] = cloneSlice(agents)
	}
	return result
}

func cloneSlice[T any](source []T) []T {
	if source == nil {
		return nil
	}
	result := make([]T, len(source))
	copy(result, source)
	return result
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func validateMerged(candidate *config.Config, home string) (*config.Config, error) {
	data, err := json.Marshal(candidate)
	if err != nil {
		return nil, err
	}
	if home == "" {
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("find home directory: %w", err)
		}
	}
	return config.Parse(data, home)
}

func requireEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("multiple JSON values are not allowed")
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
