package syncer

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/daniel100097/mcp-manager/internal/config"
	"github.com/daniel100097/mcp-manager/internal/importer"
)

func TestToolFilteringForcesWrapperInDirectModeAndRoundTrips(t *testing.T) {
	for _, mode := range []string{config.StdioModeDirect, config.StdioModeWrapper} {
		t.Run(mode, func(t *testing.T) {
			base := t.TempDir()
			options := testOptions(t, base)
			options.ConfigPath = filepath.Join(base, "central.json")
			options.LocalConfigPath = filepath.Join(base, "overrides.json")
			root := filepath.Join(base, "project")
			mustMkdir(t, root)
			cfg := wrapperTestConfig()
			cfg.Options.StdioMode = mode
			cfg.Global.DisabledTools = map[string][]string{"local": {"delete"}, "remote": {"write"}}
			cfg.Projects["demo"] = config.Project{Path: root, MCPs: []string{"local", "remote"}, DisabledTools: map[string][]string{"local": {"project_only"}}}
			remote := cfg.MCPs["remote"]
			remote.HeadersFrom = map[string]string{"Authorization": "TOKEN", "X-Other": "TOKEN"}
			cfg.MCPs["remote"] = remote
			if _, err := Sync(cfg, options); err != nil {
				t.Fatal(err)
			}
			for _, agent := range []config.Agent{config.AgentCodex, config.AgentClaude, config.AgentOpenCode} {
				for _, project := range []string{"", "demo"} {
					projectPath := ""
					if project != "" {
						projectPath = root
					}
					imported, result, err := importer.Import(cfg, importer.Options{Agent: agent, ProjectID: project, ProjectPath: projectPath, HomeDir: options.HomeDir, UserConfigDir: options.UserConfigDir})
					if err != nil {
						t.Fatalf("import %s %s: %v", agent, project, err)
					}
					if result.Unchanged != 2 || !reflect.DeepEqual(imported.Global.DisabledTools, cfg.Global.DisabledTools) || !reflect.DeepEqual(imported.Projects["demo"].DisabledTools, cfg.Projects["demo"].DisabledTools) {
						t.Fatalf("import changed tool policies: %#v", result)
					}
				}
			}
			codex := nestedMap(t, readTOML(t, filepath.Join(options.HomeDir, ".codex", "config.toml")), "mcp_servers")
			for _, name := range []string{"local", "remote"} {
				entry := nestedMap(t, codex, name)
				if entry["command"] != "mcp-manager" {
					t.Fatalf("%s bypassed filtering: %#v", name, entry)
				}
				args := entry["args"].([]any)
				want := []any{"stdio", "--config", options.ConfigPath, "--config-local", options.LocalConfigPath, name}
				if !reflect.DeepEqual(args, want) {
					t.Fatalf("wrapper config paths = %v; want %v", args, want)
				}
			}
			remoteEntry := nestedMap(t, codex, "remote")
			if !reflect.DeepEqual(remoteEntry["env_vars"], []any{"TOKEN"}) {
				t.Fatalf("header variables = %#v", remoteEntry)
			}
			if _, ok := remoteEntry["url"]; ok {
				t.Fatal("HTTP bypassed manager")
			}
			local := nestedMap(t, readTOML(t, filepath.Join(root, ".codex", "config.toml")), "mcp_servers", "local")
			if !sliceHasString(local["args"], "--project") || !sliceHasString(local["args"], "demo") {
				t.Fatalf("missing project context: %#v", local)
			}
		})
	}
}

func TestWorktreesInheritToolPolicies(t *testing.T) {
	cfg, options, _, linked := worktreeFixture(t)
	project := cfg.Projects["repo"]
	project.IncludeWorktrees = true
	project.DisabledTools = map[string][]string{"local": {"delete"}}
	cfg.Projects["repo"] = project
	if _, err := Sync(cfg, options); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{".mcp.json", "opencode.json", ".codex/config.toml"} {
		data, err := os.ReadFile(filepath.Join(linked, relative))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "--project") || !strings.Contains(string(data), "repo") {
			t.Fatalf("worktree missing owner policy: %s", data)
		}
	}
}
