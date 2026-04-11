// Package sync — safeJoin: defensive path composition.
//
// Every function that turns a server-supplied path string into a
// concrete local file path must route through safeJoin so that a
// malicious or buggy server cannot trick the CLI into writing outside
// the sync root (directory escape), overwriting absolute-path targets,
// or otherwise violating the sync boundary. Even though the server is
// trusted, this is defence-in-depth — per KP1, don't patch the symptom
// (rely on server correctness), fix the root cause (validate untrusted
// input at the boundary).

package sync

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ErrUnsafePath is returned by safeJoin when the input contains a
// traversal component, absolute path, null byte, or otherwise escapes
// the sync root. Callers should treat this as a protocol-violation
// class error: skip the offending entry and log.
type UnsafePathError struct {
	RawPath string
	Reason  string
}

func (e *UnsafePathError) Error() string {
	return fmt.Sprintf("unsafe sync path %q: %s", e.RawPath, e.Reason)
}

// safeJoin validates a server-derived relative path and joins it with
// the sync root. It rejects:
//
//   - null bytes (can truncate the path on some syscalls)
//   - absolute paths (would bypass root)
//   - ".." components (directory traversal)
//   - Windows drive prefixes / UNC paths
//   - post-clean results that escape the root
//
// The returned path is an absolute cleaned path guaranteed to be under
// `root`. Empty / root-only input is rejected too because the caller
// is always trying to address a file.
func safeJoin(root, rel string) (string, error) {
	// Normalise first: forward slashes → OS separators. Strip any
	// leading slashes because the server uses absolute-client paths
	// like "/docs/a.txt" and we want to treat them as relative.
	rel = strings.TrimLeft(rel, "/")
	if rel == "" {
		return "", &UnsafePathError{RawPath: rel, Reason: "empty path"}
	}
	if strings.ContainsRune(rel, 0) {
		return "", &UnsafePathError{RawPath: rel, Reason: "contains null byte"}
	}
	// Windows drive letters and UNC prefixes.
	if filepath.IsAbs(rel) || (len(rel) >= 2 && rel[1] == ':') || strings.HasPrefix(rel, `\\`) {
		return "", &UnsafePathError{RawPath: rel, Reason: "absolute path rejected"}
	}
	// Reject traversal components before the OS-level Clean can
	// silently collapse them.
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", &UnsafePathError{RawPath: rel, Reason: "contains .."}
		}
	}

	osRel := filepath.FromSlash(rel)
	joined := filepath.Join(root, osRel)
	cleanRoot := filepath.Clean(root)

	// Re-check after Clean: ensure the joined path is STILL under
	// cleanRoot. This catches anything a crafty OS-specific corner
	// case might slip past the string-level checks above.
	rels, err := filepath.Rel(cleanRoot, joined)
	if err != nil {
		return "", &UnsafePathError{RawPath: rel, Reason: err.Error()}
	}
	if rels == "." || rels == "" {
		return "", &UnsafePathError{RawPath: rel, Reason: "resolves to sync root"}
	}
	if strings.HasPrefix(rels, "..") {
		return "", &UnsafePathError{RawPath: rel, Reason: "escapes sync root after clean"}
	}

	return joined, nil
}

// safeRelPrefix normalises a server-derived directory prefix used by
// archive walks. It strips leading slashes, ensures the prefix ends
// with a trailing slash, and rejects traversal components. Returns ""
// (match-all) when the input is empty / "/" (scope-root), which is a
// legitimate case for `delete /`.
func safeRelPrefix(rel string) (string, error) {
	rel = strings.TrimLeft(rel, "/")
	if rel == "" {
		return "", nil // scope-root, match everything
	}
	if strings.ContainsRune(rel, 0) {
		return "", &UnsafePathError{RawPath: rel, Reason: "contains null byte"}
	}
	if filepath.IsAbs(rel) || (len(rel) >= 2 && rel[1] == ':') || strings.HasPrefix(rel, `\\`) {
		return "", &UnsafePathError{RawPath: rel, Reason: "absolute path rejected"}
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", &UnsafePathError{RawPath: rel, Reason: "contains .."}
		}
	}
	if !strings.HasSuffix(rel, "/") {
		rel += "/"
	}
	return rel, nil
}
