package importer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/daniel100097/mcp-manager/internal/config"
)

func TestImportCodexGlobal(t *testing.T) {
	options := testOptions(t)
	options.Agent = config.AgentCodex
	writeFile(t, filepath.Join(options.HomeDir, ".codex", "config.toml"), `
model = "gpt-5"

[mcp_servers.local]
command = "npx"
args = ["-y", "@example/local"]
env = { LOG_LEVEL = "debug", TEMPLATE = "${TOKEN}" }
env_vars = ["TOKEN"]
enabled = true

[mcp_servers.remote]
url = "https://mcp.example.com/v1"
http_headers = { X-Region = "eu", X-Literal = "{env:KEEP}" }
env_http_headers = { Authorization = "AUTH_TOKEN" }

[mcp_servers.disabled]
enabled = false
this_entry_is_intentionally_incomplete = true
`)

	got, result, err := Import(nil, options)
	if err != nil {
		t.Fatalf("Import() error = %v", err)
	}
	if result.Imported != 2 || result.Skipped != 1 || result.Added != 2 {
		t.Fatalf("result = %#v", result)
	}
	if got.Version != config.CurrentVersion {
		t.Fatalf("version = %d, want %d", got.Version, config.CurrentVersion)
	}
	local := got.MCPs["local"]
	if local.Type != "stdio" || local.Command != "npx" {
		t.Fatalf("local = %#v", local)
	}
	if !reflect.DeepEqual(got.Global.MCPs, []string{"local", "remote"}) {
		t.Fatalf("global activation = %#v", got.Global.MCPs)
	}
	if got.Global.DisabledAgents == nil {
		t.Fatal("global import left required disabledAgents map nil")
	}
	if !reflect.DeepEqual(local.Args, []string{"-y", "@example/local"}) {
		t.Fatalf("local args = %#v", local.Args)
	}
	if !reflect.DeepEqual(local.EnvFrom, []string{"TOKEN"}) {
		t.Fatalf("local envFrom = %#v", local.EnvFrom)
	}
	if local.Env["TEMPLATE"] != "${TOKEN}" {
		t.Fatalf("Codex literal env reference was changed: %#v", local.Env)
	}
	remote := got.MCPs["remote"]
	if remote.Type != "http" || remote.URL != "https://mcp.example.com/v1" {
		t.Fatalf("remote = %#v", remote)
	}
	if remote.HeadersFrom["Authorization"] != "AUTH_TOKEN" {
		t.Fatalf("remote headersFrom = %#v", remote.HeadersFrom)
	}
	if remote.Headers["X-Literal"] != "{env:KEEP}" {
		t.Fatalf("Codex literal header was changed: %#v", remote.Headers)
	}
	if _, exists := got.MCPs["disabled"]; exists {
		t.Fatalf("disabled MCP was imported: %#v", got.MCPs["disabled"])
	}
}

func TestImportClaudeProjectRegistersProjectAndConvertsExactReferences(t *testing.T) {
	options := testOptions(t)
	options.Agent = config.AgentClaude
	options.ProjectID = "demo"
	options.ProjectPath = filepath.Join(t.TempDir(), "demo")
	mustMkdir(t, options.ProjectPath)
	writeFile(t, filepath.Join(options.ProjectPath, ".mcp.json"), `{
  // Type omission is valid for Claude stdio entries.
  "mcpServers": {
    "local": {
      "command": "node",
      "args": ["server.js"],
      "env": {
        "TOKEN": "${TOKEN}",
        "LITERAL": "prefix-${TOKEN}"
      },
    },
    "remote": {
      "type": "http",
      "url": "https://example.com/mcp",
      "headers": {
        "Authorization": "${HTTP_TOKEN}",
        "X-Literal": "Bearer ${HTTP_TOKEN}"
      }
    },
    "off": { "enabled": false }
  }
}`)

	existing := &config.Config{}
	got, result, err := Import(existing, options)
	if err != nil {
		t.Fatalf("Import() error = %v", err)
	}
	if !result.ProjectRegistered || result.Scope != "demo" || result.Skipped != 1 {
		t.Fatalf("result = %#v", result)
	}
	project := got.Projects["demo"]
	if project.Path != options.ProjectPath {
		t.Fatalf("project path = %q, want %q", project.Path, options.ProjectPath)
	}
	if !reflect.DeepEqual(project.MCPs, []string{"local", "remote"}) {
		t.Fatalf("project activation = %#v", project.MCPs)
	}
	if project.DisabledAgents == nil {
		t.Fatal("registered project has nil disabledAgents")
	}
	local := got.MCPs["local"]
	if !reflect.DeepEqual(local.EnvFrom, []string{"TOKEN"}) || local.Env["LITERAL"] != "prefix-${TOKEN}" {
		t.Fatalf("local environment = %#v / %#v", local.Env, local.EnvFrom)
	}
	remote := got.MCPs["remote"]
	if remote.HeadersFrom["Authorization"] != "HTTP_TOKEN" {
		t.Fatalf("remote headersFrom = %#v", remote.HeadersFrom)
	}
	if remote.Headers["X-Literal"] != "Bearer ${HTTP_TOKEN}" {
		t.Fatalf("partial reference must remain literal: %#v", remote.Headers)
	}
	if existing.Version != 0 || existing.Global.MCPs != nil || existing.Global.DisabledAgents != nil ||
		existing.Projects != nil || existing.MCPs != nil {
		t.Fatalf("successful import mutated original: %#v", existing)
	}
}

func TestImportOpenCodeGlobal(t *testing.T) {
	options := testOptions(t)
	options.Agent = config.AgentOpenCode
	writeFile(t, filepath.Join(options.UserConfigDir, "opencode", "opencode.json"), `{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "local": {
      "type": "local",
      "command": ["bunx", "-y", "local-server"],
      "environment": {
        "TOKEN": "{env:TOKEN}",
        "TEMPLATE": "prefix-{env:TOKEN}"
      },
      "enabled": true
    },
    "remote": {
      "type": "remote",
      "url": "https://remote.example/mcp",
      "headers": {
        "Authorization": "{env:AUTH_TOKEN}",
        "X-Region": "us"
      }
    },
    "disabled": {
      "type": "local",
      "command": ["do-not-import"],
      "enabled": false
    }
  },
}`)

	got, result, err := Import(nil, options)
	if err != nil {
		t.Fatalf("Import() error = %v", err)
	}
	if result.Imported != 2 || result.Skipped != 1 || result.Added != 2 {
		t.Fatalf("result = %#v", result)
	}
	if !reflect.DeepEqual(got.Global.MCPs, []string{"local", "remote"}) {
		t.Fatalf("global activation = %#v", got.Global.MCPs)
	}
	local := got.MCPs["local"]
	if local.Command != "bunx" || !reflect.DeepEqual(local.Args, []string{"-y", "local-server"}) {
		t.Fatalf("local command = %#v", local)
	}
	if !reflect.DeepEqual(local.EnvFrom, []string{"TOKEN"}) || local.Env["TEMPLATE"] != "prefix-{env:TOKEN}" {
		t.Fatalf("local environment = %#v / %#v", local.Env, local.EnvFrom)
	}
	remote := got.MCPs["remote"]
	if remote.HeadersFrom["Authorization"] != "AUTH_TOKEN" || remote.Headers["X-Region"] != "us" {
		t.Fatalf("remote headers = %#v / %#v", remote.Headers, remote.HeadersFrom)
	}
}

func TestImportMatchingTransportAddsActivationAndPreservesMetadata(t *testing.T) {
	options := testOptions(t)
	options.Agent = config.AgentClaude
	writeFile(t, filepath.Join(options.HomeDir, ".claude.json"), `{
  "mcpServers": {"tools": {"type": "stdio", "command": "tool"}}
}`)
	projectPath := t.TempDir()
	existing := &config.Config{
		Schema:  "schema.json",
		Version: config.CurrentVersion,
		Global: config.Scope{
			MCPs: []string{"unrelated"},
			DisabledAgents: map[string][]config.Agent{
				"unrelated": {config.AgentClaude},
			},
		},
		Projects: map[string]config.Project{
			"kept": {
				Path: projectPath, MCPs: []string{"tools"},
				DisabledAgents: map[string][]config.Agent{
					"tools": {config.AgentOpenCode},
				},
			},
		},
		MCPs: map[string]config.MCP{
			"tools": {
				Type: "stdio", Command: "tool", Args: []string{}, Env: map[string]string{},
			},
			"unrelated": {
				Type: "stdio", Command: "other",
			},
		},
	}
	before := marshalConfig(t, existing)

	got, result, err := Import(existing, options)
	if err != nil {
		t.Fatalf("Import() error = %v", err)
	}
	if result.Activated != 1 || result.Added != 0 || result.Unchanged != 0 {
		t.Fatalf("result = %#v", result)
	}
	if !reflect.DeepEqual(got.Global.MCPs, []string{"tools", "unrelated"}) {
		t.Fatalf("global activation = %#v", got.Global.MCPs)
	}
	if !reflect.DeepEqual(got.Global.DisabledAgents, existing.Global.DisabledAgents) {
		t.Fatalf("global disabledAgents changed: %#v", got.Global.DisabledAgents)
	}
	if !reflect.DeepEqual(got.Projects["kept"], existing.Projects["kept"]) {
		t.Fatalf("unrelated project scope changed: %#v", got.Projects["kept"])
	}
	if after := marshalConfig(t, existing); !reflect.DeepEqual(after, before) {
		t.Fatalf("successful merge mutated existing:\n before %s\n after  %s", before, after)
	}
}

func TestImportAlreadyActiveTransportIsIdempotentAndPreservesScopeMetadata(t *testing.T) {
	options := testOptions(t)
	options.Agent = config.AgentClaude
	writeFile(t, filepath.Join(options.HomeDir, ".claude.json"), `{
  "mcpServers": {"tools": {"type": "stdio", "command": "tool"}}
}`)
	projectPath := t.TempDir()
	existing := &config.Config{
		Version: config.CurrentVersion,
		Global: config.Scope{
			MCPs: []string{"tools"},
			DisabledAgents: map[string][]config.Agent{
				"tools": {config.AgentOpenCode},
			},
		},
		Projects: map[string]config.Project{
			"kept": {
				Path: projectPath, MCPs: []string{"tools"},
				DisabledAgents: map[string][]config.Agent{
					"tools": {config.AgentClaude},
				},
			},
		},
		MCPs: map[string]config.MCP{
			"tools": {Type: "stdio", Command: "tool"},
		},
	}
	before := marshalConfig(t, existing)

	got, result, err := Import(existing, options)
	if err != nil {
		t.Fatalf("Import() error = %v", err)
	}
	if result.Added != 0 || result.Activated != 0 || result.Unchanged != 1 {
		t.Fatalf("result = %#v", result)
	}
	if !reflect.DeepEqual(got.Global, existing.Global) {
		t.Fatalf("global scope changed: %#v", got.Global)
	}
	if !reflect.DeepEqual(got.Projects, existing.Projects) {
		t.Fatalf("project scopes changed: %#v", got.Projects)
	}
	if after := marshalConfig(t, existing); !reflect.DeepEqual(after, before) {
		t.Fatalf("idempotent import mutated existing:\n before %s\n after  %s", before, after)
	}
}

func TestImportIntoRegisteredProjectPreservesEveryOtherScopeSetting(t *testing.T) {
	options := testOptions(t)
	options.Agent = config.AgentClaude
	options.ProjectID = "kept"
	options.ProjectPath = t.TempDir()
	writeFile(t, filepath.Join(options.ProjectPath, ".mcp.json"), `{
  "mcpServers": {"tools": {"type": "stdio", "command": "tool"}}
}`)
	existing := &config.Config{
		Version: config.CurrentVersion,
		Global: config.Scope{
			MCPs: []string{"tools"},
			DisabledAgents: map[string][]config.Agent{
				"tools": {config.AgentCodex},
			},
		},
		Projects: map[string]config.Project{
			"kept": {
				Path: options.ProjectPath, MCPs: []string{"other"},
				DisabledAgents: map[string][]config.Agent{
					"other": {config.AgentOpenCode},
				},
			},
		},
		MCPs: map[string]config.MCP{
			"tools": {Type: "stdio", Command: "tool"},
			"other": {Type: "stdio", Command: "other"},
		},
	}

	got, result, err := Import(existing, options)
	if err != nil {
		t.Fatalf("Import() error = %v", err)
	}
	if result.ProjectRegistered || result.Added != 0 || result.Activated != 1 {
		t.Fatalf("result = %#v", result)
	}
	if !reflect.DeepEqual(got.Global, existing.Global) {
		t.Fatalf("global scope changed: %#v", got.Global)
	}
	project := got.Projects["kept"]
	if !reflect.DeepEqual(project.MCPs, []string{"other", "tools"}) {
		t.Fatalf("project activation = %#v", project.MCPs)
	}
	if !reflect.DeepEqual(project.DisabledAgents, existing.Projects["kept"].DisabledAgents) {
		t.Fatalf("project disabledAgents changed: %#v", project.DisabledAgents)
	}
}

func TestImportUnwrapsManagedWrapperEntries(t *testing.T) {
	options := testOptions(t)
	options.Agent = config.AgentClaude
	writeFile(t, filepath.Join(options.HomeDir, ".claude.json"), `{
  "mcpServers": {
    "tools": {
      "type": "stdio",
      "command": "/usr/local/bin/mcp-manager",
      "args": ["stdio", "--config", "/central/config.json", "--config-local", "/central/config.local.json", "tools"],
      "env": {"TOKEN": "${TOKEN}"}
    }
  }
}`)
	definition := config.MCP{
		Type: "stdio", Command: "tool", Args: []string{"--serve"},
		Env: map[string]string{"LOG_LEVEL": "debug"}, EnvFrom: []string{"TOKEN"},
	}
	existing := &config.Config{
		Version: config.CurrentVersion,
		Global: config.Scope{
			MCPs: []string{}, DisabledAgents: map[string][]config.Agent{},
		},
		Projects: map[string]config.Project{},
		MCPs:     map[string]config.MCP{"tools": definition},
	}

	got, result, err := Import(existing, options)
	if err != nil {
		t.Fatalf("Import() error = %v", err)
	}
	if result.Added != 0 || result.Activated != 1 || result.Unchanged != 0 {
		t.Fatalf("result = %#v", result)
	}
	if !reflect.DeepEqual(got.MCPs["tools"], definition) {
		t.Fatalf("wrapper import changed the central definition:\n got  %#v\n want %#v", got.MCPs["tools"], definition)
	}
	if !reflect.DeepEqual(got.Global.MCPs, []string{"tools"}) {
		t.Fatalf("global activation = %#v, want [tools]", got.Global.MCPs)
	}
}

func TestImportRejectsWrapperEntriesThatCannotBeResolved(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		wantErr string
	}{
		{
			name:    "unknown central MCP",
			source:  `{"mcpServers":{"tools":{"type":"stdio","command":"mcp-manager","args":["stdio","tools"]}}}`,
			wantErr: `central config does not define "tools"`,
		},
		{
			name:    "different name",
			source:  `{"mcpServers":{"alias":{"type":"stdio","command":"mcp-manager","args":["stdio","other"]}}}`,
			wantErr: `wrapper for a differently named MCP "other"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := testOptions(t)
			options.Agent = config.AgentClaude
			writeFile(t, filepath.Join(options.HomeDir, ".claude.json"), test.source)
			existing := &config.Config{
				Version: config.CurrentVersion,
				Global: config.Scope{
					MCPs: []string{"other"}, DisabledAgents: map[string][]config.Agent{},
				},
				Projects: map[string]config.Project{},
				MCPs:     map[string]config.MCP{"other": {Type: "stdio", Command: "other"}},
			}
			before := marshalConfig(t, existing)

			got, _, err := Import(existing, options)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Import() error = %v, want substring %q", err, test.wantErr)
			}
			if got != nil {
				t.Fatalf("Import() config = %#v, want nil on error", got)
			}
			if after := marshalConfig(t, existing); !reflect.DeepEqual(after, before) {
				t.Fatalf("failed import mutated existing:\n before %s\n after  %s", before, after)
			}
		})
	}
}

func TestImportConflictDoesNotMutateOriginal(t *testing.T) {
	options := testOptions(t)
	options.Agent = config.AgentClaude
	options.ProjectID = "new-project"
	options.ProjectPath = t.TempDir()
	writeFile(t, filepath.Join(options.ProjectPath, ".mcp.json"), `{
  "mcpServers": {"tools": {"type": "stdio", "command": "new-command"}}
}`)
	existing := &config.Config{
		Version: config.CurrentVersion,
		Global: config.Scope{
			MCPs: []string{"tools"}, DisabledAgents: map[string][]config.Agent{},
		},
		Projects: map[string]config.Project{},
		MCPs: map[string]config.MCP{
			"tools": {Type: "stdio", Command: "old-command"},
		},
	}
	before := marshalConfig(t, existing)

	got, _, err := Import(existing, options)
	if err == nil || !strings.Contains(err.Error(), "transport conflicts") {
		t.Fatalf("Import() error = %v, want transport conflict", err)
	}
	if got != nil {
		t.Fatalf("Import() config = %#v, want nil on conflict", got)
	}
	if after := marshalConfig(t, existing); !reflect.DeepEqual(after, before) {
		t.Fatalf("failed merge mutated existing:\n before %s\n after  %s", before, after)
	}
}

func TestImportRejectsMalformedOrUnsupportedNativeEntries(t *testing.T) {
	tests := []struct {
		name    string
		agent   config.Agent
		data    string
		wantErr string
	}{
		{
			name: "Codex env_vars type", agent: config.AgentCodex,
			data: `[mcp_servers.bad]
command = "tool"
env_vars = "TOKEN"
`,
			wantErr: `field "env_vars" must be an array of strings`,
		},
		{
			name: "Claude unsupported SSE", agent: config.AgentClaude,
			data:    `{"mcpServers":{"bad":{"type":"sse","url":"https://example.com"}}}`,
			wantErr: "unsupported Claude transport type",
		},
		{
			name: "Claude HTTP requires an explicit type", agent: config.AgentClaude,
			data:    `{"mcpServers":{"bad":{"url":"https://example.com"}}}`,
			wantErr: `must explicitly set type "http"`,
		},
		{
			name: "OpenCode command must be array", agent: config.AgentOpenCode,
			data:    `{"mcp":{"bad":{"type":"local","command":"tool"}}}`,
			wantErr: `field "command" must be an array of strings`,
		},
		{
			name: "renamed env reference is not representable", agent: config.AgentClaude,
			data:    `{"mcpServers":{"bad":{"command":"tool","env":{"ALIAS":"${TOKEN}"}}}}`,
			wantErr: "envFrom cannot represent differently named variables",
		},
		{
			name: "enabled must be boolean", agent: config.AgentOpenCode,
			data:    `{"mcp":{"bad":{"enabled":"false","type":"local","command":["tool"]}}}`,
			wantErr: `field "enabled" must be a boolean`,
		},
		{
			name: "duplicate JSON key", agent: config.AgentClaude,
			data:    `{"mcpServers":{},"mcpServers":{}}`,
			wantErr: "duplicate key",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := testOptions(t)
			options.Agent = test.agent
			writeSource(t, options, test.data)
			_, _, err := Import(nil, options)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Import() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestSameTransportNormalizesEmptyCollectionsButChecksMapKeys(t *testing.T) {
	base := config.MCP{
		Type: "stdio", Command: "tool", Args: []string{}, Env: map[string]string{}, EnvFrom: []string{},
	}
	if !sameTransport(base, config.MCP{Type: "stdio", Command: "tool"}) {
		t.Fatal("nil and empty transport collections should compare equal")
	}
	left := config.MCP{Type: "stdio", Command: "tool", Env: map[string]string{"LEFT": ""}}
	right := config.MCP{Type: "stdio", Command: "tool", Env: map[string]string{"RIGHT": ""}}
	if sameTransport(left, right) {
		t.Fatal("maps with different keys and empty values compared equal")
	}
	left = config.MCP{Type: "http", URL: "https://example.com", Headers: map[string]string{"X-Left": ""}}
	right = config.MCP{Type: "http", URL: "https://example.com", Headers: map[string]string{"X-Right": ""}}
	if sameTransport(left, right) {
		t.Fatal("case-folded maps with different keys and empty values compared equal")
	}
}

func TestImportRejectsProjectRegistrationCollisions(t *testing.T) {
	options := testOptions(t)
	options.Agent = config.AgentClaude
	options.ProjectID = "new"
	options.ProjectPath = t.TempDir()
	existing := &config.Config{
		Version: config.CurrentVersion,
		Global: config.Scope{
			MCPs: []string{}, DisabledAgents: map[string][]config.Agent{},
		},
		Projects: map[string]config.Project{
			"old": {Path: options.ProjectPath, MCPs: []string{}, DisabledAgents: map[string][]config.Agent{}},
		},
		MCPs: map[string]config.MCP{},
	}

	_, _, err := Import(existing, options)
	if err == nil || !strings.Contains(err.Error(), "already registered as") {
		t.Fatalf("Import() error = %v, want registration collision", err)
	}
}

func TestImportMissingSourceIsClear(t *testing.T) {
	options := testOptions(t)
	options.Agent = config.AgentCodex
	_, _, err := Import(nil, options)
	if err == nil || !strings.Contains(err.Error(), "read codex global MCP config") ||
		!strings.Contains(err.Error(), filepath.Join(".codex", "config.toml")) {
		t.Fatalf("Import() error = %v", err)
	}
}

func testOptions(t *testing.T) Options {
	t.Helper()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	userConfig := filepath.Join(base, "user-config")
	mustMkdir(t, home)
	mustMkdir(t, userConfig)
	return Options{HomeDir: home, UserConfigDir: userConfig}
}

func writeSource(t *testing.T, options Options, data string) {
	t.Helper()
	var path string
	switch options.Agent {
	case config.AgentCodex:
		path = filepath.Join(options.HomeDir, ".codex", "config.toml")
	case config.AgentClaude:
		path = filepath.Join(options.HomeDir, ".claude.json")
	case config.AgentOpenCode:
		path = filepath.Join(options.UserConfigDir, "opencode", "opencode.json")
	default:
		t.Fatalf("unsupported test agent %q", options.Agent)
	}
	writeFile(t, path, data)
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func marshalConfig(t *testing.T, value *config.Config) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
