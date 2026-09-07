package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/daniel100097/mcp-manager/internal/config"
)

// parseFlags accepts options before or after positional arguments. Everything
// following -- is positional, so server arguments are never parsed as ours.
func parseFlags(flags *flag.FlagSet, args []string) error {
	var options, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		options = append(options, arg)
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		option := flags.Lookup(name)
		if option == nil || hasValue {
			continue
		}
		if boolean, ok := option.Value.(interface{ IsBoolFlag() bool }); ok && boolean.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			i++
			options = append(options, args[i])
		}
	}
	// Let flag.Parse diagnose a missing final option value before inserting --.
	if err := flags.Parse(options); err != nil {
		return err
	}
	return flags.Parse(append([]string{"--"}, positional...))
}

func openConfigEdit(source config.Source, create bool) (*config.Edit, error) {
	if create {
		return source.OpenOrCreateEdit()
	}
	edit, err := source.OpenEdit()
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no configuration found; run 'mcp-manager project add' or 'mcp-manager add NAME --global --url URL' first: %w", err)
	}
	return edit, err
}

// saveAndSync preflights the entire sync before saving configuration edits.
// Descriptions use the imperative (e.g. "add MCP ...") for dry-run output.
func saveAndSync(edit *config.Edit, source config.Source, changed, dryRun bool, description string, stdout, stderr io.Writer) int {
	changed = changed || edit.NeedsInitialization()
	effective, err := edit.Effective()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	var preview, diagnostics bytes.Buffer
	if code := syncConfig(effective, source, false, true, &preview, &diagnostics); code != 0 {
		fmt.Fprint(stderr, diagnostics.String())
		return code
	}
	if changed {
		if dryRun {
			fmt.Fprintf(stdout, "would %s\n", description)
			fmt.Fprintf(stdout, "%s would change: %s\n", edit.Label(), edit.Target())
		} else {
			if err := edit.Save(); err != nil {
				fmt.Fprintf(stderr, "error: %v\n", err)
				return 1
			}
			fmt.Fprintf(stdout, "%s updated: %s\n", edit.Label(), edit.Target())
			fmt.Fprintf(stdout, "applied: %s\n", description)
		}
	} else {
		fmt.Fprintf(stdout, "%s unchanged: %s\n", edit.Label(), description)
	}
	if dryRun {
		fmt.Fprint(stdout, preview.String())
		fmt.Fprint(stderr, diagnostics.String())
		return 0
	}
	return syncConfig(effective, source, false, false, stdout, stderr)
}

// selectedProject accepts the legacy positional selector alongside --project.
func selectedProject(positional, option string) (string, error) {
	if positional != "" && option != "" {
		return "", errors.New("choose either --project or a positional project, not both")
	}
	if option != "" {
		return option, nil
	}
	return positional, nil
}
