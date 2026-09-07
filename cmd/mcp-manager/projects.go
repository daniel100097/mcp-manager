package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/daniel100097/mcp-manager/internal/config"
	"github.com/daniel100097/mcp-manager/internal/syncer"
)

func printProjectUsage(output io.Writer) {
	fmt.Fprintln(output, `Usage:
  mcp-manager project add [PATH] [--name ID] [--dry-run]
  mcp-manager project list
  mcp-manager project show [PROJECT]
  mcp-manager project remove [PROJECT] [--dry-run]

Add registers the current directory by default. Show and remove find the
nearest registered project from the current directory, including its enabled
Git worktrees. Use a project ID, path, or --project ID|PATH to select another.
Registration and removal only update the registry; agent configs are retained.

Use "mcp-manager project add --help" for options.`)
}

func runProject(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		printProjectUsage(stdout)
		return 0
	}
	action := args[0]
	if action != "add" && action != "list" && action != "show" && action != "remove" {
		fmt.Fprintf(stderr, "error: unknown project action %q\n", action)
		printProjectUsage(stderr)
		return 2
	}
	defaultConfig, err := syncer.DefaultConfigPath()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	flags := flag.NewFlagSet("project "+action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configFlags := addConfigFlags(flags, defaultConfig)
	var name, selector string
	var dryRun bool
	switch action {
	case "add":
		flags.StringVar(&name, "name", "", "project ID; defaults to the directory name")
	case "show", "remove":
		flags.StringVar(&selector, "project", "", "project ID or path; defaults to the current directory")
	}
	if action == "add" || action == "remove" {
		flags.BoolVar(&dryRun, "dry-run", false, "show registry changes without writing files")
	}
	flags.Usage = func() {
		argument := " [PROJECT]"
		if action == "add" {
			argument = " [PATH]"
		} else if action == "list" {
			argument = ""
		}
		fmt.Fprintf(stderr, "Usage: mcp-manager project %s%s [options]\n\nOptions:\n", action, argument)
		printLongFlagDefaults(stderr, flags)
	}
	if err := parseFlags(flags, args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() > 1 || (action == "list" && flags.NArg() != 0) || (selector != "" && flags.NArg() != 0) {
		fmt.Fprintln(stderr, "error: select at most one project using its ID, path, or --project")
		flags.Usage()
		return 2
	}
	if flags.NArg() == 1 {
		selector = flags.Arg(0)
	}
	source, err := configFlags.source()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	edit, err := openConfigEdit(source, action == "add")
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if action == "add" {
		return addProject(edit, selector, name, dryRun, stdout, stderr)
	}
	if action == "list" {
		if len(edit.Config.Projects) == 0 {
			fmt.Fprintln(stdout, "No projects registered. Run \"mcp-manager project add\" in a project directory.")
			return 0
		}
		current, _ := resolveProject(edit.Config, "")
		table := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(table, "\tPROJECT\tPATH\tWORKTREES\tLOCAL MCPS")
		for _, id := range projectIDs(edit.Config) {
			project := edit.Config.Projects[id]
			marker := ""
			if id == current {
				marker = "*"
			}
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%d\n", marker, id, project.Path, worktreeSetting(project.IncludeWorktrees), len(project.MCPs))
		}
		_ = table.Flush()
		return 0
	}
	id, err := resolveProject(edit.Config, selector)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	project := edit.Config.Projects[id]
	if action == "show" {
		fmt.Fprintf(stdout, "Project: %s\nPath: %s\nWorktrees: %s\nLocal MCPs: %s\nGlobal MCPs: %s\n", id, project.Path, worktreeSetting(project.IncludeWorktrees), projectMCPNames(project.MCPs), projectMCPNames(edit.Config.Global.MCPs))
		return 0
	}
	if dryRun {
		fmt.Fprintf(stdout, "would remove project %q from the registry (%s)\n", id, project.Path)
	} else {
		delete(edit.Config.Projects, id)
		if err := edit.Save(); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "removed project %q from the registry (%s)\n", id, project.Path)
		fmt.Fprintf(stdout, "%s updated: %s\n", edit.Label(), edit.Target())
	}
	fmt.Fprintln(stdout, "Agent configs are retained; registry changes do not sync files.")
	return 0
}

func addProject(edit *config.Edit, path, name string, dryRun bool, stdout, stderr io.Writer) int {
	if path == "" {
		path = "."
	}
	canonical, err := canonicalProjectDirectory(path)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if name == "" {
		name = regexp.MustCompile(`[^A-Za-z0-9_-]+`).ReplaceAllString(filepath.Base(canonical), "-")
		name = strings.Trim(name, "-_")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(name) {
		fmt.Fprintln(stderr, "error: use --name with a project ID containing only letters, numbers, underscores, and hyphens")
		return 2
	}
	if existing, found := edit.Config.Projects[name]; found {
		existingPath, pathErr := canonicalProjectDirectory(existing.Path)
		if pathErr != nil || existingPath != canonical {
			fmt.Fprintf(stderr, "error: project ID %q is already registered at %q; choose another --name\n", name, existing.Path)
			return 1
		}
	}
	for _, id := range projectIDs(edit.Config) {
		existingPath, pathErr := canonicalProjectDirectory(edit.Config.Projects[id].Path)
		if pathErr == nil && existingPath == canonical {
			if edit.NeedsInitialization() {
				if dryRun {
					fmt.Fprintf(stdout, "would initialize config for already registered project %q at %s\n", id, canonical)
					return 0
				}
				if err := edit.Save(); err != nil {
					fmt.Fprintf(stderr, "error: %v\n", err)
					return 1
				}
				fmt.Fprintf(stdout, "initialized config for already registered project %q at %s\n", id, canonical)
				return 0
			}
			fmt.Fprintf(stdout, "project %q is already registered at %s; %s unchanged\n", id, canonical, edit.Label())
			return 0
		}
	}
	if dryRun {
		fmt.Fprintf(stdout, "would register project %q at %s\n", name, canonical)
	} else {
		if edit.Config.Projects == nil {
			edit.Config.Projects = map[string]config.Project{}
		}
		edit.Config.Projects[name] = config.Project{Path: canonical, MCPs: []string{}}
		if err := edit.Save(); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "registered project %q at %s\n", name, canonical)
		fmt.Fprintf(stdout, "%s updated: %s\n", edit.Label(), edit.Target())
	}
	fmt.Fprintln(stdout, "Agent configs are retained; registry changes do not sync files.")
	return 0
}

func worktreeSetting(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

func projectMCPNames(names []string) string {
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

func projectIDs(cfg *config.Config) []string {
	ids := make([]string, 0, len(cfg.Projects))
	for id := range cfg.Projects {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// canonicalProjectDirectory accepts user-facing relative paths and the ~
// notation used by the registry. It never mutates stored project paths.
func canonicalProjectDirectory(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path, err = config.ExpandProjectPath(path, home)
		if err != nil {
			return "", err
		}
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("inspect project directory %q: %w", path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("project path %q is not a directory", path)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve project directory %q: %w", path, err)
	}
	return filepath.Clean(canonical), nil
}

// resolveProject selects an explicit ID or the closest containing registered
// directory. Enabled Git worktrees inherit their owner's ID, unless a project
// is explicitly registered at that worktree or closer to the selected path.
// OpenEdit keeps paths unexpanded, so resolve each path without changing cfg.
func resolveProject(cfg *config.Config, selector string) (string, error) {
	if selector != "" {
		if _, exists := cfg.Projects[selector]; exists {
			return selector, nil
		}
	}
	path := selector
	if path == "" {
		path = "."
	}
	current, err := canonicalProjectDirectory(path)
	if err != nil {
		return "", fmt.Errorf("cannot select project %q: %w; use a registered project ID or run \"mcp-manager project add\" in its directory", path, err)
	}
	type candidate struct {
		id       string
		root     string
		explicit bool
	}
	var matches []candidate
	roots := map[string]string{}
	contains := func(root string) bool {
		relative, err := filepath.Rel(root, current)
		return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
	}
	for _, id := range projectIDs(cfg) {
		root, err := canonicalProjectDirectory(cfg.Projects[id].Path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("project %q: %w", id, err)
		}
		roots[id] = root
		if contains(root) {
			matches = append(matches, candidate{id: id, root: root, explicit: true})
		}
	}
	var discoveryErrors []error
	for _, id := range projectIDs(cfg) {
		root, exists := roots[id]
		if !exists || !cfg.Projects[id].IncludeWorktrees {
			continue
		}
		worktrees, err := syncer.DiscoverWorktrees(root)
		if err != nil {
			discoveryErrors = append(discoveryErrors, fmt.Errorf("project %q: %w", id, err))
			continue
		}
		for _, worktree := range worktrees {
			if worktree != root && contains(worktree) {
				matches = append(matches, candidate{id: id, root: worktree})
			}
		}
	}
	if len(matches) == 0 {
		err := fmt.Errorf("no registered project contains %q; run \"mcp-manager project add\" in the project directory, or select one with --project ID", current)
		return "", errors.Join(append([]error{err}, discoveryErrors...)...)
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if len(matches[i].root) != len(matches[j].root) {
			return len(matches[i].root) > len(matches[j].root)
		}
		return matches[i].explicit && !matches[j].explicit
	})
	best := matches[0]
	var owners []string
	for _, match := range matches {
		if match.root == best.root && match.explicit == best.explicit {
			owners = append(owners, match.id)
		}
	}
	if len(owners) > 1 {
		return "", fmt.Errorf("project selection is ambiguous at %q (%s); select one with --project ID", best.root, strings.Join(owners, ", "))
	}
	return best.id, nil
}
