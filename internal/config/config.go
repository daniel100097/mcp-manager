package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/daniel100097/mcp-manager/internal/fileutil"
)

type Agent string

const (
	CurrentVersion = 2

	AgentCodex    Agent = "codex"
	AgentClaude   Agent = "claude"
	AgentOpenCode Agent = "opencode"

	// StdioModeWrapper generates stdio entries that launch
	// "mcp-manager stdio NAME"; the wrapper resolves the real command from the
	// central config at start time. It is the default.
	StdioModeWrapper = "wrapper"
	// StdioModeDirect generates stdio entries that contain the real command,
	// args, and environment.
	StdioModeDirect = "direct"
)

var (
	idPattern     = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	envPattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	headerPattern = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
)

type Config struct {
	Schema   string             `json:"$schema,omitempty"`
	Version  int                `json:"version"`
	Options  Options            `json:"options,omitempty"`
	Global   Scope              `json:"global"`
	Projects map[string]Project `json:"projects"`
	MCPs     map[string]MCP     `json:"mcps"`
}

type Options struct {
	InlineSecrets bool   `json:"inlineSecrets,omitempty"`
	StdioMode     string `json:"stdioMode,omitempty"`
}

// WrapStdio reports whether generated stdio entries should launch the
// mcp-manager wrapper instead of the real command. An unset StdioMode selects
// the wrapper.
func (o Options) WrapStdio() bool {
	return o.StdioMode != StdioModeDirect
}

type Scope struct {
	MCPs           []string           `json:"mcps"`
	DisabledAgents map[string][]Agent `json:"disabledAgents,omitempty"`
}

type Project struct {
	Path           string             `json:"path"`
	MCPs           []string           `json:"mcps"`
	DisabledAgents map[string][]Agent `json:"disabledAgents,omitempty"`
}

type MCP struct {
	Type        string            `json:"type"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	EnvFrom     []string          `json:"envFrom,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	HeadersFrom map[string]string `json:"headersFrom,omitempty"`
}

type configV1 struct {
	Schema   string            `json:"$schema,omitempty"`
	Version  int               `json:"version"`
	Options  Options           `json:"options,omitempty"`
	Projects map[string]string `json:"projects"`
	MCPs     map[string]mcpV1  `json:"mcps"`
}

type mcpV1 struct {
	MCP
	Global         bool     `json:"global"`
	Projects       []string `json:"projects"`
	DisabledAgents []Agent  `json:"disabledAgents,omitempty"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read central config %q: %w", path, err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("find home directory: %w", err)
	}
	return Parse(data, home)
}

func Save(path string, cfg *Config) error {
	if cfg == nil {
		return errors.New("config must not be nil")
	}
	cfg.Version = CurrentVersion
	cfg.normalizeCollections()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode central config: %w", err)
	}
	data = append(data, '\n')
	mode, err := fileutil.ExistingMode(path, 0o600)
	if err != nil {
		return err
	}
	if err := fileutil.WriteAtomic(path, data, mode); err != nil {
		return fmt.Errorf("write central config %q: %w", path, err)
	}
	return nil
}

func Parse(data []byte, home string) (*Config, error) {
	if err := ValidateJSON(data); err != nil {
		return nil, fmt.Errorf("invalid central config: %w", err)
	}

	version, err := configVersion(data)
	if err != nil {
		return nil, fmt.Errorf("invalid central config: %w", err)
	}
	switch version {
	case 1:
		return parseV1(data, home)
	case CurrentVersion:
		return parseV2(data, home)
	default:
		return nil, fmt.Errorf(
			"unsupported config version %d (expected %d)",
			version,
			CurrentVersion,
		)
	}
}

func parseV2(data []byte, home string) (*Config, error) {
	if err := validateRequiredFieldsV2(data); err != nil {
		return nil, fmt.Errorf("invalid central config: %w", err)
	}
	var cfg Config
	if err := decodeStrict(data, &cfg); err != nil {
		return nil, fmt.Errorf("invalid central config: %w", err)
	}
	cfg.normalizeCollections()
	if err := cfg.validate(home); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func parseV1(data []byte, home string) (*Config, error) {
	if err := validateRequiredFieldsV1(data); err != nil {
		return nil, fmt.Errorf("invalid central config: %w", err)
	}
	var legacy configV1
	if err := decodeStrict(data, &legacy); err != nil {
		return nil, fmt.Errorf("invalid central config: %w", err)
	}
	cfg, err := migrateV1(legacy)
	if err != nil {
		return nil, err
	}
	cfg.normalizeCollections()
	if err := cfg.validate(home); err != nil {
		return nil, err
	}
	return cfg, nil
}

func decodeStrict(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	return requireEOF(decoder)
}

func configVersion(data []byte) (int, error) {
	if firstJSONByte(data) != '{' {
		return 0, errors.New("top-level configuration must be an object")
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return 0, err
	}
	rawVersion, exists := root["version"]
	if !exists {
		return 0, errors.New("required field \"version\" is missing")
	}
	if bytes.Equal(bytes.TrimSpace(rawVersion), []byte("null")) {
		return 0, errors.New("field \"version\" must be an integer")
	}
	var version int
	if err := json.Unmarshal(rawVersion, &version); err != nil {
		return 0, fmt.Errorf("field %q must be an integer: %w", "version", err)
	}
	return version, nil
}

func migrateV1(legacy configV1) (*Config, error) {
	cfg := &Config{
		Schema:  legacy.Schema,
		Version: CurrentVersion,
		Options: legacy.Options,
		Global: Scope{
			MCPs:           []string{},
			DisabledAgents: map[string][]Agent{},
		},
		Projects: make(map[string]Project, len(legacy.Projects)),
		MCPs:     make(map[string]MCP, len(legacy.MCPs)),
	}
	for id, path := range legacy.Projects {
		cfg.Projects[id] = Project{
			Path:           path,
			MCPs:           []string{},
			DisabledAgents: map[string][]Agent{},
		}
	}

	for _, name := range sortedKeys(legacy.MCPs) {
		legacyMCP := legacy.MCPs[name]
		if err := validateAgentList(fmt.Sprintf("MCP %q", name), legacyMCP.DisabledAgents); err != nil {
			return nil, err
		}
		cfg.MCPs[name] = legacyMCP.MCP
		if legacyMCP.Global {
			cfg.Global.MCPs = append(cfg.Global.MCPs, name)
			if len(legacyMCP.DisabledAgents) > 0 {
				cfg.Global.DisabledAgents[name] = cloneAgents(legacyMCP.DisabledAgents)
			}
		}

		seenProjects := make(map[string]struct{}, len(legacyMCP.Projects))
		for _, id := range legacyMCP.Projects {
			project, exists := cfg.Projects[id]
			if !exists {
				return nil, fmt.Errorf("MCP %q references unknown project %q", name, id)
			}
			if _, duplicate := seenProjects[id]; duplicate {
				return nil, fmt.Errorf("MCP %q lists project %q more than once", name, id)
			}
			seenProjects[id] = struct{}{}
			project.MCPs = append(project.MCPs, name)
			if len(legacyMCP.DisabledAgents) > 0 {
				project.DisabledAgents[name] = cloneAgents(legacyMCP.DisabledAgents)
			}
			cfg.Projects[id] = project
		}
	}
	return cfg, nil
}

func cloneAgents(agents []Agent) []Agent {
	return append([]Agent(nil), agents...)
}

// ValidateJSON rejects malformed input, trailing values, and duplicate object
// keys. encoding/json otherwise silently accepts the last duplicate key.
func ValidateJSON(data []byte) error {
	return rejectDuplicateKeys(data)
}

func (c *Config) validate(home string) error {
	if c.Version != CurrentVersion {
		return fmt.Errorf("unsupported config version %d (expected %d)", c.Version, CurrentVersion)
	}
	switch c.Options.StdioMode {
	case "", StdioModeWrapper, StdioModeDirect:
	default:
		return fmt.Errorf(
			"options.stdioMode must be either %q or %q; got %q",
			StdioModeWrapper, StdioModeDirect, c.Options.StdioMode,
		)
	}

	for _, name := range sortedKeys(c.MCPs) {
		if !idPattern.MatchString(name) {
			return fmt.Errorf("MCP name %q must match %s", name, idPattern)
		}
		if err := validateMCP(name, c.MCPs[name]); err != nil {
			return err
		}
	}
	if err := validateScope("global scope", c.Global.MCPs, c.Global.DisabledAgents, c.MCPs); err != nil {
		return err
	}

	projectIDs := sortedKeys(c.Projects)
	canonicalRoots := make(map[string]string, len(projectIDs))
	for _, id := range projectIDs {
		if !idPattern.MatchString(id) {
			return fmt.Errorf("project ID %q must match %s", id, idPattern)
		}
		project := c.Projects[id]
		expanded, err := expandProjectPath(project.Path, home)
		if err != nil {
			return fmt.Errorf("project %q: %w", id, err)
		}
		info, err := os.Stat(expanded)
		if err != nil {
			return fmt.Errorf("project %q path %q: %w", id, expanded, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("project %q path %q is not a directory", id, expanded)
		}
		canonical, err := filepath.EvalSymlinks(expanded)
		if err != nil {
			return fmt.Errorf("resolve project %q path %q: %w", id, expanded, err)
		}
		canonical, err = filepath.Abs(canonical)
		if err != nil {
			return fmt.Errorf("make project %q path absolute: %w", id, err)
		}
		canonical = filepath.Clean(canonical)
		if previous, exists := canonicalRoots[canonical]; exists {
			return fmt.Errorf("projects %q and %q resolve to the same path %q", previous, id, canonical)
		}
		canonicalRoots[canonical] = id
		project.Path = canonical
		c.Projects[id] = project
		if err := validateScope(
			fmt.Sprintf("project %q", id),
			project.MCPs,
			project.DisabledAgents,
			c.MCPs,
		); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) normalizeCollections() {
	if c.Projects == nil {
		c.Projects = map[string]Project{}
	}
	if c.MCPs == nil {
		c.MCPs = map[string]MCP{}
	}
	if c.Global.MCPs == nil {
		c.Global.MCPs = []string{}
	}
	if c.Global.DisabledAgents == nil {
		c.Global.DisabledAgents = map[string][]Agent{}
	}
	normalizeDisabledAgents(c.Global.DisabledAgents)
	for id, project := range c.Projects {
		if project.MCPs == nil {
			project.MCPs = []string{}
		}
		if project.DisabledAgents == nil {
			project.DisabledAgents = map[string][]Agent{}
		}
		normalizeDisabledAgents(project.DisabledAgents)
		c.Projects[id] = project
	}
}

func normalizeDisabledAgents(disabledAgents map[string][]Agent) {
	for name, agents := range disabledAgents {
		if agents == nil {
			disabledAgents[name] = []Agent{}
		}
	}
}

func validateMCP(name string, mcp MCP) error {
	for i, arg := range mcp.Args {
		if strings.ContainsRune(arg, '\x00') {
			return fmt.Errorf("MCP %q args[%d] contains a NUL byte", name, i)
		}
	}

	switch mcp.Type {
	case "stdio":
		if strings.TrimSpace(mcp.Command) == "" {
			return fmt.Errorf("MCP %q: stdio command must not be empty", name)
		}
		if strings.ContainsRune(mcp.Command, '\x00') {
			return fmt.Errorf("MCP %q: stdio command contains a NUL byte", name)
		}
		if mcp.URL != "" || len(mcp.Headers) != 0 || len(mcp.HeadersFrom) != 0 {
			return fmt.Errorf("MCP %q: stdio transport cannot set url, headers, or headersFrom", name)
		}
		if err := validateEnvironment(name, mcp.Env, mcp.EnvFrom); err != nil {
			return err
		}
	case "http":
		if mcp.Command != "" || len(mcp.Args) != 0 || len(mcp.Env) != 0 || len(mcp.EnvFrom) != 0 {
			return fmt.Errorf("MCP %q: http transport cannot set command, args, env, or envFrom", name)
		}
		parsed, err := url.Parse(mcp.URL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("MCP %q: url must be an absolute http or https URL", name)
		}
		if err := validateHeaders(name, mcp.Headers, mcp.HeadersFrom); err != nil {
			return err
		}
	default:
		return fmt.Errorf("MCP %q: type must be either %q or %q", name, "stdio", "http")
	}
	return nil
}

func validateScope(
	label string,
	names []string,
	disabledAgents map[string][]Agent,
	definitions map[string]MCP,
) error {
	active := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, exists := definitions[name]; !exists {
			return fmt.Errorf("%s references unknown MCP %q", label, name)
		}
		if _, duplicate := active[name]; duplicate {
			return fmt.Errorf("%s lists MCP %q more than once", label, name)
		}
		active[name] = struct{}{}
	}

	for _, name := range sortedKeys(disabledAgents) {
		if _, enabled := active[name]; !enabled {
			return fmt.Errorf("%s disabledAgents key %q is not active in that scope", label, name)
		}
		if err := validateAgentList(
			fmt.Sprintf("%s MCP %q", label, name),
			disabledAgents[name],
		); err != nil {
			return err
		}
	}
	return nil
}

func validateAgentList(label string, agents []Agent) error {
	seen := make(map[Agent]struct{}, len(agents))
	for _, agent := range agents {
		switch agent {
		case AgentCodex, AgentClaude, AgentOpenCode:
		default:
			return fmt.Errorf("%s has unknown disabled agent %q", label, agent)
		}
		if _, duplicate := seen[agent]; duplicate {
			return fmt.Errorf("%s lists disabled agent %q more than once", label, agent)
		}
		seen[agent] = struct{}{}
	}
	return nil
}

func validateEnvironment(name string, literal map[string]string, from []string) error {
	for key, value := range literal {
		if !envPattern.MatchString(key) {
			return fmt.Errorf("MCP %q has invalid environment variable name %q", name, key)
		}
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("MCP %q environment variable %q contains a NUL byte", name, key)
		}
	}
	seen := make(map[string]struct{}, len(from))
	for _, variable := range from {
		if !envPattern.MatchString(variable) {
			return fmt.Errorf("MCP %q has invalid envFrom variable %q", name, variable)
		}
		if _, exists := literal[variable]; exists {
			return fmt.Errorf("MCP %q defines %q in both env and envFrom", name, variable)
		}
		if _, duplicate := seen[variable]; duplicate {
			return fmt.Errorf("MCP %q lists envFrom variable %q more than once", name, variable)
		}
		seen[variable] = struct{}{}
	}
	return nil
}

func validateHeaders(name string, literal, from map[string]string) error {
	seen := make(map[string]string, len(literal)+len(from))
	for header, value := range literal {
		if err := validateHeaderName(header); err != nil {
			return fmt.Errorf("MCP %q: %w", name, err)
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("MCP %q header %q contains a forbidden control character", name, header)
		}
		folded := strings.ToLower(header)
		if previous, duplicate := seen[folded]; duplicate {
			return fmt.Errorf("MCP %q headers %q and %q differ only by case", name, previous, header)
		}
		seen[folded] = header
	}
	for header, variable := range from {
		if err := validateHeaderName(header); err != nil {
			return fmt.Errorf("MCP %q: %w", name, err)
		}
		if !envPattern.MatchString(variable) {
			return fmt.Errorf("MCP %q header %q has invalid environment variable %q", name, header, variable)
		}
		folded := strings.ToLower(header)
		if previous, duplicate := seen[folded]; duplicate {
			return fmt.Errorf("MCP %q headers %q and %q conflict", name, previous, header)
		}
		seen[folded] = header
	}
	return nil
}

func validateHeaderName(name string) error {
	if !headerPattern.MatchString(name) {
		return fmt.Errorf("invalid HTTP header name %q", name)
	}
	return nil
}

func validateRequiredFieldsV2(data []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return err
	}
	if root == nil {
		return errors.New("top-level configuration must be an object")
	}
	for _, field := range []string{"version", "global", "projects", "mcps"} {
		if _, exists := root[field]; !exists {
			return fmt.Errorf("required field %q is missing", field)
		}
	}
	for _, field := range []string{"global", "projects", "mcps"} {
		if firstJSONByte(root[field]) != '{' {
			return fmt.Errorf("field %q must be an object", field)
		}
	}
	if rawOptions, exists := root["options"]; exists && firstJSONByte(rawOptions) != '{' {
		return fmt.Errorf("field %q must be an object", "options")
	}

	var global map[string]json.RawMessage
	if err := json.Unmarshal(root["global"], &global); err != nil {
		return fmt.Errorf("field %q must be an object", "global")
	}
	if err := validateRequiredScopeFields("global scope", global); err != nil {
		return err
	}

	var projects map[string]json.RawMessage
	if err := json.Unmarshal(root["projects"], &projects); err != nil {
		return fmt.Errorf("field %q must be an object", "projects")
	}
	for id, rawProject := range projects {
		label := fmt.Sprintf("project %q", id)
		if firstJSONByte(rawProject) != '{' {
			return fmt.Errorf("%s must be an object", label)
		}
		var project map[string]json.RawMessage
		if err := json.Unmarshal(rawProject, &project); err != nil {
			return fmt.Errorf("%s must be an object", label)
		}
		if _, exists := project["path"]; !exists {
			return fmt.Errorf("%s is missing required field %q", label, "path")
		}
		if firstJSONByte(project["path"]) != '"' {
			return fmt.Errorf("%s field %q must be a string", label, "path")
		}
		if err := validateRequiredScopeFields(label, project); err != nil {
			return err
		}
	}

	var mcps map[string]json.RawMessage
	if err := json.Unmarshal(root["mcps"], &mcps); err != nil {
		return fmt.Errorf("field %q must be an object", "mcps")
	}
	for name, rawMCP := range mcps {
		if firstJSONByte(rawMCP) != '{' {
			return fmt.Errorf("MCP %q must be an object", name)
		}
		var mcp map[string]json.RawMessage
		if err := json.Unmarshal(rawMCP, &mcp); err != nil {
			return fmt.Errorf("MCP %q must be an object", name)
		}
		if _, exists := mcp["type"]; !exists {
			return fmt.Errorf("MCP %q is missing required field %q", name, "type")
		}
	}
	return nil
}

func validateRequiredScopeFields(label string, scope map[string]json.RawMessage) error {
	if _, exists := scope["mcps"]; !exists {
		return fmt.Errorf("%s is missing required field %q", label, "mcps")
	}
	if firstJSONByte(scope["mcps"]) != '[' {
		return fmt.Errorf("%s field %q must be an array", label, "mcps")
	}
	rawDisabledAgents, exists := scope["disabledAgents"]
	if !exists {
		return nil
	}
	if firstJSONByte(rawDisabledAgents) != '{' {
		return fmt.Errorf("%s field %q must be an object", label, "disabledAgents")
	}

	var disabledAgents map[string]json.RawMessage
	if err := json.Unmarshal(rawDisabledAgents, &disabledAgents); err != nil {
		return fmt.Errorf("%s field %q must be an object", label, "disabledAgents")
	}
	for name, rawAgents := range disabledAgents {
		if firstJSONByte(rawAgents) != '[' {
			return fmt.Errorf("%s disabledAgents for MCP %q must be an array", label, name)
		}
	}
	return nil
}

func validateRequiredFieldsV1(data []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return err
	}
	if root == nil {
		return errors.New("top-level configuration must be an object")
	}
	for _, field := range []string{"version", "projects", "mcps"} {
		if _, exists := root[field]; !exists {
			return fmt.Errorf("required field %q is missing", field)
		}
	}
	for _, field := range []string{"projects", "mcps"} {
		if firstJSONByte(root[field]) != '{' {
			return fmt.Errorf("field %q must be an object", field)
		}
	}
	if rawOptions, exists := root["options"]; exists && firstJSONByte(rawOptions) != '{' {
		return fmt.Errorf("field %q must be an object", "options")
	}

	var projects map[string]json.RawMessage
	if err := json.Unmarshal(root["projects"], &projects); err != nil {
		return fmt.Errorf("field %q must be an object", "projects")
	}
	for id, path := range projects {
		if firstJSONByte(path) != '"' {
			return fmt.Errorf("project %q path must be a string", id)
		}
	}

	var mcps map[string]json.RawMessage
	if err := json.Unmarshal(root["mcps"], &mcps); err != nil {
		return fmt.Errorf("field %q must be an object", "mcps")
	}
	for name, rawMCP := range mcps {
		if firstJSONByte(rawMCP) != '{' {
			return fmt.Errorf("MCP %q must be an object", name)
		}
		var mcp map[string]json.RawMessage
		if err := json.Unmarshal(rawMCP, &mcp); err != nil {
			return fmt.Errorf("MCP %q must be an object", name)
		}
		for _, field := range []string{"type", "global", "projects"} {
			if _, exists := mcp[field]; !exists {
				return fmt.Errorf("MCP %q is missing required field %q", name, field)
			}
		}
		global := string(bytes.TrimSpace(mcp["global"]))
		if global != "true" && global != "false" {
			return fmt.Errorf("MCP %q field %q must be a boolean", name, "global")
		}
		if firstJSONByte(mcp["projects"]) != '[' {
			return fmt.Errorf("MCP %q field %q must be an array", name, "projects")
		}
		if rawDisabled, exists := mcp["disabledAgents"]; exists && firstJSONByte(rawDisabled) != '[' {
			return fmt.Errorf("MCP %q field %q must be an array", name, "disabledAgents")
		}
	}
	return nil
}

func firstJSONByte(data []byte) byte {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return 0
	}
	return trimmed[0]
}

func expandProjectPath(path, home string) (string, error) {
	if path == "" {
		return "", errors.New("path must not be empty")
	}
	if path == "~" {
		path = home
	} else if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		path = filepath.Join(home, path[2:])
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path %q must be absolute (a leading ~/ is supported)", path)
	}
	return filepath.Clean(path), nil
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := inspectJSONValue(decoder, "$"); err != nil {
		return err
	}
	return requireEOF(decoder)
}

func inspectJSONValue(decoder *json.Decoder, path string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key at %s is not a string", path)
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate key %q at %s", key, path)
			}
			seen[key] = struct{}{}
			if err := inspectJSONValue(decoder, path+"."+key); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		index := 0
		for decoder.More() {
			if err := inspectJSONValue(decoder, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
			index++
		}
		_, err = decoder.Token()
		return err
	default:
		return fmt.Errorf("unexpected JSON delimiter %q at %s", delim, path)
	}
}

func requireEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
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
