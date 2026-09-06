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
	"reflect"
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

// Source names the central config file and its optional local overlay.
type Source struct {
	// Path is the central config.
	Path string
	// LocalPath is the overlay applied on top of the central config as a JSON
	// merge patch. An empty value disables layering; a missing file is not an
	// error, so the overlay is purely optional.
	LocalPath string
}

// LocalPathFor returns the default overlay location for a central config: the
// same file name with ".local" inserted before the extension, so config.json
// pairs with config.local.json.
func LocalPathFor(path string) string {
	ext := filepath.Ext(path)
	if ext == "" {
		return path + ".local.json"
	}
	return strings.TrimSuffix(path, ext) + ".local" + ext
}

// Load reads and validates a central config without a local overlay.
func Load(path string) (*Config, error) {
	return Source{Path: path}.Load()
}

// Load returns the effective config: the central file with the local overlay
// applied, migrated to the current version, and fully validated, including
// the project directory checks.
func (s Source) Load() (*Config, error) {
	data, err := readCentralV2(s.Path)
	if err != nil {
		return nil, err
	}
	return s.overlay(data)
}

// Edit is a config opened for modification. When the local overlay file
// exists, Config is the central config with the overlay applied and Save
// writes the resulting changes back to the overlay only, keeping the central
// file as shared defaults. Otherwise Config is the central config and Save
// writes it. In both cases project paths stay exactly as written.
type Edit struct {
	source Source
	// central is the canonical version 2 form of the central file, or nil
	// when the central file does not exist yet.
	central []byte
	// local is the decoded overlay when Save writes to it.
	local      any
	writeLocal bool
	// Config is the configuration to modify.
	Config *Config
}

// OpenEdit reads the central config and, when present, the local overlay,
// for modification. Structural and semantic checks run, but project paths are
// neither resolved nor required to exist, so a shared central config stays
// editable on machines where the overlay supplies the real paths.
func (s Source) OpenEdit() (*Edit, error) {
	central, err := readCentralV2(s.Path)
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("find home directory: %w", err)
	}
	patch, present, err := readLocalPatch(s.LocalPath)
	if err != nil {
		return nil, err
	}
	data := central
	if present {
		if data, err = applyLocalPatch(central, patch, s.LocalPath); err != nil {
			return nil, err
		}
	}
	var cfg Config
	if err := decodeStrict(data, &cfg); err != nil {
		return nil, fmt.Errorf("invalid central config: %w", err)
	}
	cfg.normalizeCollections()
	if err := cfg.validateStatic(home); err != nil {
		if present {
			return nil, fmt.Errorf("with local config %q applied: %w", s.LocalPath, err)
		}
		return nil, err
	}
	edit := &Edit{source: s, central: central, writeLocal: present, Config: &cfg}
	if present {
		if err := decodeAny(patch, &edit.local); err != nil {
			return nil, fmt.Errorf("invalid local config %q: %w", s.LocalPath, err)
		}
	}
	return edit, nil
}

// NewEdit prepares a central config that does not exist on disk yet. Save
// writes it to the central file; the local overlay, if any, still applies to
// the effective config.
func (s Source) NewEdit(cfg *Config) *Edit {
	return &Edit{source: s, Config: cfg}
}

// WritesLocal reports whether Save writes to the local overlay instead of the
// central config.
func (e *Edit) WritesLocal() bool {
	return e.writeLocal
}

// Target returns the file that Save writes.
func (e *Edit) Target() string {
	if e.writeLocal {
		return e.source.LocalPath
	}
	return e.source.Path
}

// Label names the file that Save writes, for messages.
func (e *Edit) Label() string {
	if e.writeLocal {
		return "local config"
	}
	return "central config"
}

// Effective fully validates the current state of Config, including project
// directories, and returns the effective config that sync should apply.
// Config itself is left unchanged.
func (e *Edit) Effective() (*Config, error) {
	if !e.writeLocal {
		return e.source.Overlay(e.Config)
	}
	data, err := json.Marshal(e.Config)
	if err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("find home directory: %w", err)
	}
	cfg, err := parseV2(data, home)
	if err != nil {
		return nil, fmt.Errorf("with local config %q applied: %w", e.source.LocalPath, err)
	}
	return cfg, nil
}

// Save writes the changes made to Config. With a local overlay present, it
// rewrites the overlay as the merge patch that turns the central config into
// Config, preserving overlay entries that still have no effect (such as a
// value pinned to its central default). Without an overlay it writes the
// central file.
func (e *Edit) Save() error {
	if e.Config == nil {
		return errors.New("config must not be nil")
	}
	if !e.writeLocal {
		return Save(e.source.Path, e.Config)
	}
	e.Config.Version = CurrentVersion
	e.Config.normalizeCollections()
	edited, err := json.Marshal(e.Config)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	patch, err := localPatchFor(e.central, e.local, edited)
	if err != nil {
		return err
	}
	return writeJSONFile(e.source.LocalPath, patch, "local config")
}

// localPatchFor returns the overlay that reproduces edited when applied to
// central. It starts from the minimal merge patch and re-adds entries of the
// previous overlay that remain no-ops, then verifies the result.
func localPatchFor(central []byte, previous any, edited []byte) (map[string]any, error) {
	var centralConfig Config
	if err := decodeStrict(central, &centralConfig); err != nil {
		return nil, fmt.Errorf("invalid central config: %w", err)
	}
	centralConfig.normalizeCollections()
	canonical, err := json.Marshal(&centralConfig)
	if err != nil {
		return nil, fmt.Errorf("encode central config: %w", err)
	}
	var base, target any
	if err := decodeAny(canonical, &base); err != nil {
		return nil, err
	}
	if err := decodeAny(edited, &target); err != nil {
		return nil, err
	}

	patch := map[string]any{}
	if required, changed := diffValues(base, target); changed {
		patch = required.(map[string]any)
	}
	if old, ok := previous.(map[string]any); ok {
		preserveRedundant(patch, old, base, target)
	}
	if !reflect.DeepEqual(mergeValues(deepCopy(base), patch), target) {
		return nil, errors.New("internal error: local config patch does not reproduce the edited config")
	}
	return patch, nil
}

// diffValues returns the JSON merge patch that turns source into target and
// whether the two differ at all.
func diffValues(source, target any) (any, bool) {
	sourceObject, sourceIsObject := source.(map[string]any)
	targetObject, targetIsObject := target.(map[string]any)
	if !sourceIsObject || !targetIsObject {
		if reflect.DeepEqual(source, target) {
			return nil, false
		}
		return target, true
	}
	patch := map[string]any{}
	for key, targetValue := range targetObject {
		sourceValue, exists := sourceObject[key]
		if !exists {
			patch[key] = targetValue
			continue
		}
		if sub, changed := diffValues(sourceValue, targetValue); changed {
			patch[key] = sub
		}
	}
	for key := range sourceObject {
		if _, exists := targetObject[key]; !exists {
			patch[key] = nil
		}
	}
	if len(patch) == 0 {
		return nil, false
	}
	return patch, true
}

// preserveRedundant copies entries from the previous overlay into patch when
// they are absent from patch and applying them to central still yields
// target, so that deliberate pins survive an edit. patch stays a valid patch
// for target because only verified no-ops are added.
func preserveRedundant(patch, previous map[string]any, central, target any) {
	centralObject, _ := central.(map[string]any)
	targetObject, _ := target.(map[string]any)
	for key, oldValue := range previous {
		if existing, exists := patch[key]; exists {
			existingObject, existingIsObject := existing.(map[string]any)
			oldObject, oldIsObject := oldValue.(map[string]any)
			if existingIsObject && oldIsObject {
				preserveRedundant(existingObject, oldObject, centralObject[key], targetObject[key])
			}
			continue
		}
		targetValue, inTarget := targetObject[key]
		if oldValue == nil {
			if !inTarget {
				patch[key] = nil
			}
			continue
		}
		if inTarget && reflect.DeepEqual(mergeValues(deepCopy(centralObject[key]), oldValue), targetValue) {
			patch[key] = oldValue
		}
	}
}

func deepCopy(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, entry := range typed {
			result[key] = deepCopy(entry)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, entry := range typed {
			result[index] = deepCopy(entry)
		}
		return result
	default:
		return value
	}
}

// Overlay applies the local overlay to an in-memory central config and
// returns the validated effective config. central itself is not modified.
func (s Source) Overlay(central *Config) (*Config, error) {
	if central == nil {
		return nil, errors.New("config must not be nil")
	}
	data, err := json.Marshal(central)
	if err != nil {
		return nil, fmt.Errorf("encode central config: %w", err)
	}
	return s.overlay(data)
}

func (s Source) overlay(central []byte) (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("find home directory: %w", err)
	}
	data, applied, err := applyLocalOverlay(central, s.LocalPath)
	if err != nil {
		return nil, err
	}
	cfg, err := parseV2(data, home)
	if err != nil && applied {
		return nil, fmt.Errorf("with local config %q applied: %w", s.LocalPath, err)
	}
	return cfg, err
}

// readCentralV2 reads the central config and returns it in the current
// format. Version 2 files are checked structurally and returned unchanged;
// version 1 files are migrated in memory. Nothing here touches the
// filesystem beyond reading the file, so the result is safe to edit on any
// machine.
func readCentralV2(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read central config %q: %w", path, err)
	}
	if err := ValidateJSON(data); err != nil {
		return nil, fmt.Errorf("invalid central config: %w", err)
	}
	version, err := configVersion(data)
	if err != nil {
		return nil, fmt.Errorf("invalid central config: %w", err)
	}
	switch version {
	case 1:
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
		migrated, err := json.Marshal(cfg)
		if err != nil {
			return nil, fmt.Errorf("encode migrated central config: %w", err)
		}
		return migrated, nil
	case CurrentVersion:
		if err := validateRequiredFieldsV2(data); err != nil {
			return nil, fmt.Errorf("invalid central config: %w", err)
		}
		if err := decodeStrict(data, &Config{}); err != nil {
			return nil, fmt.Errorf("invalid central config: %w", err)
		}
		return data, nil
	default:
		return nil, fmt.Errorf(
			"unsupported config version %d (expected %d)",
			version,
			CurrentVersion,
		)
	}
}

// applyLocalOverlay merges the local config file, when it exists, into a
// version 2 central config and reports whether anything was applied.
func applyLocalOverlay(central []byte, localPath string) ([]byte, bool, error) {
	patch, present, err := readLocalPatch(localPath)
	if err != nil || !present {
		return central, false, err
	}
	merged, err := applyLocalPatch(central, patch, localPath)
	if err != nil {
		return nil, false, err
	}
	return merged, true, nil
}

// readLocalPatch reads the local overlay. A missing file, or an empty path,
// means there is no overlay.
func readLocalPatch(localPath string) ([]byte, bool, error) {
	if localPath == "" {
		return nil, false, nil
	}
	patch, err := os.ReadFile(localPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read local config %q: %w", localPath, err)
	}
	return patch, true, nil
}

// applyLocalPatch merges the overlay into a version 2 central config using
// JSON merge patch semantics (RFC 7386): objects merge key by key, any other
// value replaces the central value, and null removes a key. Errors that the
// overlay introduces name the local file.
func applyLocalPatch(central, patch []byte, localPath string) ([]byte, error) {
	invalid := func(err error) error {
		return fmt.Errorf("invalid local config %q: %w", localPath, err)
	}
	if err := ValidateJSON(patch); err != nil {
		return nil, invalid(err)
	}
	if firstJSONByte(patch) != '{' {
		return nil, invalid(errors.New("top-level configuration must be an object"))
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(patch, &root); err != nil {
		return nil, invalid(err)
	}
	if rawVersion, exists := root["version"]; exists {
		var version int
		if err := json.Unmarshal(rawVersion, &version); err != nil || version != CurrentVersion {
			return nil, invalid(fmt.Errorf("field %q must be %d", "version", CurrentVersion))
		}
	}
	merged, err := mergePatch(central, patch)
	if err != nil {
		return nil, invalid(err)
	}
	if err := validateRequiredFieldsV2(merged); err != nil {
		return nil, invalid(err)
	}
	if err := decodeStrict(merged, &Config{}); err != nil {
		return nil, invalid(err)
	}
	return merged, nil
}

func mergePatch(target, patch []byte) ([]byte, error) {
	var base, overlay any
	if err := decodeAny(target, &base); err != nil {
		return nil, err
	}
	if err := decodeAny(patch, &overlay); err != nil {
		return nil, err
	}
	return json.Marshal(mergeValues(base, overlay))
}

func decodeAny(data []byte, destination *any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(destination)
}

func mergeValues(target, patch any) any {
	patchObject, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	targetObject, ok := target.(map[string]any)
	if !ok {
		targetObject = map[string]any{}
	}
	for key, value := range patchObject {
		if value == nil {
			delete(targetObject, key)
			continue
		}
		targetObject[key] = mergeValues(targetObject[key], value)
	}
	return targetObject
}

func Save(path string, cfg *Config) error {
	if cfg == nil {
		return errors.New("config must not be nil")
	}
	cfg.Version = CurrentVersion
	cfg.normalizeCollections()
	return writeJSONFile(path, cfg, "central config")
}

func writeJSONFile(path string, value any, label string) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", label, err)
	}
	data = append(data, '\n')
	mode, err := fileutil.ExistingMode(path, 0o600)
	if err != nil {
		return err
	}
	if err := fileutil.WriteAtomic(path, data, mode); err != nil {
		return fmt.Errorf("write %s %q: %w", label, path, err)
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
	if err := c.validateStatic(home); err != nil {
		return err
	}
	return c.resolveProjectPaths(home)
}

// validateStatic checks everything that does not depend on the filesystem:
// the version, options, MCP definitions, and scope assignments. Project paths
// must be absolute after ~ expansion but need not exist.
func (c *Config) validateStatic(home string) error {
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

	for _, id := range sortedKeys(c.Projects) {
		if !idPattern.MatchString(id) {
			return fmt.Errorf("project ID %q must match %s", id, idPattern)
		}
		project := c.Projects[id]
		if _, err := expandProjectPath(project.Path, home); err != nil {
			return fmt.Errorf("project %q: %w", id, err)
		}
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

// resolveProjectPaths expands, checks, and canonicalizes every project path
// and rejects projects that resolve to the same directory. It rewrites the
// paths in place, so callers that intend to save the config unchanged must
// not call it.
func (c *Config) resolveProjectPaths(home string) error {
	projectIDs := sortedKeys(c.Projects)
	canonicalRoots := make(map[string]string, len(projectIDs))
	for _, id := range projectIDs {
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

// ExpandProjectPath expands a leading ~ against home and requires the result
// to be absolute. It does not consult the filesystem.
func ExpandProjectPath(path, home string) (string, error) {
	return expandProjectPath(path, home)
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
