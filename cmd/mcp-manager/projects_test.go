package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/daniel100097/mcp-manager/internal/config"
)

func runProjectOK(t *testing.T, centralPath string, args ...string) string {
	t.Helper()
	args = append(args, "--config", centralPath)
	var stdout, stderr bytes.Buffer
	if code := runProject(args, &stdout, &stderr); code != 0 {
		t.Fatalf("project %v = %d\nstdout: %s\nstderr: %s", args, code, &stdout, &stderr)
	}
	return stdout.String()
}

func TestProjectAddInitializesRegistryFromCurrentDirectoryWithoutSync(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	projectPath := filepath.Join(environment.target, "A sample.project")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(projectPath)
	centralPath := filepath.Join(environment.base, "central", "config.json")
	nativePath := filepath.Join(projectPath, ".mcp.json")
	native := []byte(`{"mcpServers":{"unmanaged":{"command":"existing-server"}}}`)
	writeTestFile(t, nativePath, native)
	sentinels := writeNativeSentinels(t, environment)
	output := runProjectOK(t, centralPath, "add")
	if !strings.Contains(output, `registered project "A-sample-project"`) || !strings.Contains(output, "do not sync") {
		t.Fatalf("registration output = %s", output)
	}
	cfg := loadCentralConfig(t, centralPath)
	project := cfg.Projects["A-sample-project"]
	if len(cfg.Projects) != 1 || project.Path != projectPath || project.IncludeWorktrees || len(project.MCPs) != 0 || len(cfg.MCPs) != 0 {
		t.Fatalf("registered config = %#v", cfg)
	}
	if !bytes.Equal(readFile(t, nativePath), native) {
		t.Fatal("registration overwrote an existing agent config")
	}
	assertSentinelsUnchanged(t, sentinels)
	for _, relative := range []string{".codex/config.toml", "opencode.json"} {
		if _, err := os.Stat(filepath.Join(projectPath, relative)); !os.IsNotExist(err) {
			t.Fatalf("registration wrote %s: %v", relative, err)
		}
	}
	before := readFile(t, centralPath)
	if output := runProjectOK(t, centralPath, "add", "."); !strings.Contains(output, "already registered") {
		t.Fatalf("repeated registration output = %s", output)
	}
	if !bytes.Equal(before, readFile(t, centralPath)) {
		t.Fatal("idempotent registration rewrote the config")
	}
}

func TestProjectAddRejectsNameCollisionAndAcceptsNamedRelativePath(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "config.json")
	writeCentralConfig(t, centralPath, environment.config())
	before := readFile(t, centralPath)
	newPath := filepath.Join(environment.base, "new", "target")
	if err := os.MkdirAll(newPath, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Dir(newPath))
	var stdout, stderr bytes.Buffer
	if code := runProject([]string{"add", "target", "--config", centralPath}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "choose another --name") {
		t.Fatalf("collision = %d; stderr: %s", code, &stderr)
	}
	if !bytes.Equal(before, readFile(t, centralPath)) {
		t.Fatal("name collision changed registry")
	}
	runProjectOK(t, centralPath, "add", "target", "--name", "new-project")
	if cfg := loadCentralConfig(t, centralPath); cfg.Projects["new-project"].Path != newPath || len(cfg.Projects) != 3 {
		t.Fatalf("named relative registration = %#v", cfg.Projects)
	}
}

func TestProjectAddRecognizesSymlinkAndRetainsSettings(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "config.json")
	cfg := environment.config()
	project := cfg.Projects["target"]
	project.IncludeWorktrees = true
	project.MCPs = []string{"tools"}
	project.DisabledAgents = map[string][]config.Agent{"tools": {config.AgentClaude}}
	cfg.Projects["target"] = project
	writeCentralConfig(t, centralPath, cfg)
	alias := filepath.Join(environment.base, "alias")
	if err := os.Symlink(environment.target, alias); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, centralPath)
	output := runProjectOK(t, centralPath, "add", alias, "--name", "unused-id")
	if !strings.Contains(output, `project "target" is already registered`) {
		t.Fatalf("symlink registration output = %s", output)
	}
	if !bytes.Equal(before, readFile(t, centralPath)) {
		t.Fatal("registration changed existing settings")
	}
}

func TestProjectRegistryDryRunsWriteNothing(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "central", "config.json")
	t.Chdir(environment.target)
	runProjectOK(t, centralPath, "add", "--dry-run")
	if _, err := os.Stat(centralPath); !os.IsNotExist(err) {
		t.Fatalf("add dry run created config: %v", err)
	}
	writeCentralConfig(t, centralPath, environment.config())
	before := readFile(t, centralPath)
	sentinels := writeNativeSentinels(t, environment)
	output := runProjectOK(t, centralPath, "remove", "--dry-run")
	if !strings.Contains(output, `would remove project "target"`) {
		t.Fatalf("remove dry run output = %s", output)
	}
	if !bytes.Equal(before, readFile(t, centralPath)) {
		t.Fatal("remove dry run changed config")
	}
	assertSentinelsUnchanged(t, sentinels)
}

func TestProjectRemoveUsesCurrentDirectoryAndRetainsAgentConfigs(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "config.json")
	cfg := environment.config()
	writeCentralConfig(t, centralPath, cfg)
	sentinels := writeNativeSentinels(t, environment)
	nested := filepath.Join(environment.other, "src")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(nested)
	output := runProjectOK(t, centralPath, "remove")
	if !strings.Contains(output, `removed project "other"`) || !strings.Contains(output, "configs are retained") {
		t.Fatalf("remove output = %s", output)
	}
	delete(cfg.Projects, "other")
	if after := loadCentralConfig(t, centralPath); !reflect.DeepEqual(after, cfg) {
		t.Fatalf("remove changed unrelated settings: got %#v, want %#v", after, cfg)
	}
	assertSentinelsUnchanged(t, sentinels)
}

func TestProjectRegistryEditsPreserveLocalOverridesAndCentralFile(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "config.json")
	writeCentralConfig(t, centralPath, environment.config())
	centralBefore := readFile(t, centralPath)
	localPath := config.LocalPathFor(centralPath)
	writeTextFile(t, localPath, `{"projects":{"target":{"includeWorktrees":false,"mcps":["tools"],"disabledAgents":{"tools":["claude"]}}}}`)
	newPath := filepath.Join(environment.home, "additional")
	if err := os.MkdirAll(newPath, 0o755); err != nil {
		t.Fatal(err)
	}
	runProjectOK(t, centralPath, "add", newPath)
	runProjectOK(t, centralPath, "remove", "other")
	if !bytes.Equal(centralBefore, readFile(t, centralPath)) {
		t.Fatal("project edits changed central defaults despite local override")
	}
	source := config.Source{Path: centralPath, LocalPath: localPath}
	effective, err := source.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := effective.Projects["other"]; exists {
		t.Fatal("removed project survives in effective config")
	}
	if effective.Projects["additional"].Path != newPath || !reflect.DeepEqual(effective.Projects["target"].DisabledAgents["tools"], []config.Agent{config.AgentClaude}) {
		t.Fatalf("local overrides not preserved: %#v", effective.Projects)
	}
	if !bytes.Contains(readFile(t, localPath), []byte(`"includeWorktrees": false`)) {
		t.Fatal("registry edit dropped pinned worktree setting")
	}
}

func TestProjectAddBootstrapsExistingLocalOverlayWithoutHidingRegistration(t *testing.T) {
	for _, populated := range []bool{false, true} {
		name := "empty"
		if populated {
			name = "populated"
		}
		t.Run(name, func(t *testing.T) {
			environment := newMoveTestEnvironment(t)
			t.Chdir(environment.other)
			centralPath := filepath.Join(environment.base, "central", "config.json")
			localPath := config.LocalPathFor(centralPath)
			local := `{}`
			if populated {
				local = `{"global":{"mcps":["existing"]},"projects":{"target":{"path":` + jsonQuote(t, environment.target) + `,"mcps":["existing"]}},"mcps":{"existing":{"type":"stdio","command":"existing-server"}}}`
			}
			writeTextFile(t, localPath, local)
			sentinels := writeNativeSentinels(t, environment)
			runProjectOK(t, centralPath, "add", "--dry-run")
			if _, err := os.Stat(centralPath); !os.IsNotExist(err) {
				t.Fatalf("dry run created central config: %v", err)
			}
			if string(readFile(t, localPath)) != local {
				t.Fatal("dry run changed orphan overlay")
			}
			output := runProjectOK(t, centralPath, "add")
			if !strings.Contains(output, `registered project "other"`) || !strings.Contains(output, "local config updated") {
				t.Fatalf("registration output = %s", output)
			}
			base := loadCentralConfig(t, centralPath)
			if len(base.MCPs) != 0 || len(base.Projects) != 0 || len(base.Global.MCPs) != 0 {
				t.Fatalf("new central config should be an empty base: %#v", base)
			}
			effective, err := (config.Source{Path: centralPath, LocalPath: localPath}).Load()
			if err != nil {
				t.Fatal(err)
			}
			if effective.Projects["other"].Path != environment.other {
				t.Fatalf("added project is not visible after layering: %#v", effective.Projects)
			}
			if populated && (effective.Projects["target"].Path != environment.target || effective.MCPs["existing"].Command != "existing-server" || !reflect.DeepEqual(effective.Global.MCPs, []string{"existing"})) {
				t.Fatalf("existing overlay data was lost: %#v", effective)
			}
			assertSentinelsUnchanged(t, sentinels)
		})
	}
}

func TestResolveProjectUsesNearestCanonicalAncestorAndToleratesMissingDirectories(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	nestedRoot := filepath.Join(environment.target, "nested")
	nestedCWD := filepath.Join(nestedRoot, "src")
	if err := os.MkdirAll(nestedCWD, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(environment.home, "repo-link")
	if err := os.Symlink(environment.target, alias); err != nil {
		t.Fatal(err)
	}
	cfg := environment.config()
	target := cfg.Projects["target"]
	target.Path = "~/repo-link"
	cfg.Projects["target"] = target
	cfg.Projects["nested"] = config.Project{Path: nestedRoot}
	cfg.Projects["missing"] = config.Project{Path: filepath.Join(environment.base, "missing"), IncludeWorktrees: true}
	t.Chdir(nestedCWD)
	for _, test := range []struct {
		selector string
		want     string
	}{
		{"", "nested"},
		{".", "nested"},
		{"target", "target"},
		{"missing", "missing"},
		{filepath.Join(alias, "nested", "src"), "nested"},
		{alias, "target"},
	} {
		got, err := resolveProject(cfg, test.selector)
		if err != nil || got != test.want {
			t.Errorf("resolveProject(%q) = %q, %v; want %q", test.selector, got, err, test.want)
		}
	}
	if cfg.Projects["target"].Path != "~/repo-link" {
		t.Fatal("resolver expanded stored path in place")
	}
	sibling := environment.target + "-sibling"
	if err := os.Mkdir(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveProject(cfg, sibling); err == nil || !strings.Contains(err.Error(), "mcp-manager project add") {
		t.Fatalf("unregistered sibling error = %v", err)
	}
}

func TestResolveProjectWorktreesRequireOptInAndRespectExplicitRegistrations(t *testing.T) {
	environment, centralPath, worktree := newWorktreesTestEnvironment(t)
	cfg := loadCentralConfig(t, centralPath)
	nested := filepath.Join(worktree, "src")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(nested)
	if _, err := resolveProject(cfg, ""); err == nil || !strings.Contains(err.Error(), "project add") {
		t.Fatalf("worktree without opt-in error = %v", err)
	}
	project := cfg.Projects["target"]
	project.IncludeWorktrees = true
	cfg.Projects["target"] = project
	assertResolvedProject(t, cfg, "target")
	// A containing umbrella directory is less specific than the worktree.
	cfg.Projects["umbrella"] = config.Project{Path: environment.base}
	assertResolvedProject(t, cfg, "target")
	cfg.Projects["explicit"] = config.Project{Path: worktree}
	assertResolvedProject(t, cfg, "explicit")
	cfg.Projects["nested"] = config.Project{Path: nested}
	assertResolvedProject(t, cfg, "nested")
}

func TestResolveProjectRejectsAmbiguousWorktreeOwners(t *testing.T) {
	environment, centralPath, worktree := newWorktreesTestEnvironment(t)
	cfg := loadCentralConfig(t, centralPath)
	project := cfg.Projects["target"]
	project.IncludeWorktrees = true
	cfg.Projects["target"] = project
	cfg.Projects["second-owner"] = config.Project{Path: worktree, IncludeWorktrees: true}
	third := filepath.Join(environment.base, "third")
	worktreesGit(t, environment.target, "worktree", "add", "--detach", third)
	t.Chdir(third)
	if _, err := resolveProject(cfg, ""); err == nil || !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "--project ID") {
		t.Fatalf("ambiguous worktree error = %v", err)
	}
	if got, err := resolveProject(cfg, "target"); err != nil || got != "target" {
		t.Fatalf("explicit ID with ambiguous cwd = %q, %v", got, err)
	}
	cfg.Projects["explicit"] = config.Project{Path: third}
	assertResolvedProject(t, cfg, "explicit")
}

func assertResolvedProject(t *testing.T, cfg *config.Config, want string) {
	t.Helper()
	if got, err := resolveProject(cfg, ""); err != nil || got != want {
		t.Fatalf("current project = %q, %v; want %q", got, err, want)
	}
}

func TestProjectListAndShowUseCurrentDirectoryAndPathSelectors(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "config.json")
	writeCentralConfig(t, centralPath, environment.config())
	t.Chdir(environment.other)
	output := runProjectOK(t, centralPath, "show")
	for _, want := range []string{"Project: other", "Path: " + environment.other, "Worktrees: disabled", "Local MCPs: tools", "Global MCPs: tools, other-mcp"} {
		if !strings.Contains(output, want) {
			t.Errorf("show output missing %q: %s", want, output)
		}
	}
	output = runProjectOK(t, centralPath, "show", "--project", environment.target)
	if !strings.Contains(output, "Project: target") {
		t.Fatalf("path selector output = %s", output)
	}
	output = runProjectOK(t, centralPath, "list")
	if !strings.Contains(output, "target") || !strings.Contains(output, "other") || !strings.Contains(output, "*") {
		t.Fatalf("list output = %s", output)
	}
}

func TestProjectCommandArguments(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "config.json")
	writeCentralConfig(t, centralPath, environment.config())
	for _, test := range []struct {
		args []string
		code int
	}{
		{[]string{"--help"}, 0},
		{[]string{"add", "--help"}, 0},
		{[]string{"unknown"}, 2},
		{[]string{"add", "a", "b"}, 2},
		{[]string{"list", "extra"}, 2},
		{[]string{"show", "target", "--project", "other"}, 2},
		{[]string{"add", environment.target, "--name", "invalid.name"}, 2},
		{[]string{"remove", "unknown"}, 1},
	} {
		args := append(test.args, "--config", centralPath)
		var stdout, stderr bytes.Buffer
		if code := runProject(args, &stdout, &stderr); code != test.code {
			t.Errorf("project %v = %d, want %d; stderr: %s", args, code, test.code, &stderr)
		}
	}
}
