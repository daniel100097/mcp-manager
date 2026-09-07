package main

import (
	"bytes"
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
	// Common options may precede the command as well as follow it.
	var common []string
	for len(args) > 0 {
		name, _, inline := strings.Cut(args[0], "=")
		if name != "--config" && name != "--config-local" && name != "--dry-run" {
			break
		}
		common = append(common, args[0])
		args = args[1:]
		if !inline && name != "--dry-run" {
			if len(args) == 0 {
				fmt.Fprintf(stderr, "error: %s requires a path\n", name)
				return 2
			}
			common = append(common, args[0])
			args = args[1:]
		}
	}
	if len(args) == 0 {
		printRootUsage(stderr)
		return 2
	}
	if len(common) > 0 {
		at := 1
		if (args[0] == "project" || args[0] == "projects" || args[0] == "worktrees" || args[0] == "mcp") && len(args) > 1 {
			at = 2
		}
		forwarded := append([]string{}, args[:at]...)
		forwarded = append(forwarded, common...)
		args = append(forwarded, args[at:]...)
	}

	switch args[0] {
	case "project", "projects":
		return runProject(args[1:], stdout, stderr)
	case "add":
		return runMCPAdd(args[1:], stdout, stderr)
	case "remove", "rm":
		return runMCPRemove(args[1:], stdout, stderr)
	case "list", "ls":
		return runMCPList(args[1:], stdout, stderr)
	case "show":
		return runMCPShow(args[1:], stdout, stderr)
	case "mcp":
		return run(args[1:], stdout, stderr)
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
	if err := parseFlags(flags, args); err != nil {
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
	projectOption := flags.String("project", "", "project ID or path; defaults to the current directory")
	global := flags.Bool("global", false, "use global scope; move transfers the selected project's assignment")
	local := flags.Bool("local", false, "use project scope (default)")
	agent := flags.String("agent", "", "enable or disable only this agent: codex, claude, or opencode")
	flags.Usage = func() {
		fmt.Fprintf(stderr, "Usage: mcp-manager %s MCP [--project ID|PATH] [--global|--local] [options]\n\nThe current directory selects the project. Legacy MCP PROJECT is also accepted.\n\nOptions:\n", command.name)
		printLongFlagDefaults(stderr, flags)
	}
	if err := parseFlags(flags, args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() < 1 || flags.NArg() > 2 {
		fmt.Fprintf(stderr, "error: %s requires an MCP name and at most one project\n\n", command.name)
		flags.Usage()
		return 2
	}
	if *global && *local {
		fmt.Fprintln(stderr, "error: --global and --local cannot be combined")
		return 2
	}
	selector, err := selectedProject(flags.Arg(1), *projectOption)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	if *global && command.name != "move" && selector != "" {
		fmt.Fprintln(stderr, "error: --global cannot be combined with a project selector")
		return 2
	}
	if *agent != "" && command.name == "move" {
		fmt.Fprintln(stderr, "error: --agent is only supported by enable and disable")
		return 2
	}
	if *agent != "" && *agent != "codex" && *agent != "claude" && *agent != "opencode" {
		fmt.Fprintln(stderr, "error: --agent must be codex, claude, or opencode")
		return 2
	}
	source, err := configFlags.source()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	edit, err := openConfigEdit(source, false)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	mcpName := flags.Arg(0)
	if _, exists := edit.Config.MCPs[mcpName]; !exists {
		fmt.Fprintf(stderr, "error: MCP %q does not exist; use 'mcp-manager list --all' to list definitions\n", mcpName)
		return 1
	}
	projectID := ""
	if !*global || command.name == "move" {
		projectID, err = resolveProject(edit.Config, selector)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
	}
	var changed bool
	description := fmt.Sprintf("%s MCP %q for project %q", command.name, mcpName, projectID)
	switch {
	case command.name == "move" && *global:
		changed, err = moveMCPGlobal(edit.Config, mcpName, projectID)
		description = fmt.Sprintf("move MCP %q from project %q to global scope", mcpName, projectID)
	case *global || *agent != "":
		changed, err = toggleMCPScope(edit.Config, mcpName, projectID, *global, command.name == "enable", config.Agent(*agent))
		if *global {
			description = fmt.Sprintf("%s MCP %q in global scope", command.name, mcpName)
		}
		if *agent != "" {
			description += fmt.Sprintf(" for %s", *agent)
		}
	default:
		changed, err = command.apply(edit.Config, mcpName, projectID)
	}
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	code := saveAndSync(edit, source, changed, *dryRun, description, stdout, stderr)
	if code == 0 && !*global && command.name != "move" && stringIndex(edit.Config.Global.MCPs, mcpName) >= 0 {
		fmt.Fprintf(stdout, "Global assignment remains active. To restrict MCP %q to this project, use 'mcp-manager move %s --local'.\n", mcpName, mcpName)
	}
	return code
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
	if !alreadyAssigned && len(globalDisabledAgents) > 0 {
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
	project := flags.String("project", "", "project ID or path, or ID=PATH to register and import")
	local := flags.Bool("local", false, "import the current project's configuration")
	global := flags.Bool("global", false, "import the global configuration (default)")
	dryRun := flags.Bool("dry-run", false, "show all changes without writing files")
	flags.Usage = func() {
		fmt.Fprintf(stderr, "Usage: mcp-manager import --from AGENT [options]\n\nOptions:\n")
		printLongFlagDefaults(stderr, flags)
	}
	if err := parseFlags(flags, args); err != nil {
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
	if *global && (*local || *project != "") {
		fmt.Fprintln(stderr, "error: --global cannot be combined with --local or --project")
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
	edit, err := openConfigEdit(source, true)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	centralExists := !edit.NeedsInitialization()

	options, err := importer.DefaultOptions()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	options.Agent = agent
	if strings.Contains(*project, "=") {
		id, path, found := strings.Cut(*project, "=")
		if !found || id == "" || path == "" {
			fmt.Fprintln(stderr, "error: --project must use ID=/absolute/path")
			return 2
		}
		options.ProjectID = id
		options.ProjectPath = path
	} else if *local || *project != "" {
		id, err := resolveProject(edit.Config, *project)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		options.ProjectID = id
		options.ProjectPath = edit.Config.Projects[id].Path
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
	var diagnostics bytes.Buffer
	if code := syncConfig(effective, source, false, true, io.Discard, &diagnostics); code != 0 {
		fmt.Fprint(stderr, diagnostics.String())
		return code
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
	if err := parseFlags(flags, args); err != nil {
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
		"config":         "PATH",
		"config-local":   "PATH",
		"from":           "AGENT",
		"project":        "ID|PATH",
		"name":           "ID",
		"url":            "URL",
		"agent":          "AGENT",
		"env":            "KEY=VALUE",
		"env-from":       "VARIABLE",
		"header":         "KEY=VALUE",
		"header-from":    "KEY=VARIABLE",
		"disabled-agent": "AGENT",
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
	fmt.Fprintln(output, `Manage MCPs for Codex, Claude Code, and OpenCode.

Get started in your project directory:
  mcp-manager project add
  mcp-manager add docs --url https://example.com/mcp
  mcp-manager add tools -- npx -y @example/mcp-server
  mcp-manager list
  mcp-manager worktrees enable

MCPs:
  add NAME --url URL | -- COMMAND [ARGS...]   Define and enable an MCP
  add NAME --replace ...                    Replace a definition
  list [--all|--global]                     List MCPs and their scopes
  show NAME                                Inspect; env/header literals redacted
  enable NAME [--global]                    Activate a saved MCP
  disable NAME [--global]                   Deactivate; keep its definition
  move NAME --global                       Move from this project to global
  move NAME --local                        Move from global to this project
  remove NAME                              Delete definition and all assignments

Projects:
  project add [PATH] [--name ID]            Register a directory (default: here)
  project list                             List registered projects
  project show [ID|PATH]                    Show project settings (default: here)
  project remove [ID|PATH]                  Unregister; retain generated files
  worktrees enable|disable                 Toggle worktree discovery for here

Other commands:
  sync                                     Regenerate all managed agent configs
  import --from AGENT [--local|--global]     Import native config (default: global)
  stdio NAME                               Launch a saved stdio server
  version                                  Print the installed version

Local commands infer the project from your current directory, including
subdirectories and opted-in worktrees. Use --project ID|PATH to select another.
The legacy "enable|disable|move NAME PROJECT" forms still work.
Use enable/disable --agent AGENT to change one agent's activation in a scope.

Options may precede or follow arguments. Use -- before a server command.
--dry-run previews changes; --config PATH and --config-local PATH select configs.
Existing local override files receive edits. Project registration only changes
the registry; MCP and worktree changes automatically sync agent configs.
Run any command with --help for its options.`)
}
