package sync

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// Default exclude patterns
var defaultExcludes = []string{
	".git",
	"node_modules",
	".DS_Store",
	"Thumbs.db",
	"*.swp",
	"*.swo",
	"*~",
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

	return matchesAny(patterns)
}

func matchesAny(patterns []string) func(string) bool {
	return func(path string) bool {
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
