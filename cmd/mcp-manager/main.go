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
		return runEnable(args[1:], stdout, stderr)
	case "disable":
		return runDisable(args[1:], stdout, stderr)
	case "move":
		return runMove(args[1:], stdout, stderr)
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
	configPath := flags.String("config", defaultConfig, "path to the central JSON config")
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
	cfg, err := config.Load(*configPath)
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

func runDisable(args []string, stdout, stderr io.Writer) int {
	defaultConfig, err := syncer.DefaultConfigPath()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	flags := flag.NewFlagSet("disable", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfig, "path to the central JSON config")
	dryRun := flags.Bool("dry-run", false, "show all changes without writing files")
	flags.Usage = func() {
		fmt.Fprintf(stderr, "Usage: mcp-manager disable [options] MCP PROJECT\n\nOptions:\n")
		printLongFlagDefaults(stderr, flags)
	}
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 2 {
		fmt.Fprintln(stderr, "error: disable requires an MCP name and a project ID")
		fmt.Fprintln(stderr)
		flags.Usage()
		return 2
	}

	mcpName := flags.Arg(0)
	projectID := flags.Arg(1)
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	changed, err := disableMCP(cfg, mcpName, projectID)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	if !changed {
		fmt.Fprintf(stdout, "MCP %q is already disabled for project %q; central config unchanged\n", mcpName, projectID)
		return syncConfig(cfg, *configPath, false, *dryRun, stdout, stderr)
	}
	if *dryRun {
		fmt.Fprintf(stdout, "would disable MCP %q for project %q\n", mcpName, projectID)
		fmt.Fprintf(stdout, "dry run complete: central config would change: %s\n", *configPath)
		return syncConfig(cfg, *configPath, false, true, stdout, stderr)
	}
	if err := config.Save(*configPath, cfg); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "disabled MCP %q for project %q\n", mcpName, projectID)
	fmt.Fprintf(stdout, "central config updated: %s\n", *configPath)
	return syncConfig(cfg, *configPath, false, false, stdout, stderr)
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

func runEnable(args []string, stdout, stderr io.Writer) int {
	defaultConfig, err := syncer.DefaultConfigPath()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	flags := flag.NewFlagSet("enable", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfig, "path to the central JSON config")
	dryRun := flags.Bool("dry-run", false, "show all changes without writing files")
	flags.Usage = func() {
		fmt.Fprintf(stderr, "Usage: mcp-manager enable [options] MCP PROJECT\n\nOptions:\n")
		printLongFlagDefaults(stderr, flags)
	}
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 2 {
		fmt.Fprintln(stderr, "error: enable requires an MCP name and a project ID")
		fmt.Fprintln(stderr)
		flags.Usage()
		return 2
	}

	mcpName := flags.Arg(0)
	projectID := flags.Arg(1)
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	changed, err := enableMCP(cfg, mcpName, projectID)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	if !changed {
		fmt.Fprintf(stdout, "MCP %q is already enabled for project %q; central config unchanged\n", mcpName, projectID)
		return syncConfig(cfg, *configPath, false, *dryRun, stdout, stderr)
	}
	if *dryRun {
		fmt.Fprintf(stdout, "would enable MCP %q for project %q\n", mcpName, projectID)
		fmt.Fprintf(stdout, "dry run complete: central config would change: %s\n", *configPath)
		return syncConfig(cfg, *configPath, false, true, stdout, stderr)
	}
	if err := config.Save(*configPath, cfg); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "enabled MCP %q for project %q\n", mcpName, projectID)
	fmt.Fprintf(stdout, "central config updated: %s\n", *configPath)
	return syncConfig(cfg, *configPath, false, false, stdout, stderr)
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

func runMove(args []string, stdout, stderr io.Writer) int {
	defaultConfig, err := syncer.DefaultConfigPath()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	flags := flag.NewFlagSet("move", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfig, "path to the central JSON config")
	dryRun := flags.Bool("dry-run", false, "show all changes without writing files")
	flags.Usage = func() {
		fmt.Fprintf(stderr, "Usage: mcp-manager move [options] MCP PROJECT\n\nOptions:\n")
		printLongFlagDefaults(stderr, flags)
	}
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 2 {
		fmt.Fprintln(stderr, "error: move requires an MCP name and a project ID")
		fmt.Fprintln(stderr)
		flags.Usage()
		return 2
	}

	mcpName := flags.Arg(0)
	projectID := flags.Arg(1)
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	changed, err := moveMCP(cfg, mcpName, projectID)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	if !changed {
		fmt.Fprintf(stdout, "MCP %q is already local to project %q; central config unchanged\n", mcpName, projectID)
		return syncConfig(cfg, *configPath, false, *dryRun, stdout, stderr)
	}
	if *dryRun {
		fmt.Fprintf(stdout, "would move MCP %q from global scope to project %q\n", mcpName, projectID)
		fmt.Fprintf(stdout, "dry run complete: central config would change: %s\n", *configPath)
		return syncConfig(cfg, *configPath, false, true, stdout, stderr)
	}
	if err := config.Save(*configPath, cfg); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "moved MCP %q from global scope to project %q\n", mcpName, projectID)
	fmt.Fprintf(stdout, "central config updated: %s\n", *configPath)
	return syncConfig(cfg, *configPath, false, false, stdout, stderr)
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
	configPath := flags.String("config", defaultConfig, "path to the central JSON config")
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

	existing, err := config.Load(*configPath)
	centralExists := true
	if errors.Is(err, os.ErrNotExist) {
		centralExists = false
		existing = &config.Config{
			Version: config.CurrentVersion,
			Global: config.Scope{
				MCPs:           []string{},
				DisabledAgents: map[string][]config.Agent{},
			},
			Projects: map[string]config.Project{},
			MCPs:     map[string]config.MCP{},
		}
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

	merged, result, err := importer.Import(existing, options)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	changed := !centralExists || result.Added > 0 || result.Activated > 0 || result.ProjectRegistered
	if changed && !*dryRun {
		if err := config.Save(*configPath, merged); err != nil {
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
		fmt.Fprintln(stdout, "central config already contains these MCPs")
	} else if *dryRun {
		fmt.Fprintf(stdout, "dry run complete: central config would change: %s\n", *configPath)
	} else {
		fmt.Fprintf(stdout, "central config updated: %s\n", *configPath)
	}
	return syncConfig(merged, *configPath, false, *dryRun, stdout, stderr)
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
	configPath := flags.String("config", defaultConfig, "path to the central JSON config")
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

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	return syncConfig(cfg, *configPath, *inlineSecrets, *dryRun, stdout, stderr)
}

func syncConfig(cfg *config.Config, configPath string, inlineSecrets, dryRun bool, stdout, stderr io.Writer) int {
	options, err := syncer.DefaultOptions()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	options.ConfigPath = configPath
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
		"config":  "PATH",
		"from":    "AGENT",
		"project": "ID=PATH",
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
  mcp-manager stdio [--config PATH] MCP
  mcp-manager version
  mcp-manager help

Generated agent configs launch stdio MCPs through "mcp-manager stdio MCP", so
the mcp-manager binary must be on the PATH that Codex, Claude Code, and
OpenCode use.`)
}
