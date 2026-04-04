package sync

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0755)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writeFile(%q): %v", path, err)
	}
}

func TestWalk_BasicFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "hello")
	writeFile(t, filepath.Join(dir, "sub", "b.txt"), "world")

	files, err := Walk(dir, nil, nil)
	if err != nil {
		t.Fatalf("Walk() error: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2", len(files))
	}
	if _, ok := files["a.txt"]; !ok {
		t.Error("missing a.txt")
	}
	if _, ok := files["sub/b.txt"]; !ok {
		t.Error("missing sub/b.txt")
	}
}

func TestWalk_SkipsS2Dir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "hello")
	writeFile(t, filepath.Join(dir, ".s2", "state.json"), "{}")

	files, err := Walk(dir, nil, nil)
	if err != nil {
		t.Fatalf("Walk() error: %v", err)
	}
	if _, ok := files[".s2/state.json"]; ok {
		t.Error(".s2/state.json should be skipped")
	}
	if len(files) != 1 {
		t.Errorf("got %d files, want 1", len(files))
	}
}

func TestWalk_ExcludeFunction(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "hello")
	writeFile(t, filepath.Join(dir, ".DS_Store"), "mac")       // not excluded by default anymore
	writeFile(t, filepath.Join(dir, "._hidden"), "resource fork") // excluded: matches ._*
	writeFile(t, filepath.Join(dir, ".git", "config"), "git")  // not excluded by default anymore

	exclude := DefaultExclude()
	files, err := Walk(dir, nil, exclude)
	if err != nil {
		t.Fatalf("Walk() error: %v", err)
	}
	// Only ._hidden is excluded by DefaultExclude; .DS_Store and .git are not
	if _, ok := files["._hidden"]; ok {
		t.Error("._hidden should be excluded (matches ._*)")
	}
	if _, ok := files[".git/config"]; !ok {
		t.Error(".git/config should be included (not excluded by default)")
	}
	if _, ok := files[".DS_Store"]; !ok {
		t.Error(".DS_Store should be included (only excluded via .s2ignore)")
	}
}

func TestWalk_HashConsistency(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "consistent content")

	files1, _ := Walk(dir, nil, nil)
	files2, _ := Walk(dir, nil, nil)

	if files1["a.txt"].Hash != files2["a.txt"].Hash {
		t.Error("hash should be consistent across walks")
	}
}

func TestWalk_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	files, err := Walk(dir, nil, nil)
	if err != nil {
		t.Fatalf("Walk() error: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("got %d files, want 0", len(files))
	}
}

func TestWalk_ForwardSlashPaths(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a", "b", "c.txt"), "deep")

	files, _ := Walk(dir, nil, nil)
	if _, ok := files["a/b/c.txt"]; !ok {
		t.Error("paths should use forward slashes")
	}
}
