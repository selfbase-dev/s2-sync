package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultExclude(t *testing.T) {
	exclude := DefaultExclude()

	// Only built-in defaults: macOS metadata and hard excludes
	shouldExclude := []string{
		"desktop.ini",
		"._document.pdf",                         // macOS resource fork
		"._.DS_Store",                            // macOS
		".s2",                                    // hard exclude
		".s2/state.json",                         // hard exclude
		"file.sync-conflict-20260101-120000.txt", // hard exclude
	}

	for _, path := range shouldExclude {
		if !exclude(path) {
			t.Errorf("should exclude %q", path)
		}
	}

	// .git and node_modules are NOT excluded by default
	shouldNotExclude := []string{
		"readme.md",
		"docs/notes.txt",
		"src/main.go",
		".gitignore",
		".git",
		".git/config",
		"node_modules",
		"node_modules/pkg/index.js",
		".DS_Store", // only excluded via .s2ignore
		"Thumbs.db", // only excluded via .s2ignore
		"file.swp",  // only excluded via .s2ignore
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
	if exclude("readme.md") {
		t.Error("should not exclude readme.md")
	}
	// .DS_Store not in this custom .s2ignore, so not excluded
	if exclude(".DS_Store") {
		t.Error(".DS_Store should not be excluded when not in .s2ignore")
	}
}

func TestLoadExclude_WithDefaultS2Ignore(t *testing.T) {
	dir := t.TempDir()
	// Simulate EnsureIgnoreFile creating the default .s2ignore
	if err := EnsureIgnoreFile(dir); err != nil {
		t.Fatal(err)
	}

	exclude := LoadExclude(dir)

	// Patterns from default .s2ignore
	if !exclude(".DS_Store") {
		t.Error("should exclude .DS_Store via default .s2ignore")
	}
	if !exclude("Thumbs.db") {
		t.Error("should exclude Thumbs.db via default .s2ignore")
	}
	if !exclude("file.swp") {
		t.Error("should exclude *.swp via default .s2ignore")
	}
	if !exclude("file.swo") {
		t.Error("should exclude *.swo via default .s2ignore")
	}
	if !exclude("file~") {
		t.Error("should exclude *~ via default .s2ignore")
	}

	// .git and node_modules still not excluded
	if exclude(".git") {
		t.Error(".git should not be excluded")
	}
	if exclude("node_modules") {
		t.Error("node_modules should not be excluded")
	}
}

func TestLoadExclude_NoS2Ignore(t *testing.T) {
	dir := t.TempDir()
	// No .s2ignore file — .git is no longer excluded by default
	exclude := LoadExclude(dir)
	if exclude(".git") {
		t.Error(".git should not be excluded without .s2ignore")
	}
	if exclude("node_modules") {
		t.Error("node_modules should not be excluded without .s2ignore")
	}
}

func TestEnsureIgnoreFile_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureIgnoreFile(dir); err != nil {
		t.Fatalf("EnsureIgnoreFile failed: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(dir, ".s2ignore"))
	if err != nil {
		t.Fatalf("failed to read .s2ignore: %v", err)
	}

	for _, pattern := range []string{".DS_Store", "Thumbs.db", "*.swp", "*.swo", "*~"} {
		if !strings.Contains(string(content), pattern) {
			t.Errorf("default .s2ignore missing pattern %q", pattern)
		}
	}
}

func TestEnsureIgnoreFile_DoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	custom := "my-custom-pattern\n"
	os.WriteFile(filepath.Join(dir, ".s2ignore"), []byte(custom), 0600)

	if err := EnsureIgnoreFile(dir); err != nil {
		t.Fatalf("EnsureIgnoreFile failed: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(dir, ".s2ignore"))
	if err != nil {
		t.Fatalf("failed to read .s2ignore: %v", err)
	}
	if string(content) != custom {
		t.Errorf("EnsureIgnoreFile overwrote existing .s2ignore")
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
