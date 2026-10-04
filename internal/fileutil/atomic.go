package fileutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// maxSymlinks bounds the dangling links ResolveSymlinks follows, matching
// filepath.EvalSymlinks.
const maxSymlinks = 255

// WriteAtomic writes through a temporary file in the destination directory and
// renames it over the destination. A destination that is a symbolic link is
// followed: the file it points to is replaced and the link is kept. Callers
// must validate an existing target before calling this function.
func WriteAtomic(path string, data []byte, mode os.FileMode) error {
	path, err := ResolveSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve destination: %w", err)
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".mcp-manager-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()

	if err := temporary.Chmod(mode); err != nil {
		return fmt.Errorf("set temporary file mode: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace destination: %w", err)
	}
	return nil
}

// ResolveSymlinks returns the file that a write to path replaces. Like
// filepath.EvalSymlinks it resolves every symbolic link, but the file need not
// exist: a dangling link resolves to the missing file it points to, and any
// other missing path is returned unchanged.
func ResolveSymlinks(path string) (string, error) {
	for range maxSymlinks {
		resolved, err := filepath.EvalSymlinks(path)
		if !errors.Is(err, fs.ErrNotExist) {
			return resolved, err
		}
		target, err := os.Readlink(path)
		if err != nil {
			// Not a link: the missing file is created at this path itself.
			return path, nil
		}
		if !filepath.IsAbs(target) {
			// A relative target starts at the link's real directory, which
			// differs from filepath.Dir(path) when path runs through a link.
			directory, err := filepath.EvalSymlinks(filepath.Dir(path))
			if err != nil {
				return "", err
			}
			target = filepath.Join(directory, target)
		}
		path = target
	}
	return "", errors.New("too many levels of symbolic links")
}

// ExistingMode validates an existing destination and returns its permissions.
// A symbolic link is validated by the file it points to. Missing destinations,
// including dangling links, use defaultMode.
func ExistingMode(path string, defaultMode os.FileMode) (os.FileMode, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return defaultMode, nil
	}
	if err != nil {
		return 0, fmt.Errorf("inspect destination %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("destination %q is not a regular file", path)
	}
	return info.Mode().Perm(), nil
}
