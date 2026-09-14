package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/daniel100097/mcp-manager/internal/config"
	"github.com/daniel100097/mcp-manager/internal/wrapper"
)

func TestCLIToolWorkflowAcrossScopes(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	central := filepath.Join(environment.base, "config.json")
	t.Setenv("MCP_MANAGER_CONFIG", central)
	t.Setenv("MCP_MANAGER_CONFIG_LOCAL", "")
	t.Chdir(environment.target)
	cliOK(t, "project", "add")
	cliOK(t, "project", "add", environment.other)
	cliOK(t, "add", "api", "--url", "https://example.test/mcp", "--disabled-tool", "delete")
	cliOK(t, "enable", "api", "--project", environment.other)
	cliOK(t, "disable", "api", "--tool", "write", "--tool", "admin")
	cfg := loadCentralConfig(t, central)
	want := []string{"admin", "delete", "write"}
	if !reflect.DeepEqual(cfg.Projects["target"].DisabledTools["api"], want) || len(cfg.Projects["other"].DisabledTools["api"]) != 0 {
		t.Fatalf("wrong scoped policy: %#v", cfg.Projects)
	}
	before := readFile(t, central)
	cliOK(t, "enable", "api", "--tool", "delete", "--dry-run")
	if !bytes.Equal(before, readFile(t, central)) {
		t.Fatal("dry-run changed configuration")
	}
	cliOK(t, "move", "api", "--global")
	cfg = loadCentralConfig(t, central)
	if !reflect.DeepEqual(cfg.Global.DisabledTools["api"], want) || len(cfg.Projects["target"].DisabledTools) != 0 {
		t.Fatal("move lost exclusions")
	}
	cliOK(t, "enable", "api", "--global", "--tool", "delete")
	if output := cliOK(t, "show", "api"); !strings.Contains(output, "disabled tools: admin, write") {
		t.Fatalf("show omits policy: %s", output)
	}
	cliOK(t, "move", "api", "--local")
	cfg = loadCentralConfig(t, central)
	if !reflect.DeepEqual(cfg.Projects["target"].DisabledTools["api"], []string{"admin", "write"}) || len(cfg.Global.DisabledTools) != 0 {
		t.Fatal("move back lost exclusions")
	}
	cliOK(t, "add", "api", "--replace", "--url", "https://example.test/new")
	if len(loadCentralConfig(t, central).Projects["target"].DisabledTools["api"]) != 2 {
		t.Fatal("replace lost exclusions")
	}
	cliOK(t, "disable", "api")
	if len(loadCentralConfig(t, central).Projects["target"].DisabledTools) != 0 {
		t.Fatal("disable left dangling exclusions")
	}
	cliOK(t, "enable", "api")
	cliOK(t, "disable", "api", "--tool", "delete")
	cliOK(t, "remove", "api")
	cfg = loadCentralConfig(t, central)
	if len(cfg.Projects["target"].DisabledTools) != 0 {
		t.Fatal("remove left exclusions")
	}
}

func TestToolCLIRejectsInvalidScopeAndOptions(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	central := filepath.Join(environment.base, "config.json")
	writeCentralConfig(t, central, environment.config())
	t.Setenv("MCP_MANAGER_CONFIG", central)
	t.Chdir(environment.target)
	for _, args := range [][]string{
		{"disable", "tools", "--tool", "delete"}, // only a global assignment exists
		{"disable", "tools", "--global", "--tool", ""},
		{"disable", "tools", "--global", "--tool", "delete", "--agent", "codex"},
		{"move", "tools", "--tool", "delete"},
	} {
		before := readFile(t, central)
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code == 0 {
			t.Fatalf("accepted invalid args: %v", args)
		}
		if !bytes.Equal(before, readFile(t, central)) {
			t.Fatalf("invalid args changed configuration: %v", args)
		}
	}
}

func TestStdioCommandRoutesBothTransportsAndSelectsPolicy(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	central := filepath.Join(environment.base, "config.json")
	cfg := environment.config()
	setProjectMCPs(cfg, "target", "tools", "other-mcp")
	cfg.Global.DisabledTools = map[string][]string{"tools": {"global_tool"}}
	project := cfg.Projects["target"]
	project.DisabledTools = map[string][]string{"tools": {"project_tool"}, "other-mcp": {"remote_tool"}}
	cfg.Projects["target"] = project
	definition := cfg.MCPs["tools"]
	definition.Command = os.Args[0]
	cfg.MCPs["tools"] = definition
	writeCentralConfig(t, central, cfg)
	oldStdio, oldHTTP, oldExec := serveStdio, serveHTTP, execLaunch
	t.Cleanup(func() { serveStdio, serveHTTP, execLaunch = oldStdio, oldHTTP, oldExec })
	var got []string
	serveStdio = func(_ context.Context, _ wrapper.Launch, disabled []string, _ io.ReadCloser, _ io.Writer, _ io.Writer) (int, error) {
		got = disabled
		return 0, nil
	}
	serveHTTP = func(_ context.Context, _ config.MCP, _ []string, disabled []string, _ io.ReadCloser, _ io.Writer) (int, error) {
		got = disabled
		return 0, nil
	}
	execLaunch = func(wrapper.Launch) (int, error) { t.Fatal("filtered server bypassed proxy"); return 1, nil }
	for _, test := range []struct {
		args []string
		want []string
	}{
		{[]string{"tools"}, []string{"global_tool"}},
		{[]string{"--project", "target", "tools"}, []string{"project_tool"}},
		{[]string{"--project", "target", "other-mcp"}, []string{"remote_tool"}},
		{[]string{"other-mcp"}, nil},
	} {
		cliOK(t, append([]string{"stdio", "--config", central}, test.args...)...)
		if !reflect.DeepEqual(got, test.want) {
			t.Fatalf("%v policy = %v; want %v", test.args, got, test.want)
		}
	}
}

func TestToolMovePreservesExistingDestination(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	central := filepath.Join(environment.base, "config.json")
	cfg := environment.config()
	setProjectMCPs(cfg, "target", "tools")
	cfg.Global.DisabledTools = map[string][]string{"tools": {"global"}}
	project := cfg.Projects["target"]
	project.DisabledTools = map[string][]string{"tools": {"local"}}
	cfg.Projects["target"] = project
	writeCentralConfig(t, central, cfg)
	cliOK(t, "move", "tools", "--local", "--project", "target", "--config", central)
	got := loadCentralConfig(t, central).Projects["target"].DisabledTools["tools"]
	if !reflect.DeepEqual(got, []string{"local"}) {
		t.Fatalf("move replaced destination policy: %v", got)
	}
}
