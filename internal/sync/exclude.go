package sync

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// Default exclude patterns (user can override via .s2ignore)
var defaultExcludes = []string{
	".git",
	"node_modules",
	".DS_Store",
	"._*",
	"Thumbs.db",
	"desktop.ini",
	"*.swp",
	"*.swo",
	"*~",
}

// Hard-coded excludes (always excluded, cannot be overridden)
var hardExcludes = []string{
	".s2",
}

// isHardExcluded checks paths that are always excluded regardless of .s2ignore.
func isHardExcluded(rel string) bool {
	base := filepath.Base(rel)
	// .s2 directory
	if base == ".s2" || rel == ".s2" || strings.HasPrefix(rel, ".s2/") {
		return true
	}
	// .sync-conflict-* files
	if strings.Contains(base, ".sync-conflict-") {
		return true
	}
	return false
}

// DefaultExclude returns an exclude function using built-in patterns.
func DefaultExclude() func(string) bool {
	return matchesAny(defaultExcludes)
}

// LoadExclude returns an exclude function that combines default patterns
// with patterns from .s2ignore file (if it exists).
func LoadExclude(syncRoot string) func(string) bool {
	patterns := make([]string, len(defaultExcludes))
	copy(patterns, defaultExcludes)

	ignorePath := filepath.Join(syncRoot, ".s2ignore")
	if f, err := os.Open(ignorePath); err == nil {
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			patterns = append(patterns, line)
		}
	}

	matcher := matchesAny(patterns)
	return func(path string) bool {
		if isHardExcluded(path) {
			return true
		}
		return matcher(path)
	}
}

func matchesAny(patterns []string) func(string) bool {
	return func(path string) bool {
		if isHardExcluded(path) {
			return true
		}
		base := filepath.Base(path)
		for _, p := range patterns {
			// Match against basename
			if matched, _ := filepath.Match(p, base); matched {
				return true
			}
			// Match against first component (for directory patterns like .git)
			parts := strings.SplitN(filepath.ToSlash(path), "/", 2)
			if matched, _ := filepath.Match(p, parts[0]); matched {
				return true
			}
		}
		return false
	}
}
