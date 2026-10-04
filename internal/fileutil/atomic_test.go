package fileutil

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestWriteAtomicReplacesSymlinkTargetAndKeepsLinks(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "dotfiles", "mcp.json")
	middle := filepath.Join(base, "dotfiles", "current.json")
	link := filepath.Join(base, "project", ".mcp.json")
	writeFile(t, target, "old", 0o640)
	symlink(t, "mcp.json", middle)
	symlink(t, "../dotfiles/current.json", link)

	mode, err := ExistingMode(link, 0o600)
	if err != nil || mode != 0o640 {
		t.Fatalf("ExistingMode() = %v, %v; want the target's mode 0640", mode, err)
	}
	if err := WriteAtomic(link, []byte("new"), mode); err != nil {
		t.Fatalf("WriteAtomic() error = %v", err)
	}
	assertContent(t, target, "new", 0o640)
	assertLink(t, middle, "mcp.json")
	assertLink(t, link, "../dotfiles/current.json")
	assertEntries(t, filepath.Dir(target), "current.json", "mcp.json")
	assertEntries(t, filepath.Dir(link), ".mcp.json")
}

func TestWriteAtomicCreatesDanglingSymlinkTarget(t *testing.T) {
	base := t.TempDir()
	// The link sits in real/ but is reached through a directory link in
	// another parent, so its relative target must start at real/.
	link := filepath.Join(base, "real", ".mcp.json")
	symlink(t, "../shared/mcp.json", link)
	symlink(t, "../real", filepath.Join(base, "elsewhere", "alias"))
	path := filepath.Join(base, "elsewhere", "alias", ".mcp.json")

	mode, err := ExistingMode(path, 0o600)
	if err != nil || mode != 0o600 {
		t.Fatalf("ExistingMode() = %v, %v; want the default mode for a missing target", mode, err)
	}
	if err := WriteAtomic(path, []byte("created"), mode); err != nil {
		t.Fatalf("WriteAtomic() error = %v", err)
	}
	assertContent(t, filepath.Join(base, "shared", "mcp.json"), "created", 0o600)
	assertContent(t, path, "created", 0o600)
	assertLink(t, link, "../shared/mcp.json")
	if _, err := os.Lstat(filepath.Join(base, "elsewhere", "shared")); !os.IsNotExist(err) {
		t.Fatalf("WriteAtomic() resolved the link lexically: %v", err)
	}
}

func TestExistingModeRejectsSymlinksToNonRegularFiles(t *testing.T) {
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	symlink(t, "directory", filepath.Join(base, "to-directory.json"))
	symlink(t, "second.json", filepath.Join(base, "first.json"))
	symlink(t, "first.json", filepath.Join(base, "second.json"))
	for _, name := range []string{"to-directory.json", "first.json"} {
		if _, err := ExistingMode(filepath.Join(base, name), 0o600); err == nil {
			t.Fatalf("ExistingMode(%s) accepted an invalid destination", name)
		}
	}
}

func TestWriteAtomicRejectsSymlinksThatNeverResolve(t *testing.T) {
	base := t.TempDir()
	symlink(t, "second.json", filepath.Join(base, "first.json"))
	symlink(t, "first.json", filepath.Join(base, "second.json"))
	// Its missing directory makes this link dangle, and its target cleans
	// back to the link itself.
	symlink(t, "missing/../self.json", filepath.Join(base, "self.json"))
	for _, name := range []string{"first.json", "self.json"} {
		if err := WriteAtomic(filepath.Join(base, name), []byte("{}"), 0o600); err == nil {
			t.Fatalf("WriteAtomic(%s) followed a link that never resolves", name)
		}
	}
	assertEntries(t, base, "first.json", "second.json", "self.json")
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func assertContent(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != content || info.Mode().Perm() != mode {
		t.Fatalf("%s = %q with mode %v; want %q with mode %v", path, data, info.Mode().Perm(), content, mode)
	}
}

func assertLink(t *testing.T, link, target string) {
	t.Helper()
	got, err := os.Readlink(link)
	if err != nil || got != target {
		t.Fatalf("Readlink(%s) = %q, %v; want the link to %q kept", link, got, err, target)
	}
}

// assertEntries also catches temporary files left behind by WriteAtomic.
func assertEntries(t *testing.T, directory string, expected ...string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if !slices.Equal(names, expected) {
		t.Fatalf("%s contains %v, want %v", directory, names, expected)
	}
}
