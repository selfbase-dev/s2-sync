package sync

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// defaultIgnoreContent is written to .s2ignore when it does not exist.
// Users can edit this file to customize which files are excluded from sync.
const defaultIgnoreContent = `# S2 ignore file — edit to customize sync exclusions.
# Patterns use filepath.Match syntax (e.g. *.log, build/, docs/drafts).
# Lines starting with # are comments.

# OS / editor junk
.DS_Store
Thumbs.db
*.swp
*.swo
*~
`

// defaultExcludes are always applied regardless of .s2ignore content.
// These are macOS metadata files that should never be synced.
var defaultExcludes = []string{
	"._*",
	"desktop.ini",
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

// EnsureIgnoreFile creates a default .s2ignore in syncRoot if one does not exist.
func EnsureIgnoreFile(syncRoot string) error {
	ignorePath := filepath.Join(syncRoot, ".s2ignore")
	if _, err := os.Stat(ignorePath); err == nil {
		return nil // already exists
	}
	return os.WriteFile(ignorePath, []byte(defaultIgnoreContent), 0600)
}

// DefaultExclude returns an exclude function using built-in patterns only.
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
