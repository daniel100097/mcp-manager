package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/daniel100097/mcp-manager/internal/config"
	"github.com/daniel100097/mcp-manager/internal/syncer"
	"github.com/daniel100097/mcp-manager/internal/wrapper"
)

func TestCommandHelpUsesDoubleDashLongOptions(t *testing.T) {
	tests := map[string][]string{
		"sync":    {"--config", "--dry-run", "--inline-secrets"},
		"import":  {"--config", "--dry-run", "--from", "--project"},
		"move":    {"--config", "--dry-run"},
		"enable":  {"--config", "--dry-run"},
		"disable": {"--config", "--dry-run"},
		"stdio":   {"--config"},
	}
	for command, options := range tests {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run([]string{command, "--help"}, &stdout, &stderr); code != 0 {
				t.Fatalf("run(%s --help) code = %d, want 0", command, code)
			}
			help := stderr.String()
			for _, option := range options {
				if !strings.Contains(help, "  "+option) {
					t.Fatalf("help does not contain %q:\n%s", option, help)
				}
				if strings.Contains(help, "  -"+strings.TrimPrefix(option, "--")) {
					t.Fatalf("help contains single-dash form for %q:\n%s", option, help)
				}
			}
		})
	}
}

func TestMoveCommandMovesGlobalMCPAndSynchronizesTargets(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "central", "config.json")
	cfg := environment.config()
	toolsBefore := cfg.MCPs["tools"]
	otherBefore := cfg.MCPs["other-mcp"]
	writeCentralConfig(t, centralPath, cfg)
	writeNativeSentinels(t, environment)

	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"move", "--config", centralPath, "tools", "target"},
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("run(move) code = %d, want 0\nstdout: %s\nstderr: %s", code, &stdout, &stderr)
	}
	got := loadCentralConfig(t, centralPath)
	tools := got.MCPs["tools"]
	if stringIndex(got.Global.MCPs, "tools") >= 0 {
		t.Fatalf("global MCPs still contain tools: %#v", got.Global.MCPs)
	}
	assertMCPProjectAssignments(t, got, "tools", "other", "target")
	if !reflect.DeepEqual(tools, toolsBefore) {
		t.Fatalf("move changed the MCP definition:\n got  %#v\n want %#v", tools, toolsBefore)
	}
	if _, exists := got.Global.DisabledAgents["tools"]; exists {
		t.Fatalf("global exclusions still contain tools: %#v", got.Global.DisabledAgents)
	}
	if agents := got.Projects["target"].DisabledAgents["tools"]; !reflect.DeepEqual(agents, []config.Agent{config.AgentOpenCode}) {
		t.Fatalf("moved project exclusions = %#v, want [opencode]", agents)
	}
	if got.Schema != cfg.Schema || got.Options != cfg.Options ||
		got.Projects["target"].Path != cfg.Projects["target"].Path ||
		got.Projects["other"].Path != cfg.Projects["other"].Path {
		t.Fatalf("move changed central settings or project paths:\n got  %#v\n want %#v", got, cfg)
	}
	if other := got.MCPs["other-mcp"]; !reflect.DeepEqual(other, otherBefore) {
		t.Fatalf("move changed another MCP:\n got  %#v\n want %#v", other, otherBefore)
	}
	assertGeneratedTargetsInSync(t, environment, centralPath, got)
}

func TestMoveCommandDryRunDoesNotWrite(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "config.json")
	writeCentralConfig(t, centralPath, environment.config())
	before := readFile(t, centralPath)
	sentinels := writeNativeSentinels(t, environment)

	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"move", "--config", centralPath, "--dry-run", "tools", "target"},
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("run(move --dry-run) code = %d, want 0\nstdout: %s\nstderr: %s", code, &stdout, &stderr)
	}
	if after := readFile(t, centralPath); !bytes.Equal(after, before) {
		t.Fatalf("dry run changed central config:\n before %s\n after  %s", before, after)
	}
	assertSentinelsUnchanged(t, sentinels)
}

func TestMoveCommandIsIdempotentAtDestination(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "config.json")
	writeCentralConfig(t, centralPath, environment.config())

	var firstOut, firstErr bytes.Buffer
	if code := run(
		[]string{"move", "--config", centralPath, "tools", "target"},
		&firstOut,
		&firstErr,
	); code != 0 {
		t.Fatalf("first move code = %d, want 0\nstdout: %s\nstderr: %s", code, &firstOut, &firstErr)
	}
	afterFirst := readFile(t, centralPath)
	writeNativeSentinels(t, environment)

	var secondOut, secondErr bytes.Buffer
	if code := run(
		[]string{"move", "--config", centralPath, "tools", "target"},
		&secondOut,
		&secondErr,
	); code != 0 {
		t.Fatalf("second move code = %d, want idempotent success\nstdout: %s\nstderr: %s", code, &secondOut, &secondErr)
	}
	if afterSecond := readFile(t, centralPath); !bytes.Equal(afterSecond, afterFirst) {
		t.Fatalf("idempotent move rewrote central config:\n first  %s\n second %s", afterFirst, afterSecond)
	}
	got := loadCentralConfig(t, centralPath)
	if stringIndex(got.Global.MCPs, "tools") >= 0 {
		t.Fatal("idempotent move made MCP global again")
	}
	assertMCPProjectAssignments(t, got, "tools", "other", "target")
	assertGeneratedTargetsInSync(t, environment, centralPath, got)
}

func TestMoveCommandDoesNotDuplicateExistingDestination(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "config.json")
	cfg := environment.config()
	project := cfg.Projects["target"]
	project.MCPs = append(project.MCPs, "tools")
	project.DisabledAgents["tools"] = []config.Agent{config.AgentClaude}
	cfg.Projects["target"] = project
	writeCentralConfig(t, centralPath, cfg)

	var stdout, stderr bytes.Buffer
	if code := run(
		[]string{"move", "--config", centralPath, "tools", "target"},
		&stdout,
		&stderr,
	); code != 0 {
		t.Fatalf("run(move) code = %d, want 0\nstdout: %s\nstderr: %s", code, &stdout, &stderr)
	}
	got := loadCentralConfig(t, centralPath)
	if stringIndex(got.Global.MCPs, "tools") >= 0 {
		t.Fatal("move did not clear global activation")
	}
	assertMCPProjectAssignments(t, got, "tools", "other", "target")
	if agents := got.Projects["target"].DisabledAgents["tools"]; !reflect.DeepEqual(agents, []config.Agent{config.AgentClaude}) {
		t.Fatalf("existing target exclusions = %#v, want [claude]", agents)
	}
}

func TestMoveCommandRejectsUnknownOrNonGlobalInputsWithoutWriting(t *testing.T) {
	tests := []struct {
		name        string
		mcp         string
		project     string
		makeLocal   bool
		wantMessage []string
	}{
		{
			name: "unknown MCP", mcp: "missing", project: "target",
			wantMessage: []string{"MCP", "missing"},
		},
		{
			name: "unknown project", mcp: "tools", project: "missing",
			wantMessage: []string{"project", "missing"},
		},
		{
			name: "non-global MCP assigned elsewhere", mcp: "tools", project: "target", makeLocal: true,
			wantMessage: []string{"tools", "global"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := newMoveTestEnvironment(t)
			centralPath := filepath.Join(environment.base, "config.json")
			cfg := environment.config()
			if test.makeLocal {
				setToolsGlobal(cfg, false)
			}
			writeCentralConfig(t, centralPath, cfg)
			before := readFile(t, centralPath)

			var stdout, stderr bytes.Buffer
			code := run(
				[]string{"move", "--config", centralPath, test.mcp, test.project},
				&stdout,
				&stderr,
			)
			if code == 0 {
				t.Fatalf("run(move) code = 0, want failure\nstdout: %s\nstderr: %s", &stdout, &stderr)
			}
			for _, fragment := range test.wantMessage {
				if !strings.Contains(strings.ToLower(stderr.String()), strings.ToLower(fragment)) {
					t.Fatalf("stderr = %q, want fragment %q", stderr.String(), fragment)
				}
			}
			if after := readFile(t, centralPath); !bytes.Equal(after, before) {
				t.Fatalf("failed move changed central config:\n before %s\n after  %s", before, after)
			}
		})
	}
}

func TestEnableCommandAddsProjectAndSynchronizesTargets(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "central", "config.json")
	cfg := environment.config()
	setToolsGlobal(cfg, false)
	toolsBefore := cfg.MCPs["tools"]
	otherBefore := cfg.MCPs["other-mcp"]
	writeCentralConfig(t, centralPath, cfg)
	writeNativeSentinels(t, environment)

	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"enable", "--config", centralPath, "tools", "target"},
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("run(enable) code = %d, want 0\nstdout: %s\nstderr: %s", code, &stdout, &stderr)
	}
	got := loadCentralConfig(t, centralPath)
	tools := got.MCPs["tools"]
	if stringIndex(got.Global.MCPs, "tools") >= 0 {
		t.Fatalf("global MCPs contain tools: %#v", got.Global.MCPs)
	}
	assertMCPProjectAssignments(t, got, "tools", "other", "target")
	if !reflect.DeepEqual(tools, toolsBefore) {
		t.Fatalf("enable changed the MCP definition:\n got  %#v\n want %#v", tools, toolsBefore)
	}
	if _, exists := got.Projects["target"].DisabledAgents["tools"]; exists {
		t.Fatalf("enable unexpectedly created project exclusions: %#v", got.Projects["target"].DisabledAgents)
	}
	if got.Schema != cfg.Schema || got.Options != cfg.Options ||
		got.Projects["target"].Path != cfg.Projects["target"].Path ||
		got.Projects["other"].Path != cfg.Projects["other"].Path {
		t.Fatalf("enable changed central settings or project paths:\n got  %#v\n want %#v", got, cfg)
	}
	if other := got.MCPs["other-mcp"]; !reflect.DeepEqual(other, otherBefore) {
		t.Fatalf("enable changed another MCP:\n got  %#v\n want %#v", other, otherBefore)
	}
	assertGeneratedTargetsInSync(t, environment, centralPath, got)
}

func TestEnableCommandDryRunDoesNotWrite(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "config.json")
	cfg := environment.config()
	setToolsGlobal(cfg, false)
	writeCentralConfig(t, centralPath, cfg)
	before := readFile(t, centralPath)
	sentinels := writeNativeSentinels(t, environment)

	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"enable", "--config", centralPath, "--dry-run", "tools", "target"},
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("run(enable --dry-run) code = %d, want 0\nstdout: %s\nstderr: %s", code, &stdout, &stderr)
	}
	if after := readFile(t, centralPath); !bytes.Equal(after, before) {
		t.Fatalf("dry run changed central config:\n before %s\n after  %s", before, after)
	}
	assertSentinelsUnchanged(t, sentinels)
}

func TestEnableCommandIsIdempotentAtDestination(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "config.json")
	cfg := environment.config()
	setToolsGlobal(cfg, false)
	writeCentralConfig(t, centralPath, cfg)

	var firstOut, firstErr bytes.Buffer
	if code := run(
		[]string{"enable", "--config", centralPath, "tools", "target"},
		&firstOut,
		&firstErr,
	); code != 0 {
		t.Fatalf("first enable code = %d, want 0\nstdout: %s\nstderr: %s", code, &firstOut, &firstErr)
	}
	afterFirst := readFile(t, centralPath)
	writeNativeSentinels(t, environment)

	var secondOut, secondErr bytes.Buffer
	if code := run(
		[]string{"enable", "--config", centralPath, "tools", "target"},
		&secondOut,
		&secondErr,
	); code != 0 {
		t.Fatalf("second enable code = %d, want idempotent success\nstdout: %s\nstderr: %s", code, &secondOut, &secondErr)
	}
	if afterSecond := readFile(t, centralPath); !bytes.Equal(afterSecond, afterFirst) {
		t.Fatalf("idempotent enable rewrote central config:\n first  %s\n second %s", afterFirst, afterSecond)
	}
	got := loadCentralConfig(t, centralPath)
	if stringIndex(got.Global.MCPs, "tools") >= 0 {
		t.Fatal("idempotent enable made MCP global")
	}
	assertMCPProjectAssignments(t, got, "tools", "other", "target")
	assertGeneratedTargetsInSync(t, environment, centralPath, got)
}

func TestEnableCommandRejectsUnknownOrGlobalInputsWithoutWriting(t *testing.T) {
	tests := []struct {
		name        string
		mcp         string
		project     string
		global      bool
		wantMessage []string
	}{
		{
			name: "unknown MCP", mcp: "missing", project: "target",
			wantMessage: []string{"MCP", "missing"},
		},
		{
			name: "unknown project", mcp: "tools", project: "missing",
			wantMessage: []string{"project", "missing"},
		},
		{
			name: "globally active MCP", mcp: "tools", project: "target", global: true,
			wantMessage: []string{"tools", "global"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := newMoveTestEnvironment(t)
			centralPath := filepath.Join(environment.base, "config.json")
			cfg := environment.config()
			setToolsGlobal(cfg, test.global)
			writeCentralConfig(t, centralPath, cfg)
			before := readFile(t, centralPath)

			var stdout, stderr bytes.Buffer
			code := run(
				[]string{"enable", "--config", centralPath, test.mcp, test.project},
				&stdout,
				&stderr,
			)
			if code == 0 {
				t.Fatalf("run(enable) code = 0, want failure\nstdout: %s\nstderr: %s", &stdout, &stderr)
			}
			for _, fragment := range test.wantMessage {
				if !strings.Contains(strings.ToLower(stderr.String()), strings.ToLower(fragment)) {
					t.Fatalf("stderr = %q, want fragment %q", stderr.String(), fragment)
				}
			}
			if after := readFile(t, centralPath); !bytes.Equal(after, before) {
				t.Fatalf("failed enable changed central config:\n before %s\n after  %s", before, after)
			}
		})
	}
}

func TestDisableCommandRemovesProjectAndSynchronizesTargets(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "central", "config.json")
	cfg := environment.config()
	setToolsGlobal(cfg, false)
	setProjectMCPs(cfg, "target", "tools")
	target := cfg.Projects["target"]
	target.DisabledAgents["tools"] = []config.Agent{config.AgentClaude}
	cfg.Projects["target"] = target
	toolsBefore := cfg.MCPs["tools"]
	otherBefore := cfg.MCPs["other-mcp"]
	writeCentralConfig(t, centralPath, cfg)
	writeNativeSentinels(t, environment)

	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"disable", "--config", centralPath, "tools", "target"},
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("run(disable) code = %d, want 0\nstdout: %s\nstderr: %s", code, &stdout, &stderr)
	}
	got := loadCentralConfig(t, centralPath)
	disabled := got.MCPs["tools"]
	if stringIndex(got.Global.MCPs, "tools") >= 0 {
		t.Fatalf("global MCPs contain tools: %#v", got.Global.MCPs)
	}
	assertMCPProjectAssignments(t, got, "tools", "other")
	if !reflect.DeepEqual(disabled, toolsBefore) {
		t.Fatalf("disable changed the MCP definition:\n got  %#v\n want %#v", disabled, toolsBefore)
	}
	if _, exists := got.Projects["target"].DisabledAgents["tools"]; exists {
		t.Fatalf("disable retained orphaned project exclusions: %#v", got.Projects["target"].DisabledAgents)
	}
	if got.Schema != cfg.Schema || got.Options != cfg.Options ||
		got.Projects["target"].Path != cfg.Projects["target"].Path ||
		got.Projects["other"].Path != cfg.Projects["other"].Path {
		t.Fatalf("disable changed central settings or project paths:\n got  %#v\n want %#v", got, cfg)
	}
	if other := got.MCPs["other-mcp"]; !reflect.DeepEqual(other, otherBefore) {
		t.Fatalf("disable changed another MCP:\n got  %#v\n want %#v", other, otherBefore)
	}
	assertGeneratedTargetsInSync(t, environment, centralPath, got)
}

func TestDisableCommandDryRunDoesNotWrite(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "config.json")
	cfg := environment.config()
	setToolsGlobal(cfg, false)
	setProjectMCPs(cfg, "target", "tools")
	writeCentralConfig(t, centralPath, cfg)
	before := readFile(t, centralPath)
	sentinels := writeNativeSentinels(t, environment)

	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"disable", "--config", centralPath, "--dry-run", "tools", "target"},
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("run(disable --dry-run) code = %d, want 0\nstdout: %s\nstderr: %s", code, &stdout, &stderr)
	}
	if after := readFile(t, centralPath); !bytes.Equal(after, before) {
		t.Fatalf("dry run changed central config:\n before %s\n after  %s", before, after)
	}
	assertSentinelsUnchanged(t, sentinels)
}

func TestDisableCommandIsIdempotentWhenProjectIsAbsent(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "config.json")
	cfg := environment.config()
	setToolsGlobal(cfg, false)
	setProjectMCPs(cfg, "target", "tools")
	writeCentralConfig(t, centralPath, cfg)

	var firstOut, firstErr bytes.Buffer
	if code := run(
		[]string{"disable", "--config", centralPath, "tools", "target"},
		&firstOut,
		&firstErr,
	); code != 0 {
		t.Fatalf("first disable code = %d, want 0\nstdout: %s\nstderr: %s", code, &firstOut, &firstErr)
	}
	afterFirst := readFile(t, centralPath)
	writeNativeSentinels(t, environment)

	var secondOut, secondErr bytes.Buffer
	if code := run(
		[]string{"disable", "--config", centralPath, "tools", "target"},
		&secondOut,
		&secondErr,
	); code != 0 {
		t.Fatalf("second disable code = %d, want idempotent success\nstdout: %s\nstderr: %s", code, &secondOut, &secondErr)
	}
	if afterSecond := readFile(t, centralPath); !bytes.Equal(afterSecond, afterFirst) {
		t.Fatalf("idempotent disable rewrote central config:\n first  %s\n second %s", afterFirst, afterSecond)
	}
	got := loadCentralConfig(t, centralPath)
	if stringIndex(got.Global.MCPs, "tools") >= 0 {
		t.Fatal("idempotent disable made MCP global")
	}
	assertMCPProjectAssignments(t, got, "tools", "other")
	assertGeneratedTargetsInSync(t, environment, centralPath, got)
}

func TestDisableCommandKeepsEmptyProjectsAsArray(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "config.json")
	cfg := environment.config()
	setToolsGlobal(cfg, false)
	setProjectMCPs(cfg, "target", "tools")
	setProjectMCPs(cfg, "other")
	writeCentralConfig(t, centralPath, cfg)

	var stdout, stderr bytes.Buffer
	if code := run(
		[]string{"disable", "--config", centralPath, "tools", "target"},
		&stdout,
		&stderr,
	); code != 0 {
		t.Fatalf("run(disable) code = %d, want 0\nstdout: %s\nstderr: %s", code, &stdout, &stderr)
	}

	disabledProject := loadCentralConfig(t, centralPath).Projects["target"]
	if disabledProject.MCPs == nil || len(disabledProject.MCPs) != 0 {
		t.Fatalf("disabled project MCPs = %#v, want non-nil empty array", disabledProject.MCPs)
	}
}

func TestDisableCommandRejectsUnknownOrGlobalInputsWithoutWriting(t *testing.T) {
	tests := []struct {
		name        string
		mcp         string
		project     string
		global      bool
		wantMessage []string
	}{
		{
			name: "unknown MCP", mcp: "missing", project: "target",
			wantMessage: []string{"MCP", "missing"},
		},
		{
			name: "unknown project", mcp: "tools", project: "missing",
			wantMessage: []string{"project", "missing"},
		},
		{
			name: "globally active MCP", mcp: "tools", project: "target", global: true,
			wantMessage: []string{"tools", "global"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := newMoveTestEnvironment(t)
			centralPath := filepath.Join(environment.base, "config.json")
			cfg := environment.config()
			setToolsGlobal(cfg, test.global)
			setProjectMCPs(cfg, "target", "tools")
			writeCentralConfig(t, centralPath, cfg)
			before := readFile(t, centralPath)

			var stdout, stderr bytes.Buffer
			code := run(
				[]string{"disable", "--config", centralPath, test.mcp, test.project},
				&stdout,
				&stderr,
			)
			if code == 0 {
				t.Fatalf("run(disable) code = 0, want failure\nstdout: %s\nstderr: %s", &stdout, &stderr)
			}
			for _, fragment := range test.wantMessage {
				if !strings.Contains(strings.ToLower(stderr.String()), strings.ToLower(fragment)) {
					t.Fatalf("stderr = %q, want fragment %q", stderr.String(), fragment)
				}
			}
			if after := readFile(t, centralPath); !bytes.Equal(after, before) {
				t.Fatalf("failed disable changed central config:\n before %s\n after  %s", before, after)
			}
		})
	}
}

func TestImportCommandImportsAndSynchronizesTargets(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "central", "config.json")
	writeCentralConfig(t, centralPath, environment.config())
	writeNativeSentinels(t, environment)
	sourcePath := filepath.Join(environment.home, ".claude.json")
	writeTestFile(t, sourcePath, []byte(`{
  "keep": true,
  "mcpServers": {
    "imported": {"type": "stdio", "command": "imported-server"}
  }
}`))

	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"import", "--config", centralPath, "--from", "claude"},
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("run(import) code = %d, want 0\nstdout: %s\nstderr: %s", code, &stdout, &stderr)
	}
	got := loadCentralConfig(t, centralPath)
	imported, exists := got.MCPs["imported"]
	if !exists {
		t.Fatalf("central config does not contain imported MCP: %#v", got.MCPs)
	}
	if imported.Type != "stdio" || imported.Command != "imported-server" {
		t.Fatalf("imported MCP = %#v", imported)
	}
	if stringIndex(got.Global.MCPs, "imported") < 0 {
		t.Fatalf("global MCPs = %#v, want imported activation", got.Global.MCPs)
	}
	assertGeneratedTargetsInSync(t, environment, centralPath, got)
}

func TestImportCommandDryRunWritesNeitherCentralNorTargets(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "config.json")
	writeCentralConfig(t, centralPath, environment.config())
	centralBefore := readFile(t, centralPath)
	sentinels := writeNativeSentinels(t, environment)
	sourcePath := filepath.Join(environment.home, ".claude.json")
	source := []byte(`{
  "keep": true,
  "mcpServers": {
    "imported": {"type": "stdio", "command": "imported-server"}
  }
}`)
	writeTestFile(t, sourcePath, source)
	sentinels[sourcePath] = source

	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"import", "--config", centralPath, "--from", "claude", "--dry-run"},
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("run(import --dry-run) code = %d, want 0\nstdout: %s\nstderr: %s", code, &stdout, &stderr)
	}
	if centralAfter := readFile(t, centralPath); !bytes.Equal(centralAfter, centralBefore) {
		t.Fatalf("import dry run changed central config:\n before %s\n after  %s", centralBefore, centralAfter)
	}
	assertSentinelsUnchanged(t, sentinels)
}

func TestStdioCommandLaunchesConfiguredServer(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "config.json")
	cfg := environment.config()
	tools := cfg.MCPs["tools"]
	tools.Command = os.Args[0]
	cfg.MCPs["tools"] = tools
	writeCentralConfig(t, centralPath, cfg)

	var captured wrapper.Launch
	calls := 0
	restore := execLaunch
	execLaunch = func(launch wrapper.Launch) (int, error) {
		calls++
		captured = launch
		return 7, nil
	}
	t.Cleanup(func() { execLaunch = restore })

	var stdout, stderr bytes.Buffer
	code := run([]string{"stdio", "--config", centralPath, "tools"}, &stdout, &stderr)
	if code != 7 {
		t.Fatalf("run(stdio) code = %d, want the server's exit code 7\nstdout: %s\nstderr: %s", code, &stdout, &stderr)
	}
	if calls != 1 {
		t.Fatalf("execLaunch called %d times, want 1", calls)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdio wrote to stdout, which carries the MCP protocol: %q", stdout.String())
	}
	if want := []string{os.Args[0], "server.js"}; !reflect.DeepEqual(captured.Args, want) {
		t.Fatalf("launch args = %#v, want %#v", captured.Args, want)
	}
	if captured.Path == "" {
		t.Fatal("launch path is empty")
	}
	for _, entry := range []string{"LOG_LEVEL=debug", "TOOLS_TOKEN=test-secret"} {
		if stringIndex(captured.Env, entry) < 0 {
			t.Fatalf("launch env lacks %q: %#v", entry, captured.Env)
		}
	}
}

func TestStdioCommandRejectsInvalidLaunchesWithoutExecuting(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		unsetEnv string
		wantCode int
		wantErr  string
	}{
		{name: "missing name", args: []string{}, wantCode: 2, wantErr: "exactly one MCP name"},
		{name: "too many names", args: []string{"tools", "other-mcp"}, wantCode: 2, wantErr: "exactly one MCP name"},
		{name: "unknown MCP", args: []string{"missing"}, wantCode: 1, wantErr: `MCP "missing" does not exist`},
		{name: "http MCP", args: []string{"other-mcp"}, wantCode: 1, wantErr: "only stdio MCPs can be launched"},
		{name: "unset envFrom", args: []string{"tools"}, unsetEnv: "TOOLS_TOKEN", wantCode: 1, wantErr: `requires environment variable "TOOLS_TOKEN"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := newMoveTestEnvironment(t)
			centralPath := filepath.Join(environment.base, "config.json")
			cfg := environment.config()
			tools := cfg.MCPs["tools"]
			tools.Command = os.Args[0]
			cfg.MCPs["tools"] = tools
			writeCentralConfig(t, centralPath, cfg)
			if test.unsetEnv != "" {
				t.Setenv(test.unsetEnv, "")
				if err := os.Unsetenv(test.unsetEnv); err != nil {
					t.Fatal(err)
				}
			}
			restore := execLaunch
			execLaunch = func(wrapper.Launch) (int, error) {
				t.Fatal("execLaunch was called for an invalid launch")
				return 0, nil
			}
			t.Cleanup(func() { execLaunch = restore })

			var stdout, stderr bytes.Buffer
			code := run(append([]string{"stdio", "--config", centralPath}, test.args...), &stdout, &stderr)
			if code != test.wantCode {
				t.Fatalf("run(stdio) code = %d, want %d\nstderr: %s", code, test.wantCode, &stderr)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdio wrote to stdout: %q", stdout.String())
			}
			if !strings.Contains(stderr.String(), test.wantErr) {
				t.Fatalf("stderr = %q, want substring %q", stderr.String(), test.wantErr)
			}
		})
	}
}

func TestSyncWritesWrapperEntriesThatImportRecognizes(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "central", "config.json")
	writeCentralConfig(t, centralPath, environment.config())

	var syncOut, syncErr bytes.Buffer
	if code := run([]string{"sync", "--config", centralPath}, &syncOut, &syncErr); code != 0 {
		t.Fatalf("run(sync) code = %d, want 0\nstdout: %s\nstderr: %s", code, &syncOut, &syncErr)
	}
	claude := string(readFile(t, filepath.Join(environment.home, ".claude.json")))
	for _, fragment := range []string{`"mcp-manager"`, `"stdio"`, `"--config"`, centralPath} {
		if !strings.Contains(claude, fragment) {
			t.Fatalf("generated Claude config lacks %q:\n%s", fragment, claude)
		}
	}
	if strings.Contains(claude, "server.js") {
		t.Fatalf("generated Claude config still contains the real command line:\n%s", claude)
	}
	before := readFile(t, centralPath)

	for _, agent := range []string{"claude", "codex", "opencode"} {
		var stdout, stderr bytes.Buffer
		code := run([]string{"import", "--config", centralPath, "--from", agent}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run(import --from %s) code = %d, want 0\nstdout: %s\nstderr: %s", agent, code, &stdout, &stderr)
		}
		if !strings.Contains(stdout.String(), "central config already contains these MCPs") {
			t.Fatalf("import --from %s output = %q, want no changes", agent, stdout.String())
		}
	}
	if after := readFile(t, centralPath); !bytes.Equal(after, before) {
		t.Fatalf("importing generated wrapper entries changed the central config:\n before %s\n after  %s", before, after)
	}
	got := loadCentralConfig(t, centralPath)
	if got.MCPs["tools"].Command != "node" {
		t.Fatalf("central definition was replaced by the wrapper: %#v", got.MCPs["tools"])
	}
}

type moveTestEnvironment struct {
	base       string
	home       string
	userConfig string
	target     string
	other      string
}

func newMoveTestEnvironment(t *testing.T) moveTestEnvironment {
	t.Helper()
	base := t.TempDir()
	environment := moveTestEnvironment{
		base:       base,
		home:       filepath.Join(base, "home"),
		userConfig: filepath.Join(base, "user-config"),
		target:     filepath.Join(base, "projects", "target"),
		other:      filepath.Join(base, "projects", "other"),
	}
	for _, path := range []string{environment.home, environment.userConfig, environment.target, environment.other} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", environment.home)
	t.Setenv("XDG_CONFIG_HOME", environment.userConfig)
	t.Setenv("TOOLS_TOKEN", "test-secret")
	return environment
}

func (environment moveTestEnvironment) config() *config.Config {
	return &config.Config{
		Schema:  "https://example.com/mcp-manager.schema.json",
		Version: config.CurrentVersion,
		Options: config.Options{InlineSecrets: true},
		Global: config.Scope{
			MCPs: []string{"tools", "other-mcp"},
			DisabledAgents: map[string][]config.Agent{
				"tools": {config.AgentOpenCode},
			},
		},
		Projects: map[string]config.Project{
			"target": {
				Path: environment.target, MCPs: []string{},
				DisabledAgents: map[string][]config.Agent{},
			},
			"other": {
				Path: environment.other, MCPs: []string{"tools"},
				DisabledAgents: map[string][]config.Agent{
					"tools": {config.AgentOpenCode},
				},
			},
		},
		MCPs: map[string]config.MCP{
			"tools": {
				Type: "stdio", Command: "node", Args: []string{"server.js"},
				Env: map[string]string{"LOG_LEVEL": "debug"}, EnvFrom: []string{"TOOLS_TOKEN"},
			},
			"other-mcp": {
				Type: "http", URL: "https://example.com/mcp",
				Headers: map[string]string{"X-Region": "eu"},
			},
		},
	}
}

func setToolsGlobal(cfg *config.Config, global bool) {
	index := stringIndex(cfg.Global.MCPs, "tools")
	if global {
		if index < 0 {
			cfg.Global.MCPs = append(cfg.Global.MCPs, "tools")
		}
		return
	}
	if index >= 0 {
		cfg.Global.MCPs = append(cfg.Global.MCPs[:index], cfg.Global.MCPs[index+1:]...)
	}
	delete(cfg.Global.DisabledAgents, "tools")
}

func setProjectMCPs(cfg *config.Config, projectID string, names ...string) {
	project := cfg.Projects[projectID]
	project.MCPs = append([]string{}, names...)
	for name := range project.DisabledAgents {
		if stringIndex(project.MCPs, name) < 0 {
			delete(project.DisabledAgents, name)
		}
	}
	cfg.Projects[projectID] = project
}

func writeCentralConfig(t *testing.T, path string, cfg *config.Config) {
	t.Helper()
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
}

func loadCentralConfig(t *testing.T, path string) *config.Config {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func writeNativeSentinels(t *testing.T, environment moveTestEnvironment) map[string][]byte {
	t.Helper()
	sentinels := map[string][]byte{
		filepath.Join(environment.home, ".codex", "config.toml"):           []byte("model = \"keep\"\n"),
		filepath.Join(environment.home, ".claude.json"):                    []byte("{\"keep\":true}\n"),
		filepath.Join(environment.userConfig, "opencode", "opencode.json"): []byte("{\"keep\":true}\n"),
		filepath.Join(environment.target, ".codex", "config.toml"):         []byte("model = \"keep\"\n"),
		filepath.Join(environment.target, ".mcp.json"):                     []byte("{\"keep\":true}\n"),
		filepath.Join(environment.target, "opencode.json"):                 []byte("{\"keep\":true}\n"),
		filepath.Join(environment.other, ".codex", "config.toml"):          []byte("model = \"keep\"\n"),
		filepath.Join(environment.other, ".mcp.json"):                      []byte("{\"keep\":true}\n"),
		filepath.Join(environment.other, "opencode.json"):                  []byte("{\"keep\":true}\n"),
	}
	for path, data := range sentinels {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return sentinels
}

func assertSentinelsUnchanged(t *testing.T, sentinels map[string][]byte) {
	t.Helper()
	for path, want := range sentinels {
		if got := readFile(t, path); !bytes.Equal(got, want) {
			t.Fatalf("dry run changed native config %s:\n got  %q\n want %q", path, got, want)
		}
	}
}

func assertGeneratedTargetsInSync(t *testing.T, environment moveTestEnvironment, centralPath string, cfg *config.Config) {
	t.Helper()
	result, err := syncer.Sync(cfg, syncer.Options{
		HomeDir:       environment.home,
		UserConfigDir: environment.userConfig,
		InlineSecrets: cfg.Options.InlineSecrets,
		LookupEnv:     os.LookupEnv,
		ConfigPath:    centralPath,
	})
	if err != nil {
		t.Fatalf("verify generated targets: %v", err)
	}
	if len(result.Changes) != 0 || result.Unchanged != 9 {
		t.Fatalf(
			"generated targets were not already synchronized: %d changed, %d unchanged",
			len(result.Changes), result.Unchanged,
		)
	}
}

func assertMCPProjectAssignments(t *testing.T, cfg *config.Config, mcpName string, expected ...string) {
	t.Helper()
	actual := make([]string, 0, len(cfg.Projects))
	for projectID, project := range cfg.Projects {
		if stringIndex(project.MCPs, mcpName) >= 0 {
			actual = append(actual, projectID)
		}
	}
	counts := make(map[string]int, len(actual))
	for _, project := range actual {
		counts[project]++
	}
	if len(actual) != len(expected) {
		t.Fatalf("project assignments = %v, want exactly %v", actual, expected)
	}
	for _, project := range expected {
		if counts[project] != 1 {
			t.Fatalf("project assignments = %v, want %q exactly once", actual, project)
		}
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
