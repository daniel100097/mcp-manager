package syncer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/daniel100097/mcp-manager/internal/config"
)

func TestSyncGeneratesAllFormatsAndPreservesOtherSettings(t *testing.T) {
	base := t.TempDir()
	options := testOptions(t, base)
	projectOne := filepath.Join(base, "projects", "one")
	projectTwo := filepath.Join(base, "projects", "two")
	mustMkdir(t, projectOne)
	mustMkdir(t, projectTwo)

	cfg := &config.Config{
		Version: config.CurrentVersion,
		Options: config.Options{StdioMode: config.StdioModeDirect},
		Global: config.Scope{
			MCPs: []string{"filesystem", "remote"},
			DisabledAgents: map[string][]config.Agent{
				"filesystem": {config.AgentClaude},
				"remote":     {config.AgentOpenCode},
			},
		},
		Projects: map[string]config.Project{
			"one": {
				Path: projectOne, MCPs: []string{"filesystem", "remote"},
				DisabledAgents: map[string][]config.Agent{
					"filesystem": {config.AgentOpenCode},
					"remote":     {config.AgentClaude},
				},
			},
			"two": {Path: projectTwo, MCPs: []string{}, DisabledAgents: map[string][]config.Agent{}},
		},
		MCPs: map[string]config.MCP{
			"filesystem": {
				Type: "stdio", Command: "npx", Args: []string{"-y", "@mcp/filesystem"},
				Env: map[string]string{"LOG_LEVEL": "info"}, EnvFrom: []string{"MCP_TOKEN"},
			},
			"remote": {
				Type: "http", URL: "https://mcp.example.com/v1",
				Headers:     map[string]string{"X-Region": "eu"},
				HeadersFrom: map[string]string{"Authorization": "MCP_AUTH"},
			},
		},
	}

	globalCodex := filepath.Join(options.HomeDir, ".codex", "config.toml")
	globalClaude := filepath.Join(options.HomeDir, ".claude.json")
	globalOpenCode := filepath.Join(options.UserConfigDir, "opencode", "opencode.json")
	mustWrite(t, globalCodex, []byte("model = \"gpt-5\"\n\n[mcp_servers.old]\ncommand = \"old\"\n\n[profiles.work]\nmodel = \"gpt-5-high\"\n"), 0o640)
	mustWrite(t, globalClaude, []byte(`{"theme":"dark","mcpServers":{"old":{"command":"old"}},"projects":{"elsewhere":{"trusted":true}}}`), 0o600)
	mustWrite(t, globalOpenCode, []byte("{\n  // OpenCode accepts JSONC\n  \"theme\": \"system\",\n  \"mcp\": {\"old\": {\"type\": \"local\", \"command\": [\"old\"]}},\n}\n"), 0o600)

	staleProjectTargets := []struct {
		path string
		data string
	}{
		{filepath.Join(projectTwo, ".codex", "config.toml"), "model = \"keep\"\n[mcp_servers.old]\ncommand = \"old\"\n"},
		{filepath.Join(projectTwo, ".mcp.json"), `{"keep":true,"mcpServers":{"old":{"command":"old"}}}`},
		{filepath.Join(projectTwo, "opencode.json"), `{"keep":true,"mcp":{"old":{"type":"local","command":["old"]}}}`},
	}
	for _, target := range staleProjectTargets {
		mustWrite(t, target.path, []byte(target.data), 0o600)
	}

	result, err := Sync(cfg, options)
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(result.Changes) != 9 {
		t.Fatalf("len(Changes) = %d, want 9", len(result.Changes))
	}

	codex := readTOML(t, globalCodex)
	if codex["model"] != "gpt-5" {
		t.Fatalf("Codex model setting was not preserved: %#v", codex["model"])
	}
	if profile := nestedMap(t, codex, "profiles", "work"); profile["model"] != "gpt-5-high" {
		t.Fatalf("Codex profile was not preserved: %#v", profile)
	}
	codexServers := nestedMap(t, codex, "mcp_servers")
	assertKeys(t, codexServers, "filesystem", "remote")
	filesystem := nestedMap(t, codexServers, "filesystem")
	if filesystem["command"] != "npx" || !sliceHasString(filesystem["env_vars"], "MCP_TOKEN") {
		t.Fatalf("unexpected Codex stdio server: %#v", filesystem)
	}
	remote := nestedMap(t, codexServers, "remote")
	if got := nestedMap(t, remote, "env_http_headers")["Authorization"]; got != "MCP_AUTH" {
		t.Fatalf("Codex env_http_headers Authorization = %#v", got)
	}

	claude := readJSON(t, globalClaude)
	if claude["theme"] != "dark" || nestedMap(t, claude, "projects", "elsewhere")["trusted"] != true {
		t.Fatalf("Claude unrelated settings were not preserved: %#v", claude)
	}
	claudeServers := nestedMap(t, claude, "mcpServers")
	assertKeys(t, claudeServers, "remote")
	claudeRemote := nestedMap(t, claudeServers, "remote")
	if got := nestedMap(t, claudeRemote, "headers")["Authorization"]; got != "${MCP_AUTH}" {
		t.Fatalf("Claude Authorization header = %#v", got)
	}

	openCode := readJSON(t, globalOpenCode)
	if openCode["theme"] != "system" {
		t.Fatalf("OpenCode theme was not preserved: %#v", openCode)
	}
	openCodeServers := nestedMap(t, openCode, "mcp")
	assertKeys(t, openCodeServers, "filesystem")
	openCodeFilesystem := nestedMap(t, openCodeServers, "filesystem")
	if openCodeFilesystem["type"] != "local" || openCodeFilesystem["enabled"] != true {
		t.Fatalf("unexpected OpenCode stdio server: %#v", openCodeFilesystem)
	}
	if got := nestedMap(t, openCodeFilesystem, "environment")["MCP_TOKEN"]; got != "{env:MCP_TOKEN}" {
		t.Fatalf("OpenCode MCP_TOKEN = %#v", got)
	}

	projectClaude := readJSON(t, filepath.Join(projectOne, ".mcp.json"))
	assertKeys(t, nestedMap(t, projectClaude, "mcpServers"), "filesystem")
	projectOpenCode := readJSON(t, filepath.Join(projectOne, "opencode.json"))
	assertKeys(t, nestedMap(t, projectOpenCode, "mcp"), "remote")
	projectCodex := readTOML(t, filepath.Join(projectOne, ".codex", "config.toml"))
	assertKeys(t, nestedMap(t, projectCodex, "mcp_servers"), "filesystem", "remote")

	if mode := fileMode(t, globalCodex); mode != 0o640 {
		t.Fatalf("existing mode = %o, want 640", mode)
	}
	if mode := fileMode(t, filepath.Join(projectOne, ".mcp.json")); mode != 0o600 {
		t.Fatalf("new mode = %o, want 600", mode)
	}

	staleCodex := readTOML(t, staleProjectTargets[0].path)
	if staleCodex["model"] != "keep" {
		t.Fatalf("stale Codex cleanup lost unrelated setting: %#v", staleCodex)
	}
	if _, exists := staleCodex["mcp_servers"]; exists {
		t.Fatalf("stale Codex MCP section still exists: %#v", staleCodex)
	}
	for _, target := range staleProjectTargets[1:] {
		document := readJSON(t, target.path)
		if document["keep"] != true {
			t.Fatalf("stale cleanup lost unrelated setting in %s", target.path)
		}
		if _, exists := document[mapKeyForPath(target.path)]; exists {
			t.Fatalf("stale MCP section still exists in %s: %#v", target.path, document)
		}
	}

	second, err := Sync(cfg, options)
	if err != nil {
		t.Fatalf("second Sync() error = %v", err)
	}
	if len(second.Changes) != 0 || second.Unchanged != 9 {
		t.Fatalf("second Sync() = %#v, want idempotent result", second)
	}
}

func wrapperTestConfig() *config.Config {
	return &config.Config{
		Version: config.CurrentVersion,
		Global: config.Scope{
			MCPs: []string{"local", "remote"}, DisabledAgents: map[string][]config.Agent{},
		},
		Projects: map[string]config.Project{},
		MCPs: map[string]config.MCP{
			"local": {
				Type: "stdio", Command: "npx", Args: []string{"-y", "@mcp/filesystem"},
				Env: map[string]string{"LOG_LEVEL": "info"}, EnvFrom: []string{"MCP_TOKEN"},
			},
			"remote": {
				Type: "http", URL: "https://mcp.example.com/v1",
				Headers: map[string]string{"X-Region": "eu"},
			},
		},
	}
}

func TestSyncWrapsStdioServersByDefault(t *testing.T) {
	options := testOptions(t, t.TempDir())
	options.ConfigPath = StandardConfigPath(options.UserConfigDir)
	cfg := wrapperTestConfig()

	result, err := Sync(cfg, options)
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(result.Changes) != 3 {
		t.Fatalf("len(Changes) = %d, want 3", len(result.Changes))
	}
	wantArgs := []any{"stdio", "local"}

	codex := nestedMap(t, readTOML(t, filepath.Join(options.HomeDir, ".codex", "config.toml")), "mcp_servers")
	codexLocal := nestedMap(t, codex, "local")
	if codexLocal["command"] != "mcp-manager" || !reflect.DeepEqual(codexLocal["args"], wantArgs) {
		t.Fatalf("Codex wrapper entry = %#v", codexLocal)
	}
	if _, exists := codexLocal["env"]; exists {
		t.Fatalf("Codex wrapper entry leaked literal env: %#v", codexLocal)
	}
	if !sliceHasString(codexLocal["env_vars"], "MCP_TOKEN") {
		t.Fatalf("Codex wrapper entry does not forward envFrom: %#v", codexLocal)
	}
	if nestedMap(t, codex, "remote")["url"] != "https://mcp.example.com/v1" {
		t.Fatalf("Codex http entry changed: %#v", codex["remote"])
	}

	claude := nestedMap(t, readJSON(t, filepath.Join(options.HomeDir, ".claude.json")), "mcpServers")
	claudeLocal := nestedMap(t, claude, "local")
	if claudeLocal["type"] != "stdio" || claudeLocal["command"] != "mcp-manager" || !reflect.DeepEqual(claudeLocal["args"], wantArgs) {
		t.Fatalf("Claude wrapper entry = %#v", claudeLocal)
	}
	if env := nestedMap(t, claudeLocal, "env"); !reflect.DeepEqual(env, map[string]any{"MCP_TOKEN": "${MCP_TOKEN}"}) {
		t.Fatalf("Claude wrapper env = %#v, want only the envFrom reference", env)
	}
	if nestedMap(t, claude, "remote")["url"] != "https://mcp.example.com/v1" {
		t.Fatalf("Claude http entry changed: %#v", claude["remote"])
	}

	openCode := nestedMap(t, readJSON(t, filepath.Join(options.UserConfigDir, "opencode", "opencode.json")), "mcp")
	openCodeLocal := nestedMap(t, openCode, "local")
	if openCodeLocal["type"] != "local" || !reflect.DeepEqual(openCodeLocal["command"], []any{"mcp-manager", "stdio", "local"}) {
		t.Fatalf("OpenCode wrapper entry = %#v", openCodeLocal)
	}
	if env := nestedMap(t, openCodeLocal, "environment"); !reflect.DeepEqual(env, map[string]any{"MCP_TOKEN": "{env:MCP_TOKEN}"}) {
		t.Fatalf("OpenCode wrapper environment = %#v, want only the envFrom reference", env)
	}

	second, err := Sync(cfg, options)
	if err != nil {
		t.Fatalf("second Sync() error = %v", err)
	}
	if len(second.Changes) != 0 || second.Unchanged != 3 {
		t.Fatalf("second Sync() = %#v, want idempotent result", second)
	}
}

func TestSyncEmbedsNonDefaultConfigPathInWrapperArgs(t *testing.T) {
	base := t.TempDir()
	options := testOptions(t, base)
	options.ConfigPath = filepath.Join(base, "elsewhere", "central.json")
	cfg := wrapperTestConfig()

	if _, err := Sync(cfg, options); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	wantArgs := []any{"stdio", "--config", options.ConfigPath, "local"}
	codexLocal := nestedMap(t, readTOML(t, filepath.Join(options.HomeDir, ".codex", "config.toml")), "mcp_servers", "local")
	if !reflect.DeepEqual(codexLocal["args"], wantArgs) {
		t.Fatalf("Codex wrapper args = %#v, want %#v", codexLocal["args"], wantArgs)
	}
	claudeLocal := nestedMap(t, readJSON(t, filepath.Join(options.HomeDir, ".claude.json")), "mcpServers", "local")
	if !reflect.DeepEqual(claudeLocal["args"], wantArgs) {
		t.Fatalf("Claude wrapper args = %#v, want %#v", claudeLocal["args"], wantArgs)
	}
	openCodeLocal := nestedMap(t, readJSON(t, filepath.Join(options.UserConfigDir, "opencode", "opencode.json")), "mcp", "local")
	wantCommand := append([]any{"mcp-manager"}, wantArgs...)
	if !reflect.DeepEqual(openCodeLocal["command"], wantCommand) {
		t.Fatalf("OpenCode wrapper command = %#v, want %#v", openCodeLocal["command"], wantCommand)
	}
}

func TestSyncDirectModeWritesRealCommand(t *testing.T) {
	options := testOptions(t, t.TempDir())
	options.ConfigPath = filepath.Join(options.HomeDir, "custom.json")
	cfg := wrapperTestConfig()
	cfg.Options.StdioMode = config.StdioModeDirect

	if _, err := Sync(cfg, options); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	claudeLocal := nestedMap(t, readJSON(t, filepath.Join(options.HomeDir, ".claude.json")), "mcpServers", "local")
	if claudeLocal["command"] != "npx" || !reflect.DeepEqual(claudeLocal["args"], []any{"-y", "@mcp/filesystem"}) {
		t.Fatalf("direct mode entry = %#v", claudeLocal)
	}
	if env := nestedMap(t, claudeLocal, "env"); env["LOG_LEVEL"] != "info" {
		t.Fatalf("direct mode env = %#v, want literal LOG_LEVEL", env)
	}
}

func TestSyncInlineSecrets(t *testing.T) {
	options := testOptions(t, t.TempDir())
	options.InlineSecrets = true
	secrets := map[string]string{
		"MCP_TOKEN": "stdio-secret-value",
		"MCP_AUTH":  "Bearer http-secret-value",
	}
	options.LookupEnv = func(name string) (string, bool) {
		value, exists := secrets[name]
		return value, exists
	}
	cfg := &config.Config{
		Version: config.CurrentVersion,
		Global: config.Scope{
			MCPs: []string{"local", "remote"}, DisabledAgents: map[string][]config.Agent{},
		},
		Projects: map[string]config.Project{},
		MCPs: map[string]config.MCP{
			"local":  {Type: "stdio", Command: "tool", EnvFrom: []string{"MCP_TOKEN"}},
			"remote": {Type: "http", URL: "https://example.com/mcp", HeadersFrom: map[string]string{"Authorization": "MCP_AUTH"}},
		},
	}

	result, err := Sync(cfg, options)
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(result.Changes) != 3 {
		t.Fatalf("len(Changes) = %d, want 3", len(result.Changes))
	}
	paths := []string{
		filepath.Join(options.HomeDir, ".codex", "config.toml"),
		filepath.Join(options.HomeDir, ".claude.json"),
		filepath.Join(options.UserConfigDir, "opencode", "opencode.json"),
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, secret := range secrets {
			if !strings.Contains(text, secret) {
				t.Fatalf("%s does not contain materialized secret", path)
			}
		}
		for _, marker := range []string{"env_vars", "env_http_headers", "${MCP_", "{env:MCP_"} {
			if strings.Contains(text, marker) {
				t.Fatalf("%s still contains reference marker %q: %s", path, marker, text)
			}
		}
	}
}

func TestSyncMissingInlineSecretDoesNotWrite(t *testing.T) {
	options := testOptions(t, t.TempDir())
	options.InlineSecrets = true
	options.LookupEnv = func(string) (string, bool) { return "", false }
	cfg := &config.Config{
		Version: config.CurrentVersion,
		Global: config.Scope{
			MCPs: []string{"local"}, DisabledAgents: map[string][]config.Agent{},
		},
		Projects: map[string]config.Project{},
		MCPs: map[string]config.MCP{
			"local": {Type: "stdio", Command: "tool", EnvFrom: []string{"MISSING_TOKEN"}},
		},
	}
	path := filepath.Join(options.HomeDir, ".codex", "config.toml")
	original := []byte("model = \"keep\"\n")
	mustWrite(t, path, original, 0o600)

	_, err := Sync(cfg, options)
	if err == nil || !strings.Contains(err.Error(), "MISSING_TOKEN") {
		t.Fatalf("Sync() error = %v, want missing variable", err)
	}
	assertFileBytes(t, path, original)
}

func TestSyncPrevalidatesEveryDestinationBeforeWriting(t *testing.T) {
	options := testOptions(t, t.TempDir())
	cfg := &config.Config{
		Version: config.CurrentVersion,
		Global: config.Scope{
			MCPs: []string{"local"}, DisabledAgents: map[string][]config.Agent{},
		},
		Projects: map[string]config.Project{},
		MCPs: map[string]config.MCP{
			"local": {Type: "stdio", Command: "tool"},
		},
	}
	codexPath := filepath.Join(options.HomeDir, ".codex", "config.toml")
	original := []byte("model = \"keep\"\n")
	mustWrite(t, codexPath, original, 0o600)
	mustWrite(t, filepath.Join(options.HomeDir, ".claude.json"), []byte(`{"keep":true}`), 0o600)
	mustWrite(t, filepath.Join(options.UserConfigDir, "opencode", "opencode.json"), []byte(`{"broken":`), 0o600)

	_, err := Sync(cfg, options)
	if err == nil || !strings.Contains(err.Error(), "parse opencode config") {
		t.Fatalf("Sync() error = %v, want OpenCode parse error", err)
	}
	assertFileBytes(t, codexPath, original)
}

func TestSyncDryRunDoesNotWrite(t *testing.T) {
	options := testOptions(t, t.TempDir())
	options.DryRun = true
	cfg := &config.Config{
		Version: config.CurrentVersion,
		Global: config.Scope{
			MCPs: []string{"local"}, DisabledAgents: map[string][]config.Agent{},
		},
		Projects: map[string]config.Project{},
		MCPs: map[string]config.MCP{
			"local": {Type: "stdio", Command: "tool"},
		},
	}
	result, err := Sync(cfg, options)
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(result.Changes) != 3 {
		t.Fatalf("len(Changes) = %d, want 3", len(result.Changes))
	}
	for _, change := range result.Changes {
		if change.Applied {
			t.Fatalf("dry-run change marked applied: %#v", change)
		}
		if _, err := os.Stat(change.Path); !os.IsNotExist(err) {
			t.Fatalf("dry run created %s", change.Path)
		}
	}
}

func TestMergeJSONRejectsDuplicateKeys(t *testing.T) {
	_, err := mergeJSON([]byte(`{"keep":1,"keep":2}`), "mcp", map[string]any{"demo": map[string]any{}})
	if err == nil || !strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("mergeJSON() error = %v, want duplicate key error", err)
	}
}

func TestDefaultConfigPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("MCP_MANAGER_CONFIG", "")
	path, err := DefaultConfigPath()
	if err != nil {
		t.Fatalf("DefaultConfigPath() error = %v", err)
	}
	want := filepath.Join(home, ".config", "mcp-manager", "config.json")
	if path != want {
		t.Fatalf("DefaultConfigPath() = %q, want %q", path, want)
	}
}

func testOptions(t *testing.T, base string) Options {
	t.Helper()
	home := filepath.Join(base, "home")
	userConfig := filepath.Join(base, "user-config")
	mustMkdir(t, home)
	mustMkdir(t, userConfig)
	return Options{
		HomeDir:       home,
		UserConfigDir: userConfig,
		LookupEnv:     func(string) (string, bool) { return "", false },
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

func readTOML(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := toml.Unmarshal(data, &document); err != nil {
		t.Fatalf("parse %s: %v\n%s", path, err, data)
	}
	return document
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("parse %s: %v\n%s", path, err, data)
	}
	return document
}

func nestedMap(t *testing.T, root map[string]any, path ...string) map[string]any {
	t.Helper()
	current := root
	for _, key := range path {
		next, ok := current[key].(map[string]any)
		if !ok {
			t.Fatalf("%s is %T, want map[string]any (root: %#v)", strings.Join(path, "."), current[key], root)
		}
		current = next
	}
	return current
}

func assertKeys(t *testing.T, values map[string]any, expected ...string) {
	t.Helper()
	actual := make([]string, 0, len(values))
	for key := range values {
		actual = append(actual, key)
	}
	if !reflect.DeepEqual(stringSet(actual), stringSet(expected)) {
		t.Fatalf("keys = %v, want %v", actual, expected)
	}
}

func stringSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func sliceHasString(value any, expected string) bool {
	switch values := value.(type) {
	case []any:
		for _, value := range values {
			if value == expected {
				return true
			}
		}
	case []string:
		for _, value := range values {
			if value == expected {
				return true
			}
		}
	}
	return false
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func assertFileBytes(t *testing.T, path string, expected []byte) {
	t.Helper()
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("%s changed:\n got: %q\nwant: %q", path, actual, expected)
	}
}

func mapKeyForPath(path string) string {
	if strings.HasSuffix(path, ".mcp.json") {
		return "mcpServers"
	}
	return "mcp"
}
