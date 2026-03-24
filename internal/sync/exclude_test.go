package sync

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultExclude(t *testing.T) {
	exclude := DefaultExclude()

	tests := []struct {
		path string
		want bool
	}{
		{".git", true},
		{".git/config", true},
		{"node_modules", true},
		{"node_modules/pkg/index.js", true},
		{".DS_Store", true},
		{"Thumbs.db", true},
		{"file.swp", true},
		{"file.swo", true},
		{"file~", true},
		{"readme.md", false},
		{"src/main.go", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := exclude(tt.path)
			if got != tt.want {
				t.Errorf("exclude(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestLoadExcludeWithS2Ignore(t *testing.T) {
	dir := t.TempDir()

	// Create .s2ignore
	ignoreContent := "*.log\n# comment\nbuild\n"
	os.WriteFile(filepath.Join(dir, ".s2ignore"), []byte(ignoreContent), 0644)

	exclude := LoadExclude(dir)

	tests := []struct {
		path string
		want bool
	}{
		{"app.log", true},       // from .s2ignore
		{"build", true},         // from .s2ignore
		{"build/out.js", true},  // directory match
		{".git", true},          // default
		{"readme.md", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := exclude(tt.path)
			if got != tt.want {
				t.Errorf("exclude(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}
