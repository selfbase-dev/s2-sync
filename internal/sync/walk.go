package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/selfbase-dev/s2-cli/internal/types"
)

// Walk recursively scans the local directory and returns a map of relative path → LocalFile.
// Skips .s2/ and paths matching the exclude function.
// If archive is provided, files whose modtime and size match the archive are
// reused without rehashing (like rsync).
func Walk(root string, archive map[string]types.FileState, exclude func(string) bool) (map[string]types.LocalFile, error) {
	result := make(map[string]types.LocalFile)

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		// Normalize to forward slashes
		rel = filepath.ToSlash(rel)

		// Skip root itself
		if rel == "." {
			return nil
		}

		// Skip .s2 directory
		if rel == ".s2" || strings.HasPrefix(rel, ".s2/") {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip excluded paths
		if exclude != nil && exclude(rel) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip directories (we only track files)
		if info.IsDir() {
			return nil
		}

		hash, err := hashFile(path)
		if err != nil {
			return err
		}

		result[rel] = types.LocalFile{
			Hash:    hash,
			Size:    info.Size(),
			ModTime: info.ModTime().Unix(),
		}
		return nil
	})

	return result, err
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
