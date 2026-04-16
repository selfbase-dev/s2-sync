package sync

import (
	"os"
	"path/filepath"
	"strings"
)

// walkLocalUnderPrefix enumerates regular-file relative paths under
// the given prefix. Empty prefix means scope-wide (the entire localDir
// minus .s2/).
func walkLocalUnderPrefix(localDir, prefix string) ([]string, error) {
	var walkRoot string
	if prefix == "" {
		walkRoot = localDir
	} else {
		var err error
		walkRoot, err = safeJoin(localDir, strings.TrimSuffix(prefix, "/"))
		if err != nil {
			return nil, err
		}
	}

	info, err := os.Stat(walkRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, nil
	}

	var out []string
	err = filepath.Walk(walkRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(localDir, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if rel == ".s2" || strings.HasPrefix(rel, ".s2/") {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
