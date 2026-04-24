package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/selfbase-dev/s2-sync/internal/types"
)

// CollisionGroup describes a set of paths that fold to the same
// canonical key (per ADR 0053). On case-insensitive filesystems they
// can't coexist, so only Paths[0] (lexicographic first) is included
// in the sync set; the rest get SkipCaseConflict and warning.
type CollisionGroup struct {
	Key   string   // FoldKey
	Paths []string // NFC-canonical paths, sorted UTF-8 bytewise
}

// WalkResult captures the outcome of a local walk.
type WalkResult struct {
	// Files maps NFC-canonical path → LocalFile. On collision, only
	// the lexicographic-first path is present.
	Files map[string]types.LocalFile
	// Collisions lists any groups where multiple on-disk paths folded
	// to the same canonical key (ADR 0053 key concept 4: sync does not
	// stop, caller emits warnings).
	Collisions []CollisionGroup
}

// Walk recursively scans the local directory. Paths are NFC-normalized
// (ADR 0053 key concept 5: idempotent convergence across macOS NFD and
// other-OS NFC). If archive is provided, files whose modtime and size
// match are reused without rehashing (like rsync).
//
// Skips .s2/ and paths matching exclude.
func Walk(root string, archive map[string]types.FileState, exclude func(string) bool) (WalkResult, error) {
	// buckets: FoldKey → canonical path → LocalFile.
	// A bucket with >1 entries indicates a case/unicode collision.
	type entry struct {
		file      types.LocalFile
		canonical string
	}
	buckets := make(map[string][]entry)

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

		canonical := NormalizePath(rel)
		key := FoldKey(rel)

		hash, err := hashFile(path)
		if err != nil {
			return err
		}

		buckets[key] = append(buckets[key], entry{
			canonical: canonical,
			file: types.LocalFile{
				Hash:    hash,
				Size:    info.Size(),
				ModTime: info.ModTime().Unix(),
			},
		})
		return nil
	})
	if err != nil {
		return WalkResult{}, err
	}

	result := WalkResult{
		Files: make(map[string]types.LocalFile),
	}
	for key, es := range buckets {
		if len(es) == 1 {
			result.Files[es[0].canonical] = es[0].file
			continue
		}
		// Collision — sort deterministically, include first in Files
		sort.Slice(es, func(i, j int) bool { return es[i].canonical < es[j].canonical })
		paths := make([]string, len(es))
		for i, e := range es {
			paths[i] = e.canonical
		}
		result.Files[es[0].canonical] = es[0].file
		result.Collisions = append(result.Collisions, CollisionGroup{Key: key, Paths: paths})
	}
	sort.Slice(result.Collisions, func(i, j int) bool {
		return result.Collisions[i].Key < result.Collisions[j].Key
	})
	return result, nil
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
