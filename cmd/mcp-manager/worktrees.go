package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"

	"github.com/daniel100097/mcp-manager/internal/syncer"
)

func printWorktreesUsage(output io.Writer) {
	fmt.Fprintln(output, `Usage: mcp-manager worktrees enable|disable [--project ID|PATH] [options]

Enable or disable Git worktree discovery for a registered project's path.
The current directory selects the project; a positional project is also accepted.
Enabling synchronizes its current worktrees. Run sync after creating more.
Disabling stops managing discovered worktrees; existing configs are retained.

Use "mcp-manager worktrees enable --help" for options.`)
}

func runWorktrees(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "error: worktrees requires enable or disable")
		printWorktreesUsage(stderr)
		return 2
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		printWorktreesUsage(stdout)
		return 0
	}
	action := args[0]
	if action != "enable" && action != "disable" {
		fmt.Fprintf(stderr, "error: unknown worktrees action %q\n", action)
		printWorktreesUsage(stderr)
		return 2
	}
	defaultConfig, err := syncer.DefaultConfigPath()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	flags := flag.NewFlagSet("worktrees "+action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configFlags := addConfigFlags(flags, defaultConfig)
	dryRun := flags.Bool("dry-run", false, "show all changes without writing files")
	projectOption := flags.String("project", "", "project ID or path; defaults to the current directory")
	flags.Usage = func() {
		fmt.Fprintf(stderr, "Usage: mcp-manager worktrees %s [--project ID|PATH] [options]\n\nOptions:\n", action)
		printLongFlagDefaults(stderr, flags)
	}
	if err := parseFlags(flags, args[1:]); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() > 1 {
		fmt.Fprintf(stderr, "error: worktrees %s accepts at most one project\n\n", action)
		flags.Usage()
		return 2
	}
	selector, err := selectedProject(flags.Arg(0), *projectOption)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
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
	projectID, err := resolveProject(edit.Config, selector)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	project := edit.Config.Projects[projectID]
	enabled := action == "enable"
	changed := project.IncludeWorktrees != enabled
	project.IncludeWorktrees = enabled
	edit.Config.Projects[projectID] = project
	effective, err := edit.Effective()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	// Discover and validate every target before persisting the activation. Keep
	// the preview for dry runs without printing duplicate output on real syncs.
	var preview, diagnostics bytes.Buffer
	if code := syncConfig(effective, source, false, true, &preview, &diagnostics); code != 0 {
		fmt.Fprint(stderr, diagnostics.String())
		return code
	}
	switch {
	case !changed:
		fmt.Fprintf(stdout, "worktree discovery is already %sd for project %q; %s unchanged\n", action, projectID, edit.Label())
	case *dryRun:
		fmt.Fprintf(stdout, "would %s worktree discovery for project %q\n", action, projectID)
		fmt.Fprintf(stdout, "dry run complete: %s would change: %s\n", edit.Label(), edit.Target())
	default:
		if err := edit.Save(); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "%sd worktree discovery for project %q\n", action, projectID)
		fmt.Fprintf(stdout, "%s updated: %s\n", edit.Label(), edit.Target())
	}
	if *dryRun {
		fmt.Fprint(stdout, preview.String())
		fmt.Fprint(stderr, diagnostics.String())
		return 0
	}
	// Repeated enables also sync, so they pick up newly created worktrees.
	return syncConfig(effective, source, false, false, stdout, stderr)
}
