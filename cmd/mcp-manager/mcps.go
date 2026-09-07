package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/daniel100097/mcp-manager/internal/config"
	"github.com/daniel100097/mcp-manager/internal/syncer"
)

type mcpStringFlags []string

func (values *mcpStringFlags) String() string { return strings.Join(*values, ", ") }

func (values *mcpStringFlags) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func runMCPAdd(args []string, stdout, stderr io.Writer) int {
	defaultConfig, err := syncer.DefaultConfigPath()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	flags := flag.NewFlagSet("add", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configFlags := addConfigFlags(flags, defaultConfig)
	projectSelector := flags.String("project", "", "project ID or path; defaults to the current directory")
	global := flags.Bool("global", false, "activate the MCP globally")
	local := flags.Bool("local", false, "activate the MCP for the current project (default)")
	url := flags.String("url", "", "HTTP MCP server URL")
	replace := flags.Bool("replace", false, "replace an existing definition, preserving its other scope settings")
	dryRun := flags.Bool("dry-run", false, "show all changes without writing files")
	var env, envFrom, headers, headersFrom, disabledAgents mcpStringFlags
	flags.Var(&env, "env", "stdio environment KEY=VALUE (repeatable)")
	flags.Var(&envFrom, "env-from", "stdio environment variable to read at launch (repeatable)")
	flags.Var(&headers, "header", "HTTP header NAME=VALUE (repeatable)")
	flags.Var(&headersFrom, "header-from", "HTTP header NAME=ENV_VAR read from the environment (repeatable)")
	flags.Var(&disabledAgents, "disabled-agent", "agent to exclude in this scope: codex, claude, or opencode (repeatable)")
	flags.Usage = func() {
		fmt.Fprintln(stderr, `Usage: mcp-manager add NAME [options] -- COMMAND [ARGS...]
       mcp-manager add NAME --url URL [options]

Adds a definition and activates it for the current registered project.
Use --global to make it available everywhere. Run project add first to
register the current folder. Arguments after -- are passed to the server.

Examples:
  mcp-manager add files -- npx -y @modelcontextprotocol/server-filesystem .
  mcp-manager add api --global --url https://example.com/mcp
  mcp-manager add api --replace --url https://example.com/new-mcp

Options:`)
		printLongFlagDefaults(stderr, flags)
	}
	// Split before parsing manager flags so server arguments retain their exact
	// order and values, including flags that the manager also recognizes.
	var serverArgs []string
	hasSeparator := false
	for i, argument := range args {
		if argument == "--" {
			hasSeparator = true
			serverArgs = args[i+1:]
			args = args[:i]
			break
		}
	}
	if err := parseFlags(flags, args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "error: add requires one MCP name; put the server command after --")
		flags.Usage()
		return 2
	}
	if *global && (*local || *projectSelector != "") {
		fmt.Fprintln(stderr, "error: --global cannot be combined with --local or --project")
		return 2
	}
	if *url != "" && hasSeparator {
		fmt.Fprintln(stderr, "error: choose --url for HTTP or -- COMMAND for stdio")
		return 2
	}
	if *url == "" && len(serverArgs) == 0 {
		fmt.Fprintln(stderr, "error: provide --url URL or a server command after --")
		return 2
	}
	mcp := config.MCP{Type: "http", URL: *url, EnvFrom: envFrom}
	if len(serverArgs) > 0 {
		mcp.Type = "stdio"
		mcp.Command = serverArgs[0]
		mcp.Args = append([]string(nil), serverArgs[1:]...)
	}
	if mcp.Env, err = mcpAssignments("--env", env); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	if mcp.Headers, err = mcpAssignments("--header", headers); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	if mcp.HeadersFrom, err = mcpAssignments("--header-from", headersFrom); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	source, err := configFlags.source()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	edit, err := openConfigEdit(source, *global)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	name := flags.Arg(0)
	_, exists := edit.Config.MCPs[name]
	if exists && !*replace {
		fmt.Fprintf(stderr, "error: MCP %q already exists; use --replace to update its definition, or mcp-manager enable to activate it\n", name)
		return 1
	}
	scope := "global scope"
	projectID := ""
	if !*global {
		projectID, err = resolveProject(edit.Config, *projectSelector)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		scope = fmt.Sprintf("project %q", projectID)
	}
	edit.Config.MCPs[name] = mcp
	agents := make([]config.Agent, len(disabledAgents))
	for i, agent := range disabledAgents {
		agents[i] = config.Agent(agent)
	}
	if *global {
		if stringIndex(edit.Config.Global.MCPs, name) < 0 {
			edit.Config.Global.MCPs = append(edit.Config.Global.MCPs, name)
		}
		if len(agents) > 0 {
			if edit.Config.Global.DisabledAgents == nil {
				edit.Config.Global.DisabledAgents = map[string][]config.Agent{}
			}
			edit.Config.Global.DisabledAgents[name] = agents
		}
	} else {
		project := edit.Config.Projects[projectID]
		if stringIndex(project.MCPs, name) < 0 {
			project.MCPs = append(project.MCPs, name)
		}
		if len(agents) > 0 {
			if project.DisabledAgents == nil {
				project.DisabledAgents = map[string][]config.Agent{}
			}
			project.DisabledAgents[name] = agents
		}
		edit.Config.Projects[projectID] = project
	}
	action := "add"
	if exists {
		action = "replace"
	}
	return saveAndSync(edit, source, true, *dryRun, fmt.Sprintf("%s MCP %q in %s", action, name, scope), stdout, stderr)
}

func mcpAssignments(option string, values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	result := make(map[string]string, len(values))
	for _, value := range values {
		key, assignment, ok := strings.Cut(value, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("%s requires NAME=VALUE", option)
		}
		if _, duplicate := result[key]; duplicate {
			return nil, fmt.Errorf("%s repeats name %q", option, key)
		}
		result[key] = assignment
	}
	return result, nil
}

func runMCPRemove(args []string, stdout, stderr io.Writer) int {
	defaultConfig, err := syncer.DefaultConfigPath()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	flags := flag.NewFlagSet("remove", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configFlags := addConfigFlags(flags, defaultConfig)
	dryRun := flags.Bool("dry-run", false, "show all changes without writing files")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: mcp-manager remove NAME [options]\n\nDeletes the definition and removes its activations from every scope.\nUse mcp-manager disable NAME to keep the definition and disable it in one scope.\n\nOptions:")
		printLongFlagDefaults(stderr, flags)
	}
	if err := parseFlags(flags, args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "error: remove requires exactly one MCP name")
		flags.Usage()
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
	name := flags.Arg(0)
	if _, exists := edit.Config.MCPs[name]; !exists {
		fmt.Fprintf(stderr, "error: MCP %q does not exist\n", name)
		return 1
	}
	delete(edit.Config.MCPs, name)
	edit.Config.Global.MCPs = removeMCPName(edit.Config.Global.MCPs, name)
	delete(edit.Config.Global.DisabledAgents, name)
	for id, project := range edit.Config.Projects {
		project.MCPs = removeMCPName(project.MCPs, name)
		delete(project.DisabledAgents, name)
		edit.Config.Projects[id] = project
	}
	return saveAndSync(edit, source, true, *dryRun, fmt.Sprintf("remove MCP %q from all scopes", name), stdout, stderr)
}

func removeMCPName(names []string, name string) []string {
	if index := stringIndex(names, name); index >= 0 {
		return append(names[:index], names[index+1:]...)
	}
	return names
}

func runMCPList(args []string, stdout, stderr io.Writer) int {
	return runMCPRead("list", args, stdout, stderr)
}

func runMCPShow(args []string, stdout, stderr io.Writer) int {
	return runMCPRead("show", args, stdout, stderr)
}

func runMCPRead(action string, args []string, stdout, stderr io.Writer) int {
	defaultConfig, err := syncer.DefaultConfigPath()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	flags := flag.NewFlagSet(action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configFlags := addConfigFlags(flags, defaultConfig)
	var all, global, local bool
	var projectSelector string
	if action == "list" {
		flags.BoolVar(&all, "all", false, "list all definitions and scopes (default)")
		flags.BoolVar(&global, "global", false, "list globally active MCPs")
		flags.BoolVar(&local, "local", false, "list global MCPs and MCPs active for the current project")
		flags.StringVar(&projectSelector, "project", "", "list global MCPs and MCPs active for this project ID or path")
	}
	flags.Usage = func() {
		if action == "show" {
			fmt.Fprintln(stderr, "Usage: mcp-manager show NAME [options]\n\nShows a definition and its scopes. Literal environment and header values are redacted.\n\nOptions:")
		} else {
			fmt.Fprintln(stderr, "Usage: mcp-manager list [--all | --global | --local | --project ID|PATH] [options]\n\nLists all definitions and their activation scopes by default.\nUse --local to see the MCPs available in the current registered project.\n\nOptions:")
		}
		printLongFlagDefaults(stderr, flags)
	}
	if err := parseFlags(flags, args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if (action == "list" && flags.NArg() != 0) || (action == "show" && flags.NArg() != 1) {
		if action == "show" {
			fmt.Fprintln(stderr, "error: show requires exactly one MCP name")
		} else {
			fmt.Fprintln(stderr, "error: list does not accept positional arguments")
		}
		flags.Usage()
		return 2
	}
	projectFilter := local || projectSelector != ""
	if (all && (global || projectFilter)) || (global && projectFilter) {
		fmt.Fprintln(stderr, "error: choose one list filter: --all, --global, or --local/--project")
		return 2
	}
	source, err := configFlags.source()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	// Opening for reading skips filesystem validation of other projects and
	// does not resolve environment references or start a server.
	edit, err := openConfigEdit(source, false)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	cfg := edit.Config
	if action == "show" {
		name := flags.Arg(0)
		mcp, exists := cfg.MCPs[name]
		if !exists {
			fmt.Fprintf(stderr, "error: MCP %q does not exist\n", name)
			return 1
		}
		mcp.Env = redactedMCPValues(mcp.Env)
		mcp.Headers = redactedMCPValues(mcp.Headers)
		fmt.Fprintf(stdout, "MCP: %s\nScopes: %s\n", name, strings.Join(mcpScopes(cfg, name), ", "))
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(mcp); err != nil {
			fmt.Fprintf(stderr, "error: write MCP details: %v\n", err)
			return 1
		}
		return 0
	}
	projectID := ""
	if projectFilter {
		projectID, err = resolveProject(cfg, projectSelector)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
	}
	names := make([]string, 0, len(cfg.MCPs))
	for name := range cfg.MCPs {
		globallyActive := stringIndex(cfg.Global.MCPs, name) >= 0
		if global && !globallyActive {
			continue
		}
		if projectFilter && !globallyActive && stringIndex(cfg.Projects[projectID].MCPs, name) < 0 {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		switch {
		case global:
			fmt.Fprintln(stdout, "No MCPs enabled globally.")
		case projectFilter:
			fmt.Fprintf(stdout, "No MCPs enabled for project %q (including global MCPs).\n", projectID)
		default:
			fmt.Fprintln(stdout, "No MCPs configured. Add one with mcp-manager add NAME -- COMMAND.")
		}
		return 0
	}
	sort.Strings(names)
	table := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "NAME\tTYPE\tSCOPES")
	for _, name := range names {
		fmt.Fprintf(table, "%s\t%s\t%s\n", name, cfg.MCPs[name].Type, strings.Join(mcpScopes(cfg, name), ", "))
	}
	if err := table.Flush(); err != nil {
		fmt.Fprintf(stderr, "error: write MCP list: %v\n", err)
		return 1
	}
	return 0
}

func redactedMCPValues(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]string, len(values))
	for key := range values {
		result[key] = "[REDACTED]"
	}
	return result
}

func mcpScopes(cfg *config.Config, name string) []string {
	scopes := []string{}
	if stringIndex(cfg.Global.MCPs, name) >= 0 {
		scopes = append(scopes, "global"+mcpExclusions(cfg.Global.DisabledAgents[name]))
	}
	projects := []string{}
	for id, project := range cfg.Projects {
		if stringIndex(project.MCPs, name) >= 0 {
			projects = append(projects, "project:"+id+mcpExclusions(project.DisabledAgents[name]))
		}
	}
	sort.Strings(projects)
	scopes = append(scopes, projects...)
	if len(scopes) == 0 {
		scopes = append(scopes, "disabled")
	}
	return scopes
}

func mcpExclusions(agents []config.Agent) string {
	if len(agents) == 0 {
		return ""
	}
	names := make([]string, len(agents))
	for i, agent := range agents {
		names[i] = string(agent)
	}
	sort.Strings(names)
	return " (except " + strings.Join(names, ", ") + ")"
}
