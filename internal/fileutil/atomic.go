package fileutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// WriteAtomic writes through a temporary file in the destination directory and
// renames it over the destination. Callers must validate an existing target
// before calling this function.
func WriteAtomic(path string, data []byte, mode os.FileMode) error {
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

// ExistingMode validates an existing destination and returns its permissions.
// Missing destinations use defaultMode.
func ExistingMode(path string, defaultMode os.FileMode) (os.FileMode, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return defaultMode, nil
	}
	if err != nil {
		return 0, fmt.Errorf("inspect destination %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return 0, fmt.Errorf("destination %q is a symlink; refusing to replace it", path)
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("destination %q is not a regular file", path)
	}
	return info.Mode().Perm(), nil
}
