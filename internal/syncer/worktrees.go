package syncer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type gitWorktree struct {
	path     string
	bare     bool
	prunable bool
}

// DiscoverWorktrees returns the existing checkout roots for an opted-in
// project, using the same rules as sync. Callers must check IncludeWorktrees.
func DiscoverWorktrees(root string) ([]string, error) {
	return discoverWorktrees(root)
}

// discoverWorktrees returns existing checkout roots, including the main
// checkout. NUL-delimited porcelain preserves spaces, newlines and quotes in
// paths. A registered subdirectory is deliberately not expanded to its repo.
func discoverWorktrees(root string) ([]string, error) {
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve project path %q: %w", root, err)
	}
	canonicalRoot, err = filepath.Abs(canonicalRoot)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", canonicalRoot, "worktree", "list", "--porcelain", "-z")
	// Repository overrides inherited from a Git hook must not redirect -C to
	// the hook's repository when syncing other registered projects.
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE":
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("git worktree list at %q: %w", root, ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("git worktree list at %q: %w: %s", root, err, bytes.TrimSpace(exitErr.Stderr))
		}
		return nil, fmt.Errorf("git worktree list at %q (Git is required when includeWorktrees is enabled): %w", root, err)
	}
	worktrees, err := parseWorktrees(output)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, worktree := range worktrees {
		if worktree.bare || worktree.prunable {
			continue
		}
		info, err := os.Stat(worktree.path)
		if errors.Is(err, os.ErrNotExist) {
			// Git can retain stale registrations until worktree prune runs.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect worktree %q: %w", worktree.path, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("worktree %q is not a directory", worktree.path)
		}
		path, err := filepath.EvalSymlinks(worktree.path)
		if err != nil {
			return nil, fmt.Errorf("resolve worktree %q: %w", worktree.path, err)
		}
		path, err = filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		seen[path] = true
	}
	if !seen[canonicalRoot] {
		return nil, fmt.Errorf("includeWorktrees requires a Git checkout root; %q is not one", root)
	}
	roots := make([]string, 0, len(seen))
	for path := range seen {
		roots = append(roots, path)
	}
	sort.Strings(roots)
	return roots, nil
}

func parseWorktrees(output []byte) ([]gitWorktree, error) {
	var worktrees []gitWorktree
	for _, record := range bytes.Split(output, []byte{0, 0}) {
		if len(record) == 0 {
			continue
		}
		fields := bytes.Split(record, []byte{0})
		path, ok := strings.CutPrefix(string(fields[0]), "worktree ")
		if !ok || !filepath.IsAbs(path) {
			return nil, fmt.Errorf("invalid git worktree list output: expected an absolute worktree path")
		}
		worktree := gitWorktree{path: path}
		for _, field := range fields[1:] {
			if string(field) == "bare" {
				worktree.bare = true
			}
			if string(field) == "prunable" || bytes.HasPrefix(field, []byte("prunable ")) {
				worktree.prunable = true
			}
		}
		worktrees = append(worktrees, worktree)
	}
	return worktrees, nil
}
