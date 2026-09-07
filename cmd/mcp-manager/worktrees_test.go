package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/daniel100097/mcp-manager/internal/config"
)

func worktreesGit(t *testing.T, path string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", path}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func newWorktreesTestEnvironment(t *testing.T) (moveTestEnvironment, string, string) {
	t.Helper()
	environment := newMoveTestEnvironment(t)
	worktreesGit(t, environment.target, "init")
	worktreesGit(t, environment.target, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "initial")
	worktree := filepath.Join(environment.base, "feature tree")
	worktreesGit(t, environment.target, "worktree", "add", "--detach", worktree)
	cfg := environment.config()
	setToolsGlobal(cfg, false)
	setProjectMCPs(cfg, "target", "tools")
	centralPath := filepath.Join(environment.base, "central", "config.json")
	writeCentralConfig(t, centralPath, cfg)
	return environment, centralPath, worktree
}

func runWorktreesOK(t *testing.T, centralPath, action string, extra ...string) string {
	t.Helper()
	args := append([]string{"worktrees", action, "--config", centralPath}, extra...)
	args = append(args, "target")
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("run(%v) = %d\nstdout: %s\nstderr: %s", args, code, &stdout, &stderr)
	}
	return stdout.String()
}

func assertWorktreeHasMCP(t *testing.T, worktree string) {
	t.Helper()
	for _, relative := range []string{".codex/config.toml", ".mcp.json", "opencode.json"} {
		if data := readFile(t, filepath.Join(worktree, relative)); !bytes.Contains(data, []byte("tools")) {
			t.Fatalf("%s has no tools MCP: %s", relative, data)
		}
	}
}

func TestWorktreesEnableSynchronizesAndDisableStopsDiscovery(t *testing.T) {
	environment, centralPath, worktree := newWorktreesTestEnvironment(t)
	before := loadCentralConfig(t, centralPath)
	output := runWorktreesOK(t, centralPath, "enable")
	if strings.Contains(output, "would update") {
		t.Fatalf("enable printed preflight preview: %s", output)
	}
	after := loadCentralConfig(t, centralPath)
	project := before.Projects["target"]
	project.IncludeWorktrees = true
	before.Projects["target"] = project
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("enable changed unrelated config: got %#v, want %#v", after, before)
	}
	assertWorktreeHasMCP(t, worktree)

	second := filepath.Join(environment.base, "second")
	worktreesGit(t, environment.target, "worktree", "add", "--detach", second)
	output = runWorktreesOK(t, centralPath, "enable")
	if !strings.Contains(output, "already enabled") {
		t.Fatalf("second enable output = %s", output)
	}
	assertWorktreeHasMCP(t, second)

	runWorktreesOK(t, centralPath, "disable")
	if loadCentralConfig(t, centralPath).Projects["target"].IncludeWorktrees {
		t.Fatal("discovery is still enabled")
	}
	assertWorktreeHasMCP(t, worktree)
	third := filepath.Join(environment.base, "third")
	worktreesGit(t, environment.target, "worktree", "add", "--detach", third)
	runWorktreesOK(t, centralPath, "disable")
	for _, relative := range []string{".codex/config.toml", ".mcp.json", "opencode.json"} {
		if _, err := os.Stat(filepath.Join(third, relative)); !os.IsNotExist(err) {
			t.Fatalf("disabled discovery wrote %s: %v", relative, err)
		}
	}
}

func TestWorktreesDryRunWritesNothing(t *testing.T) {
	environment, centralPath, worktree := newWorktreesTestEnvironment(t)
	before := readFile(t, centralPath)
	sentinels := writeNativeSentinels(t, environment)
	output := runWorktreesOK(t, centralPath, "enable", "--dry-run")
	if !strings.Contains(output, "would enable") || !strings.Contains(output, worktree) {
		t.Fatalf("dry run output = %s", output)
	}
	if !bytes.Equal(before, readFile(t, centralPath)) {
		t.Fatal("dry run changed central config")
	}
	assertSentinelsUnchanged(t, sentinels)
	for _, relative := range []string{".codex/config.toml", ".mcp.json", "opencode.json"} {
		if _, err := os.Stat(filepath.Join(worktree, relative)); !os.IsNotExist(err) {
			t.Fatalf("dry run wrote %s: %v", relative, err)
		}
	}
}

func TestWorktreesLocalOverrideCanDisableCentralDiscovery(t *testing.T) {
	_, centralPath, worktree := newWorktreesTestEnvironment(t)
	runWorktreesOK(t, centralPath, "enable")
	centralBefore := readFile(t, centralPath)
	localPath := filepath.Join(filepath.Dir(centralPath), "custom.local.json")
	writeTextFile(t, localPath, `{"projects":{"target":{"disabledAgents":{"tools":["opencode"]}}}}`)
	runWorktreesOK(t, centralPath, "disable", "--config-local", localPath)
	if !bytes.Equal(centralBefore, readFile(t, centralPath)) {
		t.Fatal("disable with local override changed central config")
	}
	if !bytes.Contains(readFile(t, localPath), []byte(`"includeWorktrees": false`)) {
		t.Fatalf("local override did not retain explicit false: %s", readFile(t, localPath))
	}
	source := config.Source{Path: centralPath, LocalPath: localPath}
	effective, err := source.Load()
	if err != nil {
		t.Fatal(err)
	}
	if effective.Projects["target"].IncludeWorktrees {
		t.Fatal("effective discovery is still enabled")
	}
	runWorktreesOK(t, centralPath, "enable", "--config-local", localPath)
	effective, err = source.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !effective.Projects["target"].IncludeWorktrees || !reflect.DeepEqual(effective.Projects["target"].DisabledAgents["tools"], []config.Agent{config.AgentOpenCode}) {
		t.Fatalf("local enable did not preserve project settings: %#v", effective.Projects["target"])
	}
	if bytes.Contains(readFile(t, filepath.Join(worktree, "opencode.json")), []byte("tools")) {
		t.Fatal("worktree did not inherit disabledAgents")
	}
}

func TestWorktreesInvalidRepositoryDoesNotSave(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "config.json")
	writeCentralConfig(t, centralPath, environment.config())
	before := readFile(t, centralPath)
	sentinels := writeNativeSentinels(t, environment)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"worktrees", "enable", "--config", centralPath, "target"}, &stdout, &stderr); code != 1 {
		t.Fatalf("invalid repository enable = %d, want 1; stderr: %s", code, &stderr)
	}
	if !bytes.Equal(before, readFile(t, centralPath)) {
		t.Fatal("failed enable persisted activation")
	}
	assertSentinelsUnchanged(t, sentinels)
}

func TestWorktreesArgumentsAndHelp(t *testing.T) {
	environment := newMoveTestEnvironment(t)
	centralPath := filepath.Join(environment.base, "config.json")
	writeCentralConfig(t, centralPath, environment.config())
	for _, test := range []struct {
		args []string
		code int
	}{
		{[]string{"worktrees"}, 2},
		{[]string{"worktrees", "unknown"}, 2},
		{[]string{"worktrees", "enable"}, 1},
		{[]string{"worktrees", "disable", "a", "b"}, 2},
		{[]string{"worktrees", "enable", "--config", centralPath, "missing"}, 1},
		{[]string{"worktrees", "--help"}, 0},
		{[]string{"worktrees", "enable", "--help"}, 0},
		{[]string{"worktrees", "disable", "--help"}, 0},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(test.args, &stdout, &stderr); code != test.code {
			t.Errorf("run(%v) = %d, want %d; stderr: %s", test.args, code, test.code, &stderr)
		}
	}
}
