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
