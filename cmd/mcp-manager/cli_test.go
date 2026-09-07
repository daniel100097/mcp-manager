package main

import (
	"bytes"
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/daniel100097/mcp-manager/internal/config"
)

func cliOK(t *testing.T, args ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("mcp-manager %v exited %d\n%s\n%s", args, code, &stdout, &stderr)
	}
	return stdout.String()
}

func TestCLIProjectWorkflowFromCurrentFolder(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	central := filepath.Join(environment.base, "config.json")
	t.Setenv("MCP_MANAGER_CONFIG", central)
	t.Setenv("MCP_MANAGER_CONFIG_LOCAL", "")
	t.Chdir(environment.target)
	cliOK(t, "project", "add")
	cliOK(t, "project", "add", environment.other)
	cliOK(t, "add", "docs", "--url", "https://example.test/mcp")
	cliOK(t, "add", "stdio", "--", "server", "--project", "literal-server-arg")
	cliOK(t, "enable", "docs", "--project", environment.other)
	cliOK(t, "disable", "docs", "--agent", "claude")
	cliOK(t, "move", "docs", "--global")
	cfg := loadCentralConfig(t, central)
	if stringIndex(cfg.Global.MCPs, "docs") < 0 || stringIndex(cfg.Projects["target"].MCPs, "docs") >= 0 {
		t.Fatalf("global move did not transfer assignment: %#v", cfg)
	}
	if !reflect.DeepEqual(cfg.Global.DisabledAgents["docs"], []config.Agent{config.AgentClaude}) || stringIndex(cfg.Projects["other"].MCPs, "docs") < 0 {
		t.Fatal("global move lost agent exclusions or another project's assignment")
	}
	cliOK(t, "move", "docs", "--local")
	cliOK(t, "enable", "docs", "--agent", "claude")
	cliOK(t, "disable", "docs")
	cliOK(t, "enable", "docs")
	cliOK(t, "enable", "docs", "--global")
	cliOK(t, "disable", "docs", "--global")
	cliOK(t, "add", "docs", "--replace", "--url", "https://example.test/new-mcp")
	cfg = loadCentralConfig(t, central)
	if cfg.MCPs["docs"].URL != "https://example.test/new-mcp" || len(cfg.Projects["target"].DisabledAgents["docs"]) != 0 {
		t.Fatal("replace or agent re-enable failed")
	}
	before := readFile(t, central)
	cliOK(t, "remove", "docs", "--dry-run")
	if !bytes.Equal(before, readFile(t, central)) {
		t.Fatal("dry run changed central config")
	}
	cliOK(t, "remove", "docs")
	cfg = loadCentralConfig(t, central)
	if _, exists := cfg.MCPs["docs"]; exists {
		t.Fatal("remove kept definition")
	}
	for _, project := range cfg.Projects {
		if stringIndex(project.MCPs, "docs") >= 0 {
			t.Fatal("remove kept project activation")
		}
	}
	cliOK(t, "project", "remove")
	if _, exists := loadCentralConfig(t, central).Projects["target"]; exists {
		t.Fatal("project remove did not use cwd")
	}
}

func TestCLIWorktreeAndNestedCurrentFolder(t *testing.T) {
	environment, central, worktree := newWorktreesTestEnvironment(t)
	t.Setenv("MCP_MANAGER_CONFIG", central)
	t.Setenv("MCP_MANAGER_CONFIG_LOCAL", "")
	t.Chdir(environment.target)
	cliOK(t, "worktrees", "enable")
	nested := filepath.Join(worktree, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(nested)
	cliOK(t, "add", "docs", "--url", "https://example.test/mcp")
	cliOK(t, "disable", "docs")
	cliOK(t, "enable", "docs")
	cliOK(t, "move", "docs", "--global")
	cliOK(t, "move", "docs", "--local")
	for _, root := range []string{environment.target, worktree} {
		if data := readFile(t, filepath.Join(root, ".mcp.json")); !bytes.Contains(data, []byte(`"docs"`)) {
			t.Fatalf("missing docs in %s: %s", root, data)
		}
	}
	cliOK(t, "worktrees", "disable")
	if loadCentralConfig(t, central).Projects["target"].IncludeWorktrees {
		t.Fatal("worktree disable did not select inherited owner")
	}
}

func TestCLIGlobalAndAgentChanges(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	central := filepath.Join(environment.base, "config.json")
	cfg := environment.config()
	setToolsGlobal(cfg, false)
	writeCentralConfig(t, central, cfg)
	t.Setenv("MCP_MANAGER_CONFIG", central)
	t.Setenv("MCP_MANAGER_CONFIG_LOCAL", "")
	t.Chdir(environment.target)
	cliOK(t, "enable", "tools", "--agent", "codex")
	cfg = loadCentralConfig(t, central)
	if !reflect.DeepEqual(cfg.Projects["target"].DisabledAgents["tools"], []config.Agent{config.AgentClaude, config.AgentOpenCode}) {
		t.Fatalf("first agent-specific enable activated other agents: %#v", cfg.Projects["target"])
	}
	cliOK(t, "enable", "tools", "--agent", "claude")
	cliOK(t, "disable", "tools", "--agent", "codex")
	cliOK(t, "move", "tools", "--global")
	cliOK(t, "enable", "tools", "--global", "--agent", "codex")
	cliOK(t, "disable", "tools", "--global", "--agent", "claude")
	cfg = loadCentralConfig(t, central)
	exclusions := cfg.Global.DisabledAgents["tools"]
	slices.Sort(exclusions)
	if !reflect.DeepEqual(exclusions, []config.Agent{config.AgentClaude, config.AgentOpenCode}) {
		t.Fatalf("global agent exclusions = %#v", cfg.Global.DisabledAgents["tools"])
	}
}

func TestCLIMovePreservesExistingDestinationPolicy(t *testing.T) {
	for _, direction := range []string{"--global", "--local"} {
		t.Run(direction, func(t *testing.T) {
			environment := newMoveTestEnvironment(t)
			central := filepath.Join(environment.base, "config.json")
			cfg := environment.config()
			setProjectMCPs(cfg, "target", "tools")
			if direction == "--global" {
				delete(cfg.Global.DisabledAgents, "tools")
				cfg.Projects["target"].DisabledAgents["tools"] = []config.Agent{config.AgentClaude}
			}
			writeCentralConfig(t, central, cfg)
			t.Chdir(environment.target)
			cliOK(t, "move", "tools", direction, "--config", central)
			cfg = loadCentralConfig(t, central)
			exclusions := cfg.Global.DisabledAgents["tools"]
			if direction == "--local" {
				exclusions = cfg.Projects["target"].DisabledAgents["tools"]
			}
			if len(exclusions) != 0 {
				t.Fatalf("move changed existing all-agents policy: %#v", exclusions)
			}
		})
	}
}

func TestCLILocalAssignmentsCanBeEditedWhileGlobal(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	central := filepath.Join(environment.base, "config.json")
	cfg := environment.config()
	writeCentralConfig(t, central, cfg)
	t.Setenv("MCP_MANAGER_CONFIG", central)
	t.Setenv("MCP_MANAGER_CONFIG_LOCAL", "")
	t.Chdir(environment.target)
	cliOK(t, "enable", "tools")
	cliOK(t, "disable", "tools", "--agent", "claude")
	cliOK(t, "enable", "tools", "--agent", "claude")
	output := cliOK(t, "disable", "tools")
	if !strings.Contains(output, "Global assignment remains active") {
		t.Fatalf("missing explanation that global scope still applies: %s", output)
	}
	after := loadCentralConfig(t, central)
	if !reflect.DeepEqual(cfg.Global, after.Global) || len(after.Projects["target"].MCPs) != 0 {
		t.Fatalf("local change affected global scope or failed to remove assignment: %#v", after)
	}
}

func TestCLIImportCurrentFolderAndGlobal(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	central := filepath.Join(environment.base, "config.json")
	t.Chdir(environment.target)
	cliOK(t, "--config", central, "project", "add")
	writeTextFile(t, filepath.Join(environment.target, ".mcp.json"), `{"mcpServers":{"local-docs":{"type":"http","url":"https://example.test/local"}}}`)
	cliOK(t, "--config", central, "import", "--from", "claude", "--local")
	writeTextFile(t, filepath.Join(environment.home, ".claude.json"), `{"mcpServers":{"global-docs":{"type":"http","url":"https://example.test/global"}}}`)
	cliOK(t, "import", "--from", "claude", "--global", "--config", central)
	cfg := loadCentralConfig(t, central)
	if !reflect.DeepEqual(cfg.Projects["target"].MCPs, []string{"local-docs"}) || !reflect.DeepEqual(cfg.Global.MCPs, []string{"global-docs"}) {
		t.Fatalf("import scopes are wrong: %#v", cfg)
	}
}

func TestCLIInitializesAlreadyRegisteredOverlayProject(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	central := filepath.Join(environment.base, "config.json")
	t.Chdir(environment.target)
	writeTextFile(t, config.LocalPathFor(central), `{"projects":{"target":{"path":`+jsonQuote(t, environment.target)+`,"mcps":[]}}}`)
	cliOK(t, "project", "add", "--config", central, "--dry-run")
	if _, err := os.Stat(central); !os.IsNotExist(err) {
		t.Fatal("dry run initialized config")
	}
	cliOK(t, "project", "add", "--config", central)
	cliOK(t, "add", "docs", "--url", "https://example.test/mcp", "--config", central)
	effective, err := (config.Source{Path: central, LocalPath: config.LocalPathFor(central)}).Load()
	if err != nil || len(effective.Projects["target"].MCPs) != 1 {
		t.Fatalf("initialized config not readable or project absent: %#v, %v", effective, err)
	}
}

func TestCLIImportInitializesExistingLocalOverlay(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	central := filepath.Join(environment.base, "config.json")
	local := config.LocalPathFor(central)
	writeTextFile(t, local, `{}`)
	writeTextFile(t, filepath.Join(environment.home, ".claude.json"), `{"mcpServers":{"docs":{"type":"http","url":"https://example.test/mcp"}}}`)
	cliOK(t, "import", "--from", "claude", "--global", "--config", central)
	base := loadCentralConfig(t, central)
	if len(base.MCPs) != 0 {
		t.Fatal("import saved definitions to central instead of existing overlay")
	}
	effective, err := (config.Source{Path: central, LocalPath: local}).Load()
	if err != nil || len(effective.MCPs) != 1 {
		t.Fatalf("initialized overlay import: %#v, %v", effective, err)
	}
}

func TestCLIInterspersedOptionsAndLiteralArguments(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dry := flags.Bool("dry-run", false, "")
	path := flags.String("config", "", "")
	err := parseFlags(flags, []string{"name", "--config", "a path", "--dry-run", "--", "--literal", ""})
	if err != nil || !*dry || *path != "a path" || !reflect.DeepEqual(flags.Args(), []string{"name", "--literal", ""}) {
		t.Fatalf("parse result args=%#v dry=%v path=%q err=%v", flags.Args(), *dry, *path, err)
	}
	for _, args := range [][]string{{"name", "--config"}, {"name", "--unknown"}} {
		if err := parseFlags(flags, args); err == nil {
			t.Fatalf("accepted invalid options: %v", args)
		}
	}
}

func TestCLIInvalidScopeChoicesWriteNothing(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	central := filepath.Join(environment.base, "config.json")
	writeCentralConfig(t, central, environment.config())
	t.Setenv("MCP_MANAGER_CONFIG", central)
	t.Setenv("MCP_MANAGER_CONFIG_LOCAL", "")
	t.Chdir(environment.target)
	before := readFile(t, central)
	sentinels := writeNativeSentinels(t, environment)
	for _, args := range [][]string{
		{"move", "tools", "--global", "--local"},
		{"enable", "tools", "--global", "--project", "target"},
		{"enable", "tools", "target", "--project", "target"},
		{"disable", "tools", "--agent", "unsupported"},
		{"move", "tools", "--agent", "claude"},
		{"worktrees", "enable", "target", "--project", "target"},
		{"import", "--from", "claude", "--global", "--local"},
	} {
		var stderr bytes.Buffer
		if code := run(args, io.Discard, &stderr); code != 2 {
			t.Fatalf("invalid args %v returned %d: %s", args, code, &stderr)
		}
	}
	writeTextFile(t, filepath.Join(environment.target, ".mcp.json"), "invalid JSON")
	var stderr bytes.Buffer
	if code := run([]string{"move", "tools"}, io.Discard, &stderr); code != 1 || !strings.Contains(stderr.String(), ".mcp.json") {
		t.Fatalf("invalid output was not caught in preflight: %d %s", code, &stderr)
	}
	if !bytes.Equal(before, readFile(t, central)) {
		t.Fatal("failed change wrote central config")
	}
	delete(sentinels, filepath.Join(environment.target, ".mcp.json"))
	assertSentinelsUnchanged(t, sentinels)
}
