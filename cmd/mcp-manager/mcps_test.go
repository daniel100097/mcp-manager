package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/daniel100097/mcp-manager/internal/config"
	"github.com/daniel100097/mcp-manager/internal/wrapper"
)

func TestMCPAddInfersLocalProjectAndPreservesServerArguments(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "config.json")
	writeCentralConfig(t, centralPath, environment.config())
	writeNativeSentinels(t, environment)
	nested := filepath.Join(environment.target, "nested", "folder with spaces")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	mcpTestChdir(t, nested)
	t.Setenv("MCP_TEST_TOKEN", "test-token")
	serverArgs := []string{"script with spaces.js", "--url", "https://server-argument.example", "--project", "two words", "", "--", "--global"}
	args := []string{"files", "--config", centralPath, "--local", "--env", "MESSAGE=has spaces=and equals", "--env", "EMPTY=", "--env-from", "MCP_TEST_TOKEN", "--disabled-agent", "opencode", "--", "node"}
	args = append(args, serverArgs...)
	mcpTestCommand(t, runMCPAdd, args...)

	cfg := loadCentralConfig(t, centralPath)
	mcp := cfg.MCPs["files"]
	if mcp.Type != "stdio" || mcp.Command != "node" || !reflect.DeepEqual(mcp.Args, serverArgs) {
		t.Fatalf("stdio definition does not preserve arguments: %#v", mcp)
	}
	if !reflect.DeepEqual(mcp.Env, map[string]string{"MESSAGE": "has spaces=and equals", "EMPTY": ""}) || !reflect.DeepEqual(mcp.EnvFrom, []string{"MCP_TEST_TOKEN"}) {
		t.Fatalf("environment assignments = %#v, %#v", mcp.Env, mcp.EnvFrom)
	}
	if stringIndex(cfg.Global.MCPs, "files") >= 0 {
		t.Fatal("local add made MCP global")
	}
	assertMCPProjectAssignments(t, cfg, "files", "target")
	if got := cfg.Projects["target"].DisabledAgents["files"]; !reflect.DeepEqual(got, []config.Agent{config.AgentOpenCode}) {
		t.Fatalf("disabled agents = %#v", got)
	}
	assertGeneratedTargetsInSync(t, environment, centralPath, cfg)
}

func TestMCPAddHTTPGloballyWithHeaderReferences(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "config.json")
	writeCentralConfig(t, centralPath, environment.config())
	t.Setenv("MCP_TEST_AUTH", "Bearer test-token")
	mcpTestCommand(t, runMCPAdd, "remote", "--config", centralPath, "--global", "--url", "https://example.com/mcp", "--header", "X-Region=eu=west", "--header-from", "Authorization=MCP_TEST_AUTH")
	cfg := loadCentralConfig(t, centralPath)
	want := config.MCP{Type: "http", URL: "https://example.com/mcp", Headers: map[string]string{"X-Region": "eu=west"}, HeadersFrom: map[string]string{"Authorization": "MCP_TEST_AUTH"}}
	if got := cfg.MCPs["remote"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("HTTP definition = %#v, want %#v", got, want)
	}
	if stringIndex(cfg.Global.MCPs, "remote") < 0 {
		t.Fatal("--global did not activate MCP globally")
	}
	assertMCPProjectAssignments(t, cfg, "remote")
	assertGeneratedTargetsInSync(t, environment, centralPath, cfg)
}

func TestMCPAddGlobalCreatesInitialConfiguration(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "fresh", "config.json")
	mcpTestCommand(t, runMCPAdd, "first", "--global", "--config", centralPath, "--url", "https://example.com/mcp")
	cfg := loadCentralConfig(t, centralPath)
	if len(cfg.Projects) != 0 || !reflect.DeepEqual(cfg.Global.MCPs, []string{"first"}) || len(cfg.MCPs) != 1 {
		t.Fatalf("initial configuration = %#v", cfg)
	}
	if contents := string(readFile(t, filepath.Join(environment.home, ".claude.json"))); !strings.Contains(contents, `"first"`) {
		t.Fatalf("global native configuration lacks MCP: %s", contents)
	}
}

func TestMCPAddSelectsProjectByIDOrPath(t *testing.T) {
	for _, usePath := range []bool{false, true} {
		t.Run(map[bool]string{false: "ID", true: "path"}[usePath], func(t *testing.T) {
			environment := newMoveTestEnvironment(t)
			centralPath := filepath.Join(environment.base, "config.json")
			writeCentralConfig(t, centralPath, environment.config())
			selector := "other"
			if usePath {
				selector = environment.other
			}
			mcpTestCommand(t, runMCPAdd, "selected", "--config", centralPath, "--project", selector, "--", "server")
			assertMCPProjectAssignments(t, loadCentralConfig(t, centralPath), "selected", "other")
		})
	}
}

func TestMCPReplacePreservesScopeAssignmentsAndExclusions(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "config.json")
	before := environment.config()
	writeCentralConfig(t, centralPath, before)
	mcpTestCommand(t, runMCPAdd, "tools", "--replace", "--config", centralPath, "--global", "--url", "https://replacement.example/mcp")
	after := loadCentralConfig(t, centralPath)
	if !reflect.DeepEqual(after.Global, before.Global) || !reflect.DeepEqual(after.Projects, before.Projects) {
		t.Fatalf("replacement changed scope settings:\n before %#v\n after %#v", before, after)
	}
	if got := after.MCPs["tools"]; !reflect.DeepEqual(got, config.MCP{Type: "http", URL: "https://replacement.example/mcp"}) {
		t.Fatalf("replacement did not replace the entire definition: %#v", got)
	}
	if !reflect.DeepEqual(after.MCPs["other-mcp"], before.MCPs["other-mcp"]) {
		t.Fatal("replacement changed another definition")
	}
	assertGeneratedTargetsInSync(t, environment, centralPath, after)
}

func TestMCPRemoveDeletesAllReferencesAndPreservesNativeSettings(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "config.json")
	writeCentralConfig(t, centralPath, environment.config())
	sentinels := writeNativeSentinels(t, environment)
	mcpTestCommand(t, runMCPRemove, "tools", "--config", centralPath)
	cfg := loadCentralConfig(t, centralPath)
	if _, exists := cfg.MCPs["tools"]; exists {
		t.Fatal("removed definition still exists")
	}
	if stringIndex(cfg.Global.MCPs, "tools") >= 0 || len(cfg.Global.DisabledAgents["tools"]) > 0 {
		t.Fatal("global activation or exclusion still exists")
	}
	for id, project := range cfg.Projects {
		if stringIndex(project.MCPs, "tools") >= 0 {
			t.Fatalf("project %s still activates removed MCP", id)
		}
		if _, exists := project.DisabledAgents["tools"]; exists {
			t.Fatalf("project %s still excludes removed MCP", id)
		}
	}
	for path := range sentinels {
		if contents := string(readFile(t, path)); !strings.Contains(contents, "keep") {
			t.Fatalf("unrelated native settings lost in %s: %s", path, contents)
		}
	}
	assertGeneratedTargetsInSync(t, environment, centralPath, cfg)
}

func TestMCPMutationsDryRunLeaveAllFilesUnchanged(t *testing.T) {
	tests := []struct {
		name    string
		command func([]string, io.Writer, io.Writer) int
		args    []string
	}{
		{"add", runMCPAdd, []string{"new", "--global", "--url", "https://example.com/mcp"}},
		{"replace", runMCPAdd, []string{"tools", "--replace", "--global", "--url", "https://example.com/mcp"}},
		{"remove", runMCPRemove, []string{"tools"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := newMoveTestEnvironment(t)
			centralPath := filepath.Join(environment.base, "config.json")
			writeCentralConfig(t, centralPath, environment.config())
			sentinels := writeNativeSentinels(t, environment)
			sentinels[centralPath] = readFile(t, centralPath)
			args := append([]string{"--config", centralPath, "--dry-run"}, test.args...)
			out := mcpTestCommand(t, test.command, args...)
			if !strings.Contains(out, "would ") {
				t.Fatalf("dry run did not describe changes: %s", out)
			}
			assertSentinelsUnchanged(t, sentinels)
		})
	}
}

func TestMCPAddRejectsInvalidInputsBeforeWriting(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantError   string
		corruptFile bool
	}{
		{name: "existing name", args: []string{"tools", "--global", "--", "replacement"}, wantError: "--replace"},
		{name: "invalid name", args: []string{"bad/name", "--global", "--", "server"}, wantError: "MCP name"},
		{name: "missing command", args: []string{"new", "--global", "--"}, wantError: "server command"},
		{name: "missing separator", args: []string{"new", "--global", "server", "arg"}, wantError: "after --"},
		{name: "conflicting scopes", args: []string{"new", "--global", "--local", "--", "server"}, wantError: "cannot be combined"},
		{name: "global with project", args: []string{"new", "--global", "--project", "target", "--", "server"}, wantError: "cannot be combined"},
		{name: "unregistered project", args: []string{"new", "--project", "missing-project", "--", "server"}, wantError: "project"},
		{name: "invalid URL", args: []string{"new", "--global", "--url", "not-a-url"}, wantError: "absolute http"},
		{name: "both transports", args: []string{"new", "--global", "--url", "https://example.com", "--", "server"}, wantError: "choose --url"},
		{name: "HTTP environment", args: []string{"new", "--global", "--url", "https://example.com", "--env", "TOKEN=value"}, wantError: "http transport"},
		{name: "stdio headers", args: []string{"new", "--global", "--header", "Authorization=token", "--", "server"}, wantError: "stdio transport"},
		{name: "invalid environment assignment", args: []string{"new", "--global", "--env", "TOKEN", "--", "server"}, wantError: "NAME=VALUE"},
		{name: "duplicate environment assignment", args: []string{"new", "--global", "--env", "TOKEN=a", "--env", "TOKEN=b", "--", "server"}, wantError: "repeats"},
		{name: "invalid environment name", args: []string{"new", "--global", "--env", "BAD-NAME=value", "--", "server"}, wantError: "environment variable name"},
		{name: "invalid environment reference", args: []string{"new", "--global", "--env-from", "BAD-NAME", "--", "server"}, wantError: "invalid envFrom variable"},
		{name: "invalid header reference", args: []string{"new", "--global", "--url", "https://example.com", "--header-from", "Authorization=BAD-NAME"}, wantError: "invalid environment variable"},
		{name: "invalid exclusion", args: []string{"new", "--global", "--disabled-agent", "unknown", "--", "server"}, wantError: "unknown disabled agent"},
		{name: "invalid native config", args: []string{"new", "--global", "--", "server"}, wantError: "JSON", corruptFile: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := newMoveTestEnvironment(t)
			centralPath := filepath.Join(environment.base, "config.json")
			writeCentralConfig(t, centralPath, environment.config())
			sentinels := writeNativeSentinels(t, environment)
			if test.corruptFile {
				path := filepath.Join(environment.target, ".mcp.json")
				sentinels[path] = []byte("invalid JSON")
				writeTestFile(t, path, sentinels[path])
			}
			sentinels[centralPath] = readFile(t, centralPath)
			var stdout, stderr bytes.Buffer
			args := append([]string{"--config", centralPath}, test.args...)
			if code := runMCPAdd(args, &stdout, &stderr); code == 0 {
				t.Fatalf("invalid add succeeded: %s", &stdout)
			}
			if !strings.Contains(strings.ToLower(stderr.String()), strings.ToLower(test.wantError)) {
				t.Fatalf("error %q lacks %q", stderr.String(), test.wantError)
			}
			assertSentinelsUnchanged(t, sentinels)
		})
	}
}

func TestMCPAddLocalRequiresRegistrationWithoutCreatingFiles(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "missing", "config.json")
	mcpTestChdir(t, environment.target)
	var stdout, stderr bytes.Buffer
	if code := runMCPAdd([]string{"new", "--config", centralPath, "--", "server"}, &stdout, &stderr); code == 0 {
		t.Fatal("unregistered local add succeeded")
	}
	if !strings.Contains(stderr.String(), "project add") {
		t.Fatalf("registration error lacks next step: %s", &stderr)
	}
	if _, err := os.Stat(centralPath); !os.IsNotExist(err) {
		t.Fatalf("failed add created config: %v", err)
	}
}

func TestMCPMutationsPreserveCentralWithLocalOverlay(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "config.json")
	writeCentralConfig(t, centralPath, environment.config())
	localPath := config.LocalPathFor(centralPath)
	writeTestFile(t, localPath, []byte(`{"options":{"inlineSecrets":true},"projects":{"target":{"includeWorktrees":false}}}`))
	centralBefore := readFile(t, centralPath)
	mcpTestCommand(t, runMCPAdd, "overlay", "--config", centralPath, "--project", "target", "--", "overlay-server")
	mcpTestCommand(t, runMCPRemove, "tools", "--config", centralPath)
	if !bytes.Equal(readFile(t, centralPath), centralBefore) {
		t.Fatal("MCP mutation rewrote the central config with an overlay present")
	}
	cfg, err := (config.Source{Path: centralPath, LocalPath: localPath}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := cfg.MCPs["tools"]; exists {
		t.Fatal("overlay did not remove definition")
	}
	if cfg.MCPs["overlay"].Command != "overlay-server" {
		t.Fatal("overlay lost added definition")
	}
	assertMCPProjectAssignments(t, cfg, "overlay", "target")
	local := string(readFile(t, localPath))
	if !strings.Contains(local, `"includeWorktrees": false`) || !strings.Contains(local, `"inlineSecrets": true`) {
		t.Fatalf("overlay lost pinned defaults: %s", local)
	}
}

func TestMCPListAndShowReadOnlyAndRedactLiteralSecrets(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "config.json")
	cfg := environment.config()
	mcp := cfg.MCPs["tools"]
	mcp.Env["LITERAL_SECRET"] = "do-not-print-env-value"
	mcp.EnvFrom = []string{"UNSET_MCP_TEST_TOKEN"}
	cfg.MCPs["tools"] = mcp
	mcp = cfg.MCPs["other-mcp"]
	mcp.Headers["Authorization"] = "do-not-print-header-value"
	mcp.HeadersFrom = map[string]string{"X-Token": "UNSET_MCP_TEST_HEADER"}
	cfg.MCPs["other-mcp"] = mcp
	cfg.MCPs["unused"] = config.MCP{Type: "stdio", Command: "unused-server"}
	writeCentralConfig(t, centralPath, cfg)
	sentinels := writeNativeSentinels(t, environment)
	sentinels[centralPath] = readFile(t, centralPath)
	// Read commands remain useful even when another registered path is absent.
	if err := os.RemoveAll(environment.other); err != nil {
		t.Fatal(err)
	}
	for path := range sentinels {
		if strings.HasPrefix(path, environment.other+string(filepath.Separator)) {
			delete(sentinels, path)
		}
	}
	previousLaunch := execLaunch
	execLaunch = func(wrapper.Launch) (int, error) {
		t.Fatal("read command attempted to launch an MCP")
		return 1, nil
	}
	t.Cleanup(func() { execLaunch = previousLaunch })
	list := mcpTestCommand(t, runMCPList, "--config", centralPath)
	for _, fragment := range []string{"NAME", "TYPE", "SCOPES", "tools", "stdio", "http", "global", "project:other", "unused", "disabled"} {
		if !strings.Contains(list, fragment) {
			t.Fatalf("list missing %q: %s", fragment, list)
		}
	}
	stdio := mcpTestCommand(t, runMCPShow, "tools", "--config", centralPath)
	http := mcpTestCommand(t, runMCPShow, "other-mcp", "--config", centralPath)
	for _, secret := range []string{"do-not-print-env-value", "do-not-print-header-value", "test-secret"} {
		if strings.Contains(list+stdio+http, secret) {
			t.Fatalf("read commands disclosed literal secret %q", secret)
		}
	}
	if !strings.Contains(stdio, "[REDACTED]") || !strings.Contains(stdio, "UNSET_MCP_TEST_TOKEN") || !strings.Contains(http, "UNSET_MCP_TEST_HEADER") || !strings.Contains(http, "[REDACTED]") {
		t.Fatalf("show output lost redaction markers or variable references:\n%s\n%s", stdio, http)
	}
	assertSentinelsUnchanged(t, sentinels)
}

func TestMCPRemoveUnknownDoesNotWrite(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "config.json")
	writeCentralConfig(t, centralPath, environment.config())
	sentinels := writeNativeSentinels(t, environment)
	sentinels[centralPath] = readFile(t, centralPath)
	var stdout, stderr bytes.Buffer
	if code := runMCPRemove([]string{"missing", "--config", centralPath}, &stdout, &stderr); code == 0 {
		t.Fatal("removing an unknown MCP succeeded")
	}
	assertSentinelsUnchanged(t, sentinels)
}

func TestMCPListFiltersWithoutWriting(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "config.json")
	cfg := environment.config()
	for _, name := range []string{"target-only", "other-only", "disabled"} {
		cfg.MCPs[name] = config.MCP{Type: "stdio", Command: name + "-server"}
	}
	target := cfg.Projects["target"]
	target.MCPs = append(target.MCPs, "target-only")
	cfg.Projects["target"] = target
	other := cfg.Projects["other"]
	other.MCPs = append(other.MCPs, "other-only")
	cfg.Projects["other"] = other
	writeCentralConfig(t, centralPath, cfg)
	sentinels := writeNativeSentinels(t, environment)
	sentinels[centralPath] = readFile(t, centralPath)
	mcpTestChdir(t, environment.target)
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{name: "default", want: []string{"disabled", "other-mcp", "other-only", "target-only", "tools"}},
		{name: "all", args: []string{"--all"}, want: []string{"disabled", "other-mcp", "other-only", "target-only", "tools"}},
		{name: "global", args: []string{"--global"}, want: []string{"other-mcp", "tools"}},
		{name: "current project", args: []string{"--local"}, want: []string{"other-mcp", "target-only", "tools"}},
		{name: "project ID", args: []string{"--project", "other"}, want: []string{"other-mcp", "other-only", "tools"}},
		{name: "project path", args: []string{"--project", environment.other}, want: []string{"other-mcp", "other-only", "tools"}},
		{name: "explicit local project", args: []string{"--local", "--project", "other"}, want: []string{"other-mcp", "other-only", "tools"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"--config", centralPath}, test.args...)
			output := mcpTestCommand(t, runMCPList, args...)
			var got []string
			for _, line := range strings.Split(strings.TrimSpace(output), "\n")[1:] {
				fields := strings.Fields(line)
				if len(fields) > 0 {
					got = append(got, fields[0])
				}
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("filtered MCPs = %v, want %v:\n%s", got, test.want, output)
			}
			assertSentinelsUnchanged(t, sentinels)
		})
	}
}

func TestMCPListRejectsConflictingFilters(t *testing.T) {
	newMoveTestEnvironment(t)
	for _, args := range [][]string{
		{"--all", "--global"},
		{"--all", "--local"},
		{"--all", "--project", "target"},
		{"--global", "--local"},
		{"--global", "--project", "target"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := runMCPList(args, &stdout, &stderr); code != 2 {
				t.Fatalf("conflicting filters returned %d, want 2: %s", code, &stderr)
			}
			if !strings.Contains(stderr.String(), "choose one list filter") {
				t.Fatalf("conflicting filters error is unclear: %s", &stderr)
			}
		})
	}
}

func TestMCPListLocalRequiresRegisteredProject(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "config.json")
	writeCentralConfig(t, centralPath, environment.config())
	mcpTestChdir(t, environment.home)
	before := readFile(t, centralPath)
	var stdout, stderr bytes.Buffer
	if code := runMCPList([]string{"--local", "--config", centralPath}, &stdout, &stderr); code == 0 {
		t.Fatal("local list succeeded outside a registered project")
	}
	if !strings.Contains(stderr.String(), "project add") {
		t.Fatalf("missing-project error lacks next step: %s", &stderr)
	}
	if !bytes.Equal(before, readFile(t, centralPath)) {
		t.Fatal("local list changed configuration")
	}
}

func mcpTestCommand(t *testing.T, command func([]string, io.Writer, io.Writer) int, args ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := command(args, &stdout, &stderr); code != 0 {
		t.Fatalf("command %q failed (%d):\nstdout: %s\nstderr: %s", args, code, &stdout, &stderr)
	}
	return stdout.String()
}

func mcpTestChdir(t *testing.T, path string) {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Error(err)
		}
	})
}
