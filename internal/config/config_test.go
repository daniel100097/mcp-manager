package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseValidV2Config(t *testing.T) {
	home := t.TempDir()
	projectRoot := filepath.Join(home, "projects", "demo")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	data := mustJSON(t, map[string]any{
		"version": CurrentVersion,
		"options": map[string]any{"inlineSecrets": true},
		"global": map[string]any{
			"mcps": []string{"docs"},
			"disabledAgents": map[string]any{
				"docs": []string{"opencode"},
			},
		},
		"projects": map[string]any{
			"demo": map[string]any{
				"path": "~/projects/demo",
				"mcps": []string{"local-tools", "docs"},
				"disabledAgents": map[string]any{
					"local-tools": []string{"claude"},
				},
			},
		},
		"mcps": map[string]any{
			"local-tools": map[string]any{
				"type": "stdio", "command": "npx", "args": []string{"-y", "tools"},
				"env": map[string]string{"LOG_LEVEL": "debug"}, "envFrom": []string{"API_TOKEN"},
			},
			"docs": map[string]any{
				"type": "http", "url": "https://example.com/mcp",
				"headers":     map[string]string{"X-Region": "eu"},
				"headersFrom": map[string]string{"Authorization": "DOCS_AUTH"},
			},
		},
	})

	cfg, err := Parse(data, home)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	resolved, err := filepath.EvalSymlinks(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	project := cfg.Projects["demo"]
	if project.Path != resolved {
		t.Fatalf("project path = %q, want %q", project.Path, resolved)
	}
	if !reflect.DeepEqual(cfg.Global.MCPs, []string{"docs"}) {
		t.Fatalf("global MCPs = %#v", cfg.Global.MCPs)
	}
	if !reflect.DeepEqual(cfg.Global.DisabledAgents["docs"], []Agent{AgentOpenCode}) {
		t.Fatalf("global disabled agents = %#v", cfg.Global.DisabledAgents)
	}
	if !reflect.DeepEqual(project.MCPs, []string{"local-tools", "docs"}) {
		t.Fatalf("project MCPs = %#v", project.MCPs)
	}
	if !reflect.DeepEqual(project.DisabledAgents["local-tools"], []Agent{AgentClaude}) {
		t.Fatalf("project disabled agents = %#v", project.DisabledAgents)
	}
	if !cfg.Options.InlineSecrets {
		t.Fatal("inlineSecrets was not decoded")
	}
	if got := cfg.MCPs["docs"].HeadersFrom["Authorization"]; got != "DOCS_AUTH" {
		t.Fatalf("headersFrom value = %q", got)
	}
}

func TestParseMigratesV1InMemory(t *testing.T) {
	home := t.TempDir()
	projectRoot := filepath.Join(home, "projects", "demo")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	legacy := mustJSON(t, map[string]any{
		"version":  1,
		"options":  map[string]any{"inlineSecrets": true},
		"projects": map[string]string{"demo": "~/projects/demo"},
		"mcps": map[string]any{
			"local-tools": map[string]any{
				"type": "stdio", "command": "tool",
				"global": false, "projects": []string{"demo"},
				"disabledAgents": []string{"opencode"},
			},
			"docs": map[string]any{
				"type": "http", "url": "https://example.com/mcp",
				"global": true, "projects": []string{"demo"},
				"disabledAgents": []string{"claude"},
			},
		},
	})

	cfg, err := Parse(legacy, home)
	if err != nil {
		t.Fatalf("Parse(v1) error = %v", err)
	}
	if cfg.Version != CurrentVersion {
		t.Fatalf("migrated version = %d, want %d", cfg.Version, CurrentVersion)
	}
	if !reflect.DeepEqual(cfg.Global.MCPs, []string{"docs"}) {
		t.Fatalf("global MCPs = %#v, want docs", cfg.Global.MCPs)
	}
	if !reflect.DeepEqual(cfg.Global.DisabledAgents["docs"], []Agent{AgentClaude}) {
		t.Fatalf("global disabled agents = %#v", cfg.Global.DisabledAgents)
	}
	project := cfg.Projects["demo"]
	if !reflect.DeepEqual(project.MCPs, []string{"docs", "local-tools"}) {
		t.Fatalf("project MCPs = %#v", project.MCPs)
	}
	if !reflect.DeepEqual(project.DisabledAgents["docs"], []Agent{AgentClaude}) {
		t.Fatalf("project docs disabled agents = %#v", project.DisabledAgents)
	}
	if !reflect.DeepEqual(project.DisabledAgents["local-tools"], []Agent{AgentOpenCode}) {
		t.Fatalf("project local-tools disabled agents = %#v", project.DisabledAgents)
	}
	if cfg.MCPs["docs"].URL != "https://example.com/mcp" {
		t.Fatalf("migrated definition = %#v", cfg.MCPs["docs"])
	}
}

func TestParseRejectsInvalidV2Configs(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		wantErr string
	}{
		{
			name:    "duplicate key",
			data:    `{"version":2,"global":{"mcps":[]},"projects":{},"mcps":{},"version":2}`,
			wantErr: `duplicate key "version"`,
		},
		{
			name:    "unknown field",
			data:    `{"version":2,"global":{"mcps":[]},"projects":{},"mcps":{},"surprise":true}`,
			wantErr: "unknown field",
		},
		{
			name:    "trailing value",
			data:    `{"version":2,"global":{"mcps":[]},"projects":{},"mcps":{}} true`,
			wantErr: "multiple JSON values",
		},
		{
			name:    "unsupported version",
			data:    `{"version":3}`,
			wantErr: "unsupported config version",
		},
		{
			name:    "missing global scope",
			data:    `{"version":2,"projects":{},"mcps":{}}`,
			wantErr: `required field "global" is missing`,
		},
		{
			name:    "null global scope",
			data:    `{"version":2,"global":null,"projects":{},"mcps":{}}`,
			wantErr: `field "global" must be an object`,
		},
		{
			name:    "missing global MCP list",
			data:    `{"version":2,"global":{},"projects":{},"mcps":{}}`,
			wantErr: `global scope is missing required field "mcps"`,
		},
		{
			name:    "null global MCP list",
			data:    `{"version":2,"global":{"mcps":null},"projects":{},"mcps":{}}`,
			wantErr: `global scope field "mcps" must be an array`,
		},
		{
			name:    "null global disabled agents",
			data:    `{"version":2,"global":{"mcps":[],"disabledAgents":null},"projects":{},"mcps":{}}`,
			wantErr: `global scope field "disabledAgents" must be an object`,
		},
		{
			name:    "project is not an object",
			data:    `{"version":2,"global":{"mcps":[]},"projects":{"demo":null},"mcps":{}}`,
			wantErr: `project "demo" must be an object`,
		},
		{
			name:    "project missing path",
			data:    `{"version":2,"global":{"mcps":[]},"projects":{"demo":{"mcps":[]}},"mcps":{}}`,
			wantErr: `project "demo" is missing required field "path"`,
		},
		{
			name:    "null project path",
			data:    `{"version":2,"global":{"mcps":[]},"projects":{"demo":{"path":null,"mcps":[]}},"mcps":{}}`,
			wantErr: `project "demo" field "path" must be a string`,
		},
		{
			name:    "project missing MCP list",
			data:    `{"version":2,"global":{"mcps":[]},"projects":{"demo":{"path":"~"}},"mcps":{}}`,
			wantErr: `project "demo" is missing required field "mcps"`,
		},
		{
			name:    "null project MCP list",
			data:    `{"version":2,"global":{"mcps":[]},"projects":{"demo":{"path":"~","mcps":null}},"mcps":{}}`,
			wantErr: `project "demo" field "mcps" must be an array`,
		},
		{
			name:    "null project disabled agents",
			data:    `{"version":2,"global":{"mcps":[]},"projects":{"demo":{"path":"~","mcps":[],"disabledAgents":null}},"mcps":{}}`,
			wantErr: `project "demo" field "disabledAgents" must be an object`,
		},
		{
			name:    "null disabled agent list",
			data:    `{"version":2,"global":{"mcps":["demo"],"disabledAgents":{"demo":null}},"projects":{},"mcps":{"demo":{"type":"stdio","command":"tool"}}}`,
			wantErr: `global scope disabledAgents for MCP "demo" must be an array`,
		},
		{
			name:    "MCP is not an object",
			data:    `{"version":2,"global":{"mcps":[]},"projects":{},"mcps":{"demo":null}}`,
			wantErr: `MCP "demo" must be an object`,
		},
		{
			name:    "MCP missing type",
			data:    `{"version":2,"global":{"mcps":[]},"projects":{},"mcps":{"demo":{}}}`,
			wantErr: `MCP "demo" is missing required field "type"`,
		},
		{
			name:    "invalid project ID",
			data:    `{"version":2,"global":{"mcps":[]},"projects":{"bad.id":{"path":"~","mcps":[]}},"mcps":{}}`,
			wantErr: "project ID",
		},
		{
			name:    "invalid MCP name",
			data:    `{"version":2,"global":{"mcps":[]},"projects":{},"mcps":{"bad.id":{"type":"stdio","command":"tool"}}}`,
			wantErr: "MCP name",
		},
		{
			name:    "unknown global MCP",
			data:    `{"version":2,"global":{"mcps":["missing"]},"projects":{},"mcps":{}}`,
			wantErr: `global scope references unknown MCP "missing"`,
		},
		{
			name:    "duplicate global MCP",
			data:    `{"version":2,"global":{"mcps":["demo","demo"]},"projects":{},"mcps":{"demo":{"type":"stdio","command":"tool"}}}`,
			wantErr: `global scope lists MCP "demo" more than once`,
		},
		{
			name:    "unknown project MCP",
			data:    `{"version":2,"global":{"mcps":[]},"projects":{"demo":{"path":"~","mcps":["missing"]}},"mcps":{}}`,
			wantErr: `project "demo" references unknown MCP "missing"`,
		},
		{
			name:    "duplicate project MCP",
			data:    `{"version":2,"global":{"mcps":[]},"projects":{"demo":{"path":"~","mcps":["tools","tools"]}},"mcps":{"tools":{"type":"stdio","command":"tool"}}}`,
			wantErr: `project "demo" lists MCP "tools" more than once`,
		},
		{
			name:    "global exclusion for inactive MCP",
			data:    `{"version":2,"global":{"mcps":[],"disabledAgents":{"demo":["codex"]}},"projects":{},"mcps":{"demo":{"type":"stdio","command":"tool"}}}`,
			wantErr: `global scope disabledAgents key "demo" is not active`,
		},
		{
			name:    "project exclusion for inactive MCP",
			data:    `{"version":2,"global":{"mcps":[]},"projects":{"demo":{"path":"~","mcps":[],"disabledAgents":{"tools":["claude"]}}},"mcps":{"tools":{"type":"stdio","command":"tool"}}}`,
			wantErr: `project "demo" disabledAgents key "tools" is not active`,
		},
		{
			name:    "unknown disabled agent",
			data:    `{"version":2,"global":{"mcps":["demo"],"disabledAgents":{"demo":["cursor"]}},"projects":{},"mcps":{"demo":{"type":"stdio","command":"tool"}}}`,
			wantErr: `unknown disabled agent "cursor"`,
		},
		{
			name:    "duplicate disabled agent",
			data:    `{"version":2,"global":{"mcps":["demo"],"disabledAgents":{"demo":["codex","codex"]}},"projects":{},"mcps":{"demo":{"type":"stdio","command":"tool"}}}`,
			wantErr: `lists disabled agent "codex" more than once`,
		},
		{
			name:    "bad HTTP URL",
			data:    `{"version":2,"global":{"mcps":[]},"projects":{},"mcps":{"demo":{"type":"http","url":"file:///tmp/mcp"}}}`,
			wantErr: "absolute http or https URL",
		},
		{
			name:    "mixed transport fields",
			data:    `{"version":2,"global":{"mcps":[]},"projects":{},"mcps":{"demo":{"type":"stdio","command":"tool","url":"https://example.com"}}}`,
			wantErr: "stdio transport cannot set",
		},
		{
			name:    "environment conflict",
			data:    `{"version":2,"global":{"mcps":[]},"projects":{},"mcps":{"demo":{"type":"stdio","command":"tool","env":{"TOKEN":"literal"},"envFrom":["TOKEN"]}}}`,
			wantErr: "both env and envFrom",
		},
		{
			name:    "legacy activation in v2 definition",
			data:    `{"version":2,"global":{"mcps":[]},"projects":{},"mcps":{"demo":{"type":"stdio","command":"tool","global":true}}}`,
			wantErr: `unknown field "global"`,
		},
		{
			name:    "unknown stdio mode",
			data:    `{"version":2,"options":{"stdioMode":"proxy"},"global":{"mcps":[]},"projects":{},"mcps":{}}`,
			wantErr: `options.stdioMode must be either "wrapper" or "direct"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse([]byte(test.data), t.TempDir())
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Parse() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestParseV1RejectsInvalidAssignments(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		wantErr string
	}{
		{
			name:    "missing global flag",
			data:    `{"version":1,"projects":{},"mcps":{"demo":{"type":"stdio","command":"tool","projects":[]}}}`,
			wantErr: `missing required field "global"`,
		},
		{
			name:    "unknown project",
			data:    `{"version":1,"projects":{},"mcps":{"demo":{"type":"stdio","command":"tool","global":false,"projects":["missing"]}}}`,
			wantErr: `references unknown project "missing"`,
		},
		{
			name:    "duplicate project",
			data:    `{"version":1,"projects":{"demo":"~"},"mcps":{"tools":{"type":"stdio","command":"tool","global":false,"projects":["demo","demo"]}}}`,
			wantErr: `lists project "demo" more than once`,
		},
		{
			name:    "unknown disabled agent on inactive MCP",
			data:    `{"version":1,"projects":{},"mcps":{"demo":{"type":"stdio","command":"tool","global":false,"projects":[],"disabledAgents":["cursor"]}}}`,
			wantErr: `unknown disabled agent "cursor"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse([]byte(test.data), t.TempDir())
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Parse(v1) error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestOptionsWrapStdioDefaultsToWrapper(t *testing.T) {
	tests := map[string]bool{"": true, StdioModeWrapper: true, StdioModeDirect: false}
	for mode, want := range tests {
		if got := (Options{StdioMode: mode}).WrapStdio(); got != want {
			t.Fatalf("Options{StdioMode: %q}.WrapStdio() = %v, want %v", mode, got, want)
		}
	}

	data := `{"version":2,"options":{"stdioMode":"direct"},"global":{"mcps":[]},"projects":{},"mcps":{}}`
	cfg, err := Parse([]byte(data), t.TempDir())
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.Options.StdioMode != StdioModeDirect || cfg.Options.WrapStdio() {
		t.Fatalf("parsed options = %#v, want direct stdio mode", cfg.Options)
	}
}

func TestParseRejectsDuplicateProjectRoots(t *testing.T) {
	root := t.TempDir()
	data := mustJSON(t, map[string]any{
		"version": CurrentVersion,
		"global":  map[string]any{"mcps": []string{}},
		"projects": map[string]any{
			"one": map[string]any{"path": root, "mcps": []string{}},
			"two": map[string]any{"path": filepath.Join(root, "."), "mcps": []string{}},
		},
		"mcps": map[string]any{},
	})
	_, err := Parse(data, root)
	if err == nil || !strings.Contains(err.Error(), "resolve to the same path") {
		t.Fatalf("Parse() error = %v, want duplicate path error", err)
	}
}

func TestSaveEmitsV2AndNormalizesCollections(t *testing.T) {
	base := t.TempDir()
	projectRoot := filepath.Join(base, "project")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "nested", "config.json")
	cfg := &Config{
		Version: 1,
		Projects: map[string]Project{
			"demo": {Path: projectRoot},
		},
	}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if cfg.Version != CurrentVersion {
		t.Fatalf("in-memory version = %d, want %d", cfg.Version, CurrentVersion)
	}
	if cfg.Global.MCPs == nil || cfg.Global.DisabledAgents == nil {
		t.Fatalf("global collections were not normalized: %#v", cfg.Global)
	}
	project := cfg.Projects["demo"]
	if project.MCPs == nil || project.DisabledAgents == nil {
		t.Fatalf("project collections were not normalized: %#v", project)
	}
	if cfg.MCPs == nil {
		t.Fatal("MCP definitions map was not normalized")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("new mode = %o, want 600", got)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load(saved config) error = %v", err)
	}
	if loaded.Version != CurrentVersion {
		t.Fatalf("saved version = %d, want %d", loaded.Version, CurrentVersion)
	}
	if loaded.Global.DisabledAgents == nil || loaded.Projects["demo"].DisabledAgents == nil {
		t.Fatal("omitted disabledAgents maps were not normalized on load")
	}

	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	cfg.Options.InlineSecrets = true
	if err := Save(path, cfg); err != nil {
		t.Fatalf("second Save() error = %v", err)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("preserved mode = %o, want 640", got)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSourceLoadAppliesLocalOverlay(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "real")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	central := filepath.Join(base, "config.json")
	local := LocalPathFor(central)
	writeTestFile(t, central, `{
  "version": 2,
  "options": {"stdioMode": "direct"},
  "global": {"mcps": ["shared"], "disabledAgents": {"shared": ["codex"]}},
  "projects": {"api": {"path": "/does/not/exist", "mcps": ["shared", "gone"]}},
  "mcps": {
    "shared": {"type": "http", "url": "https://example.com/mcp"},
    "gone": {"type": "stdio", "command": "old"}
  }
}`)
	writeTestFile(t, local, `{
  "options": {"inlineSecrets": true},
  "global": {"mcps": ["shared", "extra"]},
  "projects": {"api": {"path": `+jsonString(t, root)+`, "mcps": ["shared"]}},
  "mcps": {
    "gone": null,
    "extra": {"type": "stdio", "command": "extra-server", "args": ["--flag"]}
  }
}`)

	if _, err := Load(central); err == nil || !strings.Contains(err.Error(), "/does/not/exist") {
		t.Fatalf("Load(central only) error = %v, want missing project path", err)
	}
	cfg, err := Source{Path: central, LocalPath: local}.Load()
	if err != nil {
		t.Fatalf("Source.Load() error = %v", err)
	}
	if cfg.Options != (Options{InlineSecrets: true, StdioMode: StdioModeDirect}) {
		t.Fatalf("options = %#v, want merged options", cfg.Options)
	}
	if !reflect.DeepEqual(cfg.Global.MCPs, []string{"shared", "extra"}) {
		t.Fatalf("global MCPs = %#v, want overlay list", cfg.Global.MCPs)
	}
	if !reflect.DeepEqual(cfg.Global.DisabledAgents["shared"], []Agent{AgentCodex}) {
		t.Fatalf("global disabledAgents = %#v, want central exclusions kept", cfg.Global.DisabledAgents)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if project := cfg.Projects["api"]; project.Path != canonicalRoot || !reflect.DeepEqual(project.MCPs, []string{"shared"}) {
		t.Fatalf("project = %#v, want overlay path and MCP list", project)
	}
	if _, exists := cfg.MCPs["gone"]; exists {
		t.Fatal("null in the overlay did not remove the MCP definition")
	}
	if extra := cfg.MCPs["extra"]; extra.Command != "extra-server" || !reflect.DeepEqual(extra.Args, []string{"--flag"}) {
		t.Fatalf("added MCP = %#v", extra)
	}
}

func TestSourceLoadWithoutLocalFileMatchesLoad(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	central := filepath.Join(base, "config.json")
	writeTestFile(t, central, `{"version":2,"global":{"mcps":["tool"]},"projects":{"api":{"path":`+jsonString(t, root)+`,"mcps":[]}},"mcps":{"tool":{"type":"stdio","command":"tool"}}}`)

	plain, err := Load(central)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	layered, err := Source{Path: central, LocalPath: LocalPathFor(central)}.Load()
	if err != nil {
		t.Fatalf("Source.Load() error = %v", err)
	}
	if !reflect.DeepEqual(plain, layered) {
		t.Fatalf("missing overlay changed the result:\n plain   %#v\n layered %#v", plain, layered)
	}
}

func TestSourceLoadRejectsInvalidLocalOverlays(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	central := filepath.Join(base, "config.json")
	writeTestFile(t, central, `{"version":2,"global":{"mcps":["tool"]},"projects":{"api":{"path":`+jsonString(t, root)+`,"mcps":[]}},"mcps":{"tool":{"type":"stdio","command":"tool"}}}`)
	local := filepath.Join(base, "overrides.json")

	tests := []struct {
		name    string
		local   string
		wantErr string
	}{
		{name: "malformed", local: `{`, wantErr: "invalid local config"},
		{name: "duplicate key", local: `{"mcps":{},"mcps":{}}`, wantErr: `duplicate key "mcps"`},
		{name: "array", local: `[]`, wantErr: "must be an object"},
		{name: "wrong version", local: `{"version":1}`, wantErr: `field "version" must be 2`},
		{name: "null version", local: `{"version":null}`, wantErr: `field "version" must be 2`},
		{name: "unknown field", local: `{"surprise":true}`, wantErr: "unknown field"},
		{name: "removes required field", local: `{"mcps":null}`, wantErr: `required field "mcps" is missing`},
		{name: "unknown reference", local: `{"global":{"mcps":["missing"]}}`, wantErr: `references unknown MCP "missing"`},
		{name: "missing directory", local: `{"projects":{"api":{"path":"/does/not/exist"}}}`, wantErr: "/does/not/exist"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			writeTestFile(t, local, test.local)
			_, err := Source{Path: central, LocalPath: local}.Load()
			if err == nil || !strings.Contains(err.Error(), test.wantErr) || !strings.Contains(err.Error(), local) {
				t.Fatalf("Source.Load() error = %v, want %q naming %s", err, test.wantErr, local)
			}
		})
	}
}

func TestSourceLoadAppliesOverlayToVersion1Central(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	central := filepath.Join(base, "config.json")
	local := LocalPathFor(central)
	writeTestFile(t, central, `{"version":1,"projects":{"api":`+jsonString(t, root)+`},"mcps":{"tool":{"type":"stdio","command":"tool","global":true,"projects":["api"]}}}`)
	writeTestFile(t, local, `{"mcps":{"tool":{"command":"replacement"}}}`)

	cfg, err := Source{Path: central, LocalPath: local}.Load()
	if err != nil {
		t.Fatalf("Source.Load() error = %v", err)
	}
	if cfg.MCPs["tool"].Command != "replacement" {
		t.Fatalf("command = %q, want overlay value", cfg.MCPs["tool"].Command)
	}
	if !reflect.DeepEqual(cfg.Global.MCPs, []string{"tool"}) || !reflect.DeepEqual(cfg.Projects["api"].MCPs, []string{"tool"}) {
		t.Fatalf("migrated activations lost: global %#v, project %#v", cfg.Global.MCPs, cfg.Projects["api"].MCPs)
	}
}

func TestLocalPathFor(t *testing.T) {
	tests := map[string]string{
		"config.json":                         "config.local.json",
		filepath.Join("dir", "central.jsonc"): filepath.Join("dir", "central.local.jsonc"),
		"central":                             "central.local.json",
	}
	for input, want := range tests {
		if got := LocalPathFor(input); got != want {
			t.Fatalf("LocalPathFor(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestOpenEditWritesLocalOverlayWhenPresent(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	central := filepath.Join(base, "config.json")
	local := LocalPathFor(central)
	writeTestFile(t, central, `{
  "version": 2,
  "global": {"mcps": ["shared"]},
  "projects": {"api": {"path": "/missing/on/this/machine", "mcps": []}},
  "mcps": {
    "shared": {"type": "http", "url": "https://example.com/mcp"},
    "tool": {"type": "stdio", "command": "tool"}
  }
}`)
	writeTestFile(t, local, `{
  "projects": {"api": {"path": `+jsonString(t, root)+`}},
  "mcps": {"shared": {"url": "https://example.com/mcp"}}
}`)
	centralBefore := readTestFile(t, central)
	source := Source{Path: central, LocalPath: local}

	edit, err := source.OpenEdit()
	if err != nil {
		t.Fatalf("OpenEdit() error = %v", err)
	}
	if !edit.WritesLocal() || edit.Target() != local || edit.Label() != "local config" {
		t.Fatalf("edit targets %q (local %v), want the overlay", edit.Target(), edit.WritesLocal())
	}
	if edit.Config.Projects["api"].Path != root {
		t.Fatalf("editable path = %q, want the overlay value as written", edit.Config.Projects["api"].Path)
	}

	project := edit.Config.Projects["api"]
	project.MCPs = append(project.MCPs, "tool")
	edit.Config.Projects["api"] = project
	effective, err := edit.Effective()
	if err != nil {
		t.Fatalf("Effective() error = %v", err)
	}
	if !reflect.DeepEqual(effective.Projects["api"].MCPs, []string{"tool"}) {
		t.Fatalf("effective project MCPs = %#v", effective.Projects["api"].MCPs)
	}
	if err := edit.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if after := readTestFile(t, central); after != centralBefore {
		t.Fatalf("Save() modified the central config:\n before %s\n after  %s", centralBefore, after)
	}
	want := map[string]any{
		"projects": map[string]any{"api": map[string]any{"path": root, "mcps": []any{"tool"}}},
		"mcps":     map[string]any{"shared": map[string]any{"url": "https://example.com/mcp"}},
	}
	if got := decodeTestJSON(t, local); !reflect.DeepEqual(got, want) {
		t.Fatalf("overlay after Save() = %#v, want %#v", got, want)
	}
	reloaded, err := source.Load()
	if err != nil {
		t.Fatalf("Load() after Save() error = %v", err)
	}
	if !reflect.DeepEqual(reloaded.Projects["api"].MCPs, []string{"tool"}) {
		t.Fatalf("reloaded project MCPs = %#v", reloaded.Projects["api"].MCPs)
	}
}

func TestOpenEditDropsOverridesThatReturnToCentralValues(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	central := filepath.Join(base, "config.json")
	local := LocalPathFor(central)
	writeTestFile(t, central, `{"version":2,"global":{"mcps":["shared"]},"projects":{"api":{"path":`+jsonString(t, root)+`,"mcps":[]}},"mcps":{"shared":{"type":"http","url":"https://example.com/mcp"},"tool":{"type":"stdio","command":"tool"}}}`)
	writeTestFile(t, local, `{"global":{"mcps":["shared","tool"]}}`)

	edit, err := Source{Path: central, LocalPath: local}.OpenEdit()
	if err != nil {
		t.Fatalf("OpenEdit() error = %v", err)
	}
	edit.Config.Global.MCPs = []string{"shared"}
	delete(edit.Config.MCPs, "tool")
	if _, err := edit.Effective(); err != nil {
		t.Fatalf("Effective() error = %v", err)
	}
	if err := edit.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	want := map[string]any{"mcps": map[string]any{"tool": nil}}
	if got := decodeTestJSON(t, local); !reflect.DeepEqual(got, want) {
		t.Fatalf("overlay after Save() = %#v, want %#v", got, want)
	}
}

func TestOpenEditWritesCentralWithoutLocalOverlay(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "project"), 0o755); err != nil {
		t.Fatal(err)
	}
	central := filepath.Join(home, "config.json")
	writeTestFile(t, central, `{"version":2,"global":{"mcps":[]},"projects":{"api":{"path":"~/project","mcps":[]}},"mcps":{"tool":{"type":"stdio","command":"tool"}}}`)
	source := Source{Path: central, LocalPath: LocalPathFor(central)}

	edit, err := source.OpenEdit()
	if err != nil {
		t.Fatalf("OpenEdit() error = %v", err)
	}
	if edit.WritesLocal() || edit.Target() != central || edit.Label() != "central config" {
		t.Fatalf("edit targets %q (local %v), want the central config", edit.Target(), edit.WritesLocal())
	}
	if edit.Config.Projects["api"].Path != "~/project" {
		t.Fatalf("editable path = %q, want the value as written", edit.Config.Projects["api"].Path)
	}
	edit.Config.Global.MCPs = []string{"tool"}
	effective, err := edit.Effective()
	if err != nil {
		t.Fatalf("Effective() error = %v", err)
	}
	if !filepath.IsAbs(effective.Projects["api"].Path) || !reflect.DeepEqual(effective.Global.MCPs, []string{"tool"}) {
		t.Fatalf("effective config = %#v", effective)
	}
	if err := edit.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	saved := readTestFile(t, central)
	if !strings.Contains(saved, `"~/project"`) || !strings.Contains(saved, `"tool"`) {
		t.Fatalf("saved central config = %s", saved)
	}
	if _, err := os.Stat(source.LocalPath); !os.IsNotExist(err) {
		t.Fatalf("Save() created the overlay %s: %v", source.LocalPath, err)
	}
}

func TestDiffValuesProducesMergePatch(t *testing.T) {
	var source, target any
	if err := decodeAny([]byte(`{"a":1,"b":{"c":[1,2],"d":"x"},"e":"gone","f":{"g":true}}`), &source); err != nil {
		t.Fatal(err)
	}
	if err := decodeAny([]byte(`{"a":1,"b":{"c":[1,3],"d":"x"},"f":{"g":true,"h":{}}}`), &target); err != nil {
		t.Fatal(err)
	}
	patch, changed := diffValues(source, target)
	if !changed {
		t.Fatal("diffValues() reported no change")
	}
	var want any
	if err := decodeAny([]byte(`{"b":{"c":[1,3]},"e":null,"f":{"h":{}}}`), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(patch, want) {
		t.Fatalf("patch = %#v, want %#v", patch, want)
	}
	if got := mergeValues(deepCopy(source), patch); !reflect.DeepEqual(got, target) {
		t.Fatalf("applying the patch = %#v, want %#v", got, target)
	}
	if _, changed := diffValues(target, deepCopy(target)); changed {
		t.Fatal("diffValues() reported a change for equal values")
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func decodeTestJSON(t *testing.T, path string) any {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(readTestFile(t, path)), &value); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return value
}

func jsonString(t *testing.T, value string) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
