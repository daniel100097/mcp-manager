package syncer

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/daniel100097/mcp-manager/internal/config"
)

func gitForTest(t *testing.T, root string, args ...string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git is required for worktree integration tests")
	}
	gitArgs := []string{"-C", root, "-c", "user.name=MCP test", "-c", "user.email=mcp@example.test", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + filepath.Join(t.TempDir(), "no-hooks")}
	cmd := exec.Command("git", append(gitArgs, args...)...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func worktreeFixture(t *testing.T) (*config.Config, Options, string, string) {
	t.Helper()
	base := t.TempDir()
	// Match config.Load's canonicalization (notably /var on macOS).
	base, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "main repo")
	linked := filepath.Join(base, "linked checkout")
	mustMkdir(t, root)
	gitForTest(t, root, "init")
	gitForTest(t, root, "commit", "--allow-empty", "-m", "initial")
	gitForTest(t, root, "worktree", "add", "--detach", linked)
	cfg := &config.Config{
		Version: config.CurrentVersion,
		Global:  config.Scope{MCPs: []string{"global"}},
		Projects: map[string]config.Project{
			"repo": {
				Path: root, MCPs: []string{"local", "remote"},
				DisabledAgents: map[string][]config.Agent{"remote": {config.AgentClaude}},
			},
		},
		MCPs: map[string]config.MCP{
			"global": {Type: "http", URL: "https://example.test/global"},
			"local":  {Type: "stdio", Command: "tool"},
			"remote": {Type: "http", URL: "https://example.test/mcp"},
		},
	}
	return cfg, testOptions(t, base), root, linked
}

func optIn(cfg *config.Config, id string) {
	project := cfg.Projects[id]
	project.IncludeWorktrees = true
	cfg.Projects[id] = project
}

func TestSyncWorktreesOptInAndInheritance(t *testing.T) {
	cfg, options, root, linked := worktreeFixture(t)
	if _, err := Sync(cfg, options); err != nil {
		t.Fatal(err)
	}
	linkedClaude := filepath.Join(linked, ".mcp.json")
	if _, err := os.Stat(linkedClaude); !os.IsNotExist(err) {
		t.Fatalf("disabled discovery wrote to worktree: %v", err)
	}
	optIn(cfg, "repo")
	options.DryRun = true
	preview, err := Sync(cfg, options)
	if err != nil || len(preview.Changes) != 3 {
		t.Fatalf("dry run = %#v, %v; want three worktree files", preview, err)
	}
	for _, change := range preview.Changes {
		if change.Applied || change.Scope != "repo" || !strings.HasPrefix(change.Path, linked+string(filepath.Separator)) {
			t.Fatalf("unexpected preview: %#v", change)
		}
	}
	if _, err := os.Stat(linkedClaude); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote to worktree: %v", err)
	}
	mustWrite(t, linkedClaude, []byte(`{"keep":true,"mcpServers":{"stale":{"command":"old"}}}`), 0o600)
	options.DryRun = false
	if _, err := Sync(cfg, options); err != nil {
		t.Fatal(err)
	}
	claude := readJSON(t, linkedClaude)
	assertKeys(t, nestedMap(t, claude, "mcpServers"), "local")
	if claude["keep"] != true {
		t.Fatal("worktree's unrelated settings were lost")
	}
	for _, name := range []string{filepath.Join(".codex", "config.toml"), "opencode.json"} {
		original, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		assertFileBytes(t, filepath.Join(linked, name), original)
	}
	second, err := Sync(cfg, options)
	if err != nil || len(second.Changes) != 0 || second.Unchanged != 9 {
		t.Fatalf("second sync = %#v, %v; want nine unchanged targets", second, err)
	}
}

func TestSyncWorktreesExplicitScopeWins(t *testing.T) {
	cfg, options, _, linked := worktreeFixture(t)
	optIn(cfg, "repo")
	cfg.Projects["linked"] = config.Project{Path: linked, MCPs: []string{"remote"}}
	if _, err := Sync(cfg, options); err != nil {
		t.Fatal(err)
	}
	assertKeys(t, nestedMap(t, readJSON(t, filepath.Join(linked, ".mcp.json")), "mcpServers"), "remote")
}

func TestSyncWorktreesFromLinkedRoot(t *testing.T) {
	cfg, options, root, linked := worktreeFixture(t)
	project := cfg.Projects["repo"]
	project.Path = linked
	project.IncludeWorktrees = true
	cfg.Projects["repo"] = project
	gitForTest(t, root, "worktree", "lock", "--reason", "keep for testing", linked)
	if _, err := Sync(cfg, options); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, linked} {
		assertKeys(t, nestedMap(t, readJSON(t, filepath.Join(path, ".mcp.json")), "mcpServers"), "local")
	}
}

func TestSyncWorktreesSkipsPrunableDirectory(t *testing.T) {
	cfg, options, _, linked := worktreeFixture(t)
	optIn(cfg, "repo")
	if err := os.Remove(filepath.Join(linked, ".git")); err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(cfg, options); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(linked, ".mcp.json")); !os.IsNotExist(err) {
		t.Fatalf("sync wrote to a prunable worktree: %v", err)
	}
}

func TestSyncWithoutWorktreeDiscoveryDoesNotRequireGit(t *testing.T) {
	cfg, options, _, _ := worktreeFixture(t)
	t.Setenv("PATH", t.TempDir())
	if _, err := Sync(cfg, options); err != nil {
		t.Fatal(err)
	}
}

func TestSyncWorktreesMissingAndBare(t *testing.T) {
	cfg, options, root, linked := worktreeFixture(t)
	optIn(cfg, "repo")
	if err := os.RemoveAll(linked); err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(cfg, options); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(linked); !os.IsNotExist(err) {
		t.Fatalf("sync recreated a removed worktree: %v", err)
	}
	bare := filepath.Join(filepath.Dir(root), "bare.git")
	gitForTest(t, root, "clone", "--bare", root, bare)
	checkout := filepath.Join(filepath.Dir(root), "bare-checkout")
	gitForTest(t, bare, "worktree", "add", "--detach", checkout)
	project := cfg.Projects["repo"]
	project.Path = checkout
	cfg.Projects["repo"] = project
	if _, err := Sync(cfg, options); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(bare, ".mcp.json")); !os.IsNotExist(err) {
		t.Fatalf("sync wrote to bare repository: %v", err)
	}
}

func TestSyncWorktreesFailuresWriteNothing(t *testing.T) {
	for _, scenario := range []string{"not a repository", "subdirectory", "ambiguous owners", "malformed destination", "missing git"} {
		t.Run(scenario, func(t *testing.T) {
			cfg, options, root, linked := worktreeFixture(t)
			optIn(cfg, "repo")
			want := ""
			switch scenario {
			case "not a repository":
				project := cfg.Projects["repo"]
				project.Path = t.TempDir()
				cfg.Projects["repo"] = project
				want = "git worktree list"
			case "subdirectory":
				project := cfg.Projects["repo"]
				project.Path = filepath.Join(root, "subdirectory")
				mustMkdir(t, project.Path)
				cfg.Projects["repo"] = project
				want = "requires a Git checkout root"
			case "ambiguous owners":
				gitForTest(t, root, "worktree", "add", "--detach", filepath.Join(filepath.Dir(root), "third"))
				cfg.Projects["linked"] = config.Project{Path: linked, MCPs: []string{"remote"}, IncludeWorktrees: true}
				want = "is discovered by projects"
			case "malformed destination":
				mustWrite(t, filepath.Join(linked, ".mcp.json"), []byte("invalid JSON"), 0o600)
				want = ".mcp.json"
			case "missing git":
				t.Setenv("PATH", t.TempDir())
				want = "Git is required"
			}
			if _, err := Sync(cfg, options); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Sync() = %v, want %q", err, want)
			}
			for _, path := range []string{filepath.Join(options.HomeDir, ".codex", "config.toml"), filepath.Join(root, ".mcp.json"), filepath.Join(linked, "opencode.json")} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("failed sync wrote %s: %v", path, err)
				}
			}
		})
	}
}

func TestSyncWorktreesIgnoresInheritedGitDirectory(t *testing.T) {
	cfg, options, root, linked := worktreeFixture(t)
	optIn(cfg, "repo")
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "missing.git"))
	t.Setenv("GIT_WORK_TREE", t.TempDir())
	t.Setenv("GIT_COMMON_DIR", t.TempDir())
	if _, err := Sync(cfg, options); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, linked} {
		assertKeys(t, nestedMap(t, readJSON(t, filepath.Join(path, ".mcp.json")), "mcpServers"), "local")
	}
}

func TestParseWorktreesPreservesUnusualPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spaces\tand\n\"quotes\"")
	bare := filepath.Join(t.TempDir(), "bare.git")
	output := "worktree " + path + "\x00HEAD abc\x00detached\x00locked reason\x00\x00worktree " + bare + "\x00bare\x00\x00"
	got, err := parseWorktrees([]byte(output))
	want := []gitWorktree{{path: path}, {path: bare, bare: true}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("parseWorktrees() = %#v, %v; want %#v", got, err, want)
	}
}
