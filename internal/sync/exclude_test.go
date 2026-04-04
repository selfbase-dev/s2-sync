package sync

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultExclude(t *testing.T) {
	exclude := DefaultExclude()

	shouldExclude := []string{
		".git",
		".git/config",
		"node_modules",
		"node_modules/pkg/index.js",
		".DS_Store",
		"Thumbs.db",
		"desktop.ini",
		"file.swp",
		"file.swo",
		"file~",
		"._document.pdf",    // macOS resource fork
		"._.DS_Store",       // macOS
		".s2",               // hard exclude
		".s2/state.json",    // hard exclude
		"file.sync-conflict-20260101-120000.txt", // hard exclude
	}

	for _, path := range shouldExclude {
		if !exclude(path) {
			t.Errorf("should exclude %q", path)
		}
	}

	shouldNotExclude := []string{
		"readme.md",
		"docs/notes.txt",
		"src/main.go",
		".gitignore", // not .git itself
		"my_module",  // not node_modules
	}

	for _, path := range shouldNotExclude {
		if exclude(path) {
			t.Errorf("should not exclude %q", path)
		}
	}
}

func TestLoadExclude_WithS2Ignore(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".s2ignore"), []byte("*.log\nbuild\n# comment\n\n"), 0600)

	exclude := LoadExclude(dir)

	if !exclude("app.log") {
		t.Error("should exclude *.log")
	}
	if !exclude("build") {
		t.Error("should exclude build")
	}
	if !exclude("build/output.js") {
		t.Error("should exclude build/output.js")
	}
	// Default excludes still work
	if !exclude(".DS_Store") {
		t.Error("should still exclude .DS_Store")
	}
	if exclude("readme.md") {
		t.Error("should not exclude readme.md")
	}
}

func TestLoadExclude_NoS2Ignore(t *testing.T) {
	dir := t.TempDir()
	// No .s2ignore file
	exclude := LoadExclude(dir)
	// Should still work with defaults
	if !exclude(".git") {
		t.Error("should exclude .git")
	}
}

func TestHardExcludes_CannotBeOverridden(t *testing.T) {
	exclude := DefaultExclude()

	// .s2 is always excluded
	if !exclude(".s2") {
		t.Error(".s2 should always be excluded")
	}
	if !exclude(".s2/state.json") {
		t.Error(".s2/state.json should always be excluded")
	}

	// .sync-conflict-* is always excluded
	if !exclude("doc.sync-conflict-20260101-120000.txt") {
		t.Error("sync-conflict files should always be excluded")
	}
}
