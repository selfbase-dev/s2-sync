package sync

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWalk(t *testing.T) {
	dir := t.TempDir()

	// Create files
	os.MkdirAll(filepath.Join(dir, "sub"), 0755)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0644)
	os.WriteFile(filepath.Join(dir, "sub", "b.txt"), []byte("world"), 0644)

	// Create .s2 directory (should be skipped)
	os.MkdirAll(filepath.Join(dir, ".s2"), 0755)
	os.WriteFile(filepath.Join(dir, ".s2", "state.json"), []byte("{}"), 0644)

	files, err := Walk(dir, nil, nil)
	if err != nil {
		t.Fatalf("Walk failed: %v", err)
	}

	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d: %v", len(files), files)
	}

	if _, ok := files["a.txt"]; !ok {
		t.Error("expected a.txt")
	}
	if _, ok := files["sub/b.txt"]; !ok {
		t.Error("expected sub/b.txt")
	}
	if _, ok := files[".s2/state.json"]; ok {
		t.Error(".s2/state.json should be excluded")
	}

	// Verify hash is SHA-256
	if len(files["a.txt"].Hash) != 64 {
		t.Errorf("expected 64-char SHA-256 hash, got %d chars", len(files["a.txt"].Hash))
	}
}

func TestWalkExclude(t *testing.T) {
	dir := t.TempDir()

	os.MkdirAll(filepath.Join(dir, ".git"), 0755)
	os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("data"), 0644)
	os.MkdirAll(filepath.Join(dir, "node_modules", "pkg"), 0755)
	os.WriteFile(filepath.Join(dir, "node_modules", "pkg", "index.js"), []byte("data"), 0644)
	os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("keep"), 0644)

	exclude := DefaultExclude()
	files, err := Walk(dir, nil, exclude)
	if err != nil {
		t.Fatalf("Walk failed: %v", err)
	}

	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d: %v", len(files), files)
	}
	if _, ok := files["keep.txt"]; !ok {
		t.Error("expected keep.txt")
	}
}

func TestWalkSameContent(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("same"), 0644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("same"), 0644)

	files, err := Walk(dir, nil, nil)
	if err != nil {
		t.Fatalf("Walk failed: %v", err)
	}

	if files["a.txt"].Hash != files["b.txt"].Hash {
		t.Error("same content should produce same hash")
	}
}
