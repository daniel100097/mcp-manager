package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/daniel100097/mcp-manager/internal/buildinfo"
	"github.com/daniel100097/mcp-manager/internal/config"
	"github.com/daniel100097/mcp-manager/internal/importer"
	"github.com/daniel100097/mcp-manager/internal/syncer"
	"github.com/daniel100097/mcp-manager/internal/wrapper"
)

// execLaunch hands control to the resolved MCP server. Tests replace it to
// observe the launch without leaving the test process.
var execLaunch = wrapper.Exec

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printRootUsage(stderr)
		return 2
	}

	switch args[0] {
	case "sync":
		return runSync(args[1:], stdout, stderr)
	case "import":
		return runImport(args[1:], stdout, stderr)
	case "enable":
		return runScopeCommand(args[1:], stdout, stderr, enableCommand)
	case "disable":
		return runScopeCommand(args[1:], stdout, stderr, disableCommand)
	case "move":
		return runScopeCommand(args[1:], stdout, stderr, moveCommand)
	case "worktrees":
		return runWorktrees(args[1:], stdout, stderr)
	case "stdio":
		return runStdio(args[1:], stderr)
	case "version", "--version", "-version":
		fmt.Fprintln(stdout, buildinfo.String())
		return 0
	case "help", "--help", "-h":
		printRootUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		printRootUsage(stderr)
		return 2
	}
}

// configFlags holds the --config and --config-local options that every
// subcommand accepts.
type configFlags struct {
	path  *string
	local *string
}

func addConfigFlags(flags *flag.FlagSet, defaultConfig string) configFlags {
	return configFlags{
		path: flags.String("config", defaultConfig, "path to the central JSON config"),
		local: flags.String(
			"config-local", "",
			"path to the local override config; defaults to MCP_MANAGER_CONFIG_LOCAL or the --config path with a .local.json suffix",
		),
	}
}

// source resolves the parsed flags into the central config and its overlay.
func (f configFlags) source() (config.Source, error) {
	local := *f.local
	if local == "" {
		var err error
		if local, err = syncer.DefaultLocalConfigPath(*f.path); err != nil {
			return config.Source{}, err
		}
	}
	return config.Source{Path: *f.path, LocalPath: local}, nil
}

// runStdio launches the named stdio MCP from the central config. It never
// writes to stdout because that stream carries the MCP protocol once the
// server starts.
func runStdio(args []string, stderr io.Writer) int {
	defaultConfig, err := syncer.DefaultConfigPath()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	flags := flag.NewFlagSet("stdio", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configFlags := addConfigFlags(flags, defaultConfig)
	flags.Usage = func() {
		fmt.Fprintf(stderr, "Usage: mcp-manager stdio [options] MCP\n\nOptions:\n")
		printLongFlagDefaults(stderr, flags)
	}
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "error: stdio requires exactly one MCP name")
		fmt.Fprintln(stderr)
		flags.Usage()
		return 2
	}

	mcpName := flags.Arg(0)
	source, err := configFlags.source()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	cfg, err := source.Load()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	launch, err := wrapper.Prepare(cfg, mcpName, os.Environ())
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	code, err := execLaunch(launch)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	return code
}

// scopeCommand describes one activation change shared by enable, disable,
// and move: the mutation and the messages that report its outcome. Every
// message takes the MCP name and the project ID.
type scopeCommand struct {
	name      string
	apply     func(cfg *config.Config, mcpName, projectID string) (bool, error)
	unchanged string
	preview   string
	done      string
}

var (
	enableCommand = scopeCommand{
		name:      "enable",
		apply:     enableMCP,
		unchanged: "MCP %q is already enabled for project %q",
		preview:   "would enable MCP %q for project %q",
		done:      "enabled MCP %q for project %q",
	}
	disableCommand = scopeCommand{
		name:      "disable",
		apply:     disableMCP,
		unchanged: "MCP %q is already disabled for project %q",
		preview:   "would disable MCP %q for project %q",
		done:      "disabled MCP %q for project %q",
	}
	moveCommand = scopeCommand{
		name:      "move",
		apply:     moveMCP,
		unchanged: "MCP %q is already local to project %q",
		preview:   "would move MCP %q from global scope to project %q",
		done:      "moved MCP %q from global scope to project %q",
	}
)

// runScopeCommand applies an activation change and synchronizes the generated
// outputs. The change is written to the local override config when that file
// exists and to the central config otherwise.
func runScopeCommand(args []string, stdout, stderr io.Writer, command scopeCommand) int {
	defaultConfig, err := syncer.DefaultConfigPath()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	flags := flag.NewFlagSet(command.name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configFlags := addConfigFlags(flags, defaultConfig)
	dryRun := flags.Bool("dry-run", false, "show all changes without writing files")
	flags.Usage = func() {
		fmt.Fprintf(stderr, "Usage: mcp-manager %s [options] MCP PROJECT\n\nOptions:\n", command.name)
		printLongFlagDefaults(stderr, flags)
	}
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 2 {
		fmt.Fprintf(stderr, "error: %s requires an MCP name and a project ID\n\n", command.name)
		flags.Usage()
		return 2
	}

	mcpName := flags.Arg(0)
	projectID := flags.Arg(1)
	source, err := configFlags.source()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	edit, err := source.OpenEdit()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	changed, err := command.apply(edit.Config, mcpName, projectID)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	// Validate the complete result before anything is written.
	effective, err := edit.Effective()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	switch {
	case !changed:
		fmt.Fprintf(stdout, command.unchanged+"; %s unchanged\n", mcpName, projectID, edit.Label())
	case *dryRun:
		fmt.Fprintf(stdout, command.preview+"\n", mcpName, projectID)
		fmt.Fprintf(stdout, "dry run complete: %s would change: %s\n", edit.Label(), edit.Target())
	default:
		if err := edit.Save(); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, command.done+"\n", mcpName, projectID)
		fmt.Fprintf(stdout, "%s updated: %s\n", edit.Label(), edit.Target())
	}
	return syncConfig(effective, source, false, *dryRun, stdout, stderr)
}

func disableMCP(cfg *config.Config, mcpName, projectID string) (bool, error) {
	if cfg == nil {
		return false, errors.New("config must not be nil")
	}
	if _, exists := cfg.MCPs[mcpName]; !exists {
		return false, fmt.Errorf("MCP %q does not exist", mcpName)
	}
	project, exists := cfg.Projects[projectID]
	if !exists {
		return false, fmt.Errorf("project %q is not registered", projectID)
	}
	if stringIndex(cfg.Global.MCPs, mcpName) >= 0 {
		return false, fmt.Errorf("MCP %q is globally active and cannot be disabled for only one project", mcpName)
	}

	mcpIndex := stringIndex(project.MCPs, mcpName)
	if mcpIndex == -1 {
		return false, nil
	}

	project.MCPs = append(project.MCPs[:mcpIndex], project.MCPs[mcpIndex+1:]...)
	delete(project.DisabledAgents, mcpName)
	cfg.Projects[projectID] = project
	return true, nil
}

func enableMCP(cfg *config.Config, mcpName, projectID string) (bool, error) {
	if cfg == nil {
		return false, errors.New("config must not be nil")
	}
	if _, exists := cfg.MCPs[mcpName]; !exists {
		return false, fmt.Errorf("MCP %q does not exist", mcpName)
	}
	project, exists := cfg.Projects[projectID]
	if !exists {
		return false, fmt.Errorf("project %q is not registered", projectID)
	}
	if stringIndex(cfg.Global.MCPs, mcpName) >= 0 {
		return false, fmt.Errorf("MCP %q is globally active; use move to make it project-only", mcpName)
	}
	if stringIndex(project.MCPs, mcpName) >= 0 {
		return false, nil
	}

	project.MCPs = append(project.MCPs, mcpName)
	cfg.Projects[projectID] = project
	return true, nil
}

func moveMCP(cfg *config.Config, mcpName, projectID string) (bool, error) {
	if cfg == nil {
		return false, errors.New("config must not be nil")
	}
	if _, exists := cfg.MCPs[mcpName]; !exists {
		return false, fmt.Errorf("MCP %q does not exist", mcpName)
	}
	project, exists := cfg.Projects[projectID]
	if !exists {
		return false, fmt.Errorf("project %q is not registered", projectID)
	}

	globalIndex := stringIndex(cfg.Global.MCPs, mcpName)
	alreadyAssigned := stringIndex(project.MCPs, mcpName) >= 0
	if globalIndex == -1 {
		if alreadyAssigned {
			return false, nil
		}
		return false, fmt.Errorf("MCP %q is not globally active", mcpName)
	}

	globalDisabledAgents := cfg.Global.DisabledAgents[mcpName]
	cfg.Global.MCPs = append(cfg.Global.MCPs[:globalIndex], cfg.Global.MCPs[globalIndex+1:]...)
	delete(cfg.Global.DisabledAgents, mcpName)
	if !alreadyAssigned {
		project.MCPs = append(project.MCPs, mcpName)
	}
	if len(globalDisabledAgents) > 0 {
		if project.DisabledAgents == nil {
			project.DisabledAgents = map[string][]config.Agent{}
		}
		if _, exists := project.DisabledAgents[mcpName]; !exists {
			project.DisabledAgents[mcpName] = append([]config.Agent(nil), globalDisabledAgents...)
		}
	}
	cfg.Projects[projectID] = project
	return true, nil
}

func stringIndex(values []string, wanted string) int {
	for index, value := range values {
		if value == wanted {
			return index
		}
	}
	return -1
}

func runImport(args []string, stdout, stderr io.Writer) int {
	defaultConfig, err := syncer.DefaultConfigPath()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	flags := flag.NewFlagSet("import", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configFlags := addConfigFlags(flags, defaultConfig)
	from := flags.String("from", "", "source agent: codex, claude, or opencode (required)")
	project := flags.String("project", "", "project scope as ID=/absolute/path")
	dryRun := flags.Bool("dry-run", false, "show all changes without writing files")
	flags.Usage = func() {
		fmt.Fprintf(stderr, "Usage: mcp-manager import --from AGENT [options]\n\nOptions:\n")
		printLongFlagDefaults(stderr, flags)
	}
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "error: unexpected argument %q\n\n", flags.Arg(0))
		flags.Usage()
		return 2
	}
	agent, err := parseAgent(*from)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n\n", err)
		flags.Usage()
		return 2
	}

	source, err := configFlags.source()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	edit, err := source.OpenEdit()
	centralExists := true
	if errors.Is(err, os.ErrNotExist) {
		centralExists = false
		edit = source.NewEdit(&config.Config{
			Version: config.CurrentVersion,
			Global: config.Scope{
				MCPs:           []string{},
				DisabledAgents: map[string][]config.Agent{},
			},
			Projects: map[string]config.Project{},
			MCPs:     map[string]config.MCP{},
		})
	} else if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	options, err := importer.DefaultOptions()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	options.Agent = agent
	if *project != "" {
		id, path, found := strings.Cut(*project, "=")
		if !found || id == "" || path == "" {
			fmt.Fprintln(stderr, "error: --project must use ID=/absolute/path")
			return 2
		}
		options.ProjectID = id
		options.ProjectPath = path
	}

	merged, result, err := importer.Import(edit.Config, options)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	edit.Config = merged
	changed := !centralExists || result.Added > 0 || result.Activated > 0 || result.ProjectRegistered
	// Validate the complete result before anything is written.
	effective, err := edit.Effective()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if changed && !*dryRun {
		if err := edit.Save(); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
	}

	verb := "imported"
	if *dryRun {
		verb = "would import"
	}
	fmt.Fprintf(
		stdout,
		"%s %d MCPs from %s %s (%d added, %d activated, %d unchanged, %d disabled skipped): %s\n",
		verb, result.Imported, result.Agent, result.Scope,
		result.Added, result.Activated, result.Unchanged, result.Skipped, result.SourcePath,
	)
	if !changed {
		fmt.Fprintf(stdout, "%s already contains these MCPs\n", edit.Label())
	} else if *dryRun {
		fmt.Fprintf(stdout, "dry run complete: %s would change: %s\n", edit.Label(), edit.Target())
	} else {
		fmt.Fprintf(stdout, "%s updated: %s\n", edit.Label(), edit.Target())
	}
	return syncConfig(effective, source, false, *dryRun, stdout, stderr)
}

func parseAgent(value string) (config.Agent, error) {
	agent := config.Agent(value)
	switch agent {
	case config.AgentCodex, config.AgentClaude, config.AgentOpenCode:
		return agent, nil
	default:
		return "", fmt.Errorf("--from must be one of codex, claude, or opencode; got %q", value)
	}
}

func runSync(args []string, stdout, stderr io.Writer) int {
	defaultConfig, err := syncer.DefaultConfigPath()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	flags := flag.NewFlagSet("sync", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configFlags := addConfigFlags(flags, defaultConfig)
	inlineSecrets := flags.Bool("inline-secrets", false, "resolve env references and store their values inline")
	dryRun := flags.Bool("dry-run", false, "validate and show changes without writing files")
	flags.Usage = func() {
		fmt.Fprintf(stderr, "Usage: mcp-manager sync [options]\n\nOptions:\n")
		printLongFlagDefaults(stderr, flags)
	}
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "error: unexpected argument %q\n\n", flags.Arg(0))
		flags.Usage()
		return 2
	}

	source, err := configFlags.source()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	cfg, err := source.Load()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	return syncConfig(cfg, source, *inlineSecrets, *dryRun, stdout, stderr)
}

func syncConfig(cfg *config.Config, source config.Source, inlineSecrets, dryRun bool, stdout, stderr io.Writer) int {
	options, err := syncer.DefaultOptions()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	options.ConfigPath = source.Path
	options.LocalConfigPath = source.LocalPath
	options.InlineSecrets = cfg.Options.InlineSecrets || inlineSecrets
	options.DryRun = dryRun
	if options.InlineSecrets {
		fmt.Fprintln(stderr, "warning: inline secrets are enabled; resolved values will be stored in generated config files")
	}

	result, err := syncer.Sync(cfg, options)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	verb := "updated"
	if options.DryRun {
		verb = "would update"
	}
	for _, change := range result.Changes {
		fmt.Fprintf(stdout, "%s %s %s (%d MCPs): %s\n", verb, change.Agent, change.Scope, change.ServerCount, change.Path)
	}
	if len(result.Changes) == 0 {
		fmt.Fprintf(stdout, "already in sync (%d targets checked)\n", result.Unchanged)
		return 0
	}
	if options.DryRun {
		fmt.Fprintf(stdout, "dry run complete: %d files would change\n", len(result.Changes))
	} else {
		fmt.Fprintf(stdout, "sync complete: %d files changed\n", len(result.Changes))
	}
	return 0
}

func printLongFlagDefaults(output io.Writer, flags *flag.FlagSet) {
	placeholders := map[string]string{
		"config":       "PATH",
		"config-local": "PATH",
		"from":         "AGENT",
		"project":      "ID=PATH",
	}
	flags.VisitAll(func(option *flag.Flag) {
		argument := ""
		if placeholder := placeholders[option.Name]; placeholder != "" {
			argument = " " + placeholder
		}
		fmt.Fprintf(output, "  --%s%s\n      %s", option.Name, argument, option.Usage)
		if option.DefValue != "" && option.DefValue != "false" {
			fmt.Fprintf(output, " (default %q)", option.DefValue)
		}
		fmt.Fprintln(output)
	})
}

func printRootUsage(output io.Writer) {
	fmt.Fprintln(output, `mcp-manager keeps Codex, Claude Code, and OpenCode MCP configs in sync.

Usage:
  mcp-manager sync [--config PATH] [--inline-secrets] [--dry-run]
  mcp-manager import --from AGENT [--project ID=PATH] [--config PATH] [--dry-run]
  mcp-manager enable [--config PATH] [--dry-run] MCP PROJECT
  mcp-manager disable [--config PATH] [--dry-run] MCP PROJECT
  mcp-manager move [--config PATH] [--dry-run] MCP PROJECT
  mcp-manager worktrees enable [--config PATH] [--dry-run] PROJECT
  mcp-manager worktrees disable [--config PATH] [--dry-run] PROJECT
  mcp-manager stdio [--config PATH] MCP
  mcp-manager version
  mcp-manager help

Every command also accepts --config-local PATH to select the local override
file, which defaults to the --config path with a .local.json suffix. When that
file exists, it is applied on top of the central config and receives the
changes made by import, enable, disable, move, and worktrees.

Generated agent configs launch stdio MCPs through "mcp-manager stdio MCP", so
the mcp-manager binary must be on the PATH that Codex, Claude Code, and
OpenCode use.`)
}
