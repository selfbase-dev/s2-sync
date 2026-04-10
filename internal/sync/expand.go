package sync

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/selfbase-dev/s2-cli/internal/types"
)

// Dirlister is the subset of *client.Client used by ExpandDirEvents.
// Kept as an interface so tests can inject a fake without spinning up
// a real HTTP client.
type Dirlister interface {
	ListAllRecursive(prefix string) (map[string]types.RemoteFile, error)
}

// DirOp is a directory-level side effect emitted by ExpandDirEvents and
// applied by ApplyDirSideEffects. Keeping them in a single ordered slice
// preserves the original event order, so `delete /x` followed by
// `mkdir /x` in the same poll correctly leaves /x in place.
type DirOp struct {
	Kind DirOpKind
	// Path is client-relative with no leading slash. The empty string
	// represents the sync root (seen with ancestor-transformed
	// `delete /`). ApplyDirSideEffects refuses to remove the sync root.
	Path string
}

// DirOpKind enumerates the directory-level operations ApplyDirSideEffects
// knows how to apply.
type DirOpKind int

const (
	// DirOpMkdir creates an empty local directory (os.MkdirAll).
	DirOpMkdir DirOpKind = iota
	// DirOpRmdir walks the subtree at Path bottom-up and removes every
	// empty directory it finds. Non-empty dirs are left alone.
	DirOpRmdir
)

// ExpandDirEvents expands is_dir change events into file-level events
// plus the directory-level side effects the caller must apply after
// executing sync plans (empty-dir cleanup, empty-dir creation).
//
// ADR 0038: the server emits directory-level intent events (delete, put,
// move, mkdir). The CLI's compare pipeline operates on files, so we
// expand each directory event into the file-level effects it implies,
// using the local archive (for deletes) and ListAllRecursive (for puts).
//
// - delete (is_dir): synthesize delete entries for every archive path
//   under the deleted directory, and record the directory in dirDeletes
//   so ApplyDirSideEffects can prune now-empty local directories
//   (including empty dirs that had no tracked files at all).
// - put (is_dir): list the remote directory and synthesize put entries
//   for every remote file found.
// - move (is_dir): decompose as delete(path_before) + put(path_after).
//   Less efficient than an in-place rename, but correct and simple —
//   the server has already performed the move, so listing path_after
//   returns the new contents.
// - mkdir (is_dir): return the path in mkdirs so the caller can create
//   an empty local directory.
//
// Non is_dir events pass through unchanged.
//
// `remotePrefix` is the absolute-path prefix the client prepends to
// client-relative paths before calling the server (base_path with no
// leading slash, matching what the rest of sync uses).
func ExpandDirEvents(
	events []types.ChangeEntry,
	archive map[string]types.FileState,
	c Dirlister,
	remotePrefix string,
) (fileEvents []types.ChangeEntry, dirOps []DirOp, err error) {
	for _, ev := range events {
		if !ev.IsDir {
			fileEvents = append(fileEvents, ev)
			continue
		}
		switch ev.Action {
		case "delete":
			fileEvents = append(fileEvents, expandDirDelete(ev, archive)...)
			dirOps = append(dirOps, DirOp{
				Kind: DirOpRmdir,
				Path: stripLeadingSlash(ev.PathBefore),
			})
		case "put":
			expanded, err := expandDirPut(ev, c, remotePrefix)
			if err != nil {
				return nil, nil, err
			}
			fileEvents = append(fileEvents, expanded...)
		case "move":
			// Decompose move into delete(path_before) + put(path_after).
			fileEvents = append(fileEvents, expandDirDelete(ev, archive)...)
			dirOps = append(dirOps, DirOp{
				Kind: DirOpRmdir,
				Path: stripLeadingSlash(ev.PathBefore),
			})
			expanded, err := expandDirPut(ev, c, remotePrefix)
			if err != nil {
				return nil, nil, err
			}
			fileEvents = append(fileEvents, expanded...)
		case "mkdir":
			if p := stripLeadingSlash(ev.PathAfter); p != "" {
				dirOps = append(dirOps, DirOp{Kind: DirOpMkdir, Path: p})
			}
		}
	}
	return fileEvents, dirOps, nil
}

// expandDirDelete walks the archive for paths under the deleted directory
// and returns a synthetic delete entry for each matching file.
//
// "/" (empty after stripping) matches every archive entry — used when the
// server signals that the client's entire view has disappeared.
func expandDirDelete(
	ev types.ChangeEntry,
	archive map[string]types.FileState,
) []types.ChangeEntry {
	dir := stripLeadingSlash(ev.PathBefore)
	var out []types.ChangeEntry
	for p := range archive {
		if !pathUnderDir(p, dir) {
			continue
		}
		out = append(out, types.ChangeEntry{
			Seq:     ev.Seq,
			TokenID: ev.TokenID,
			Action:  "delete",
			// Restore leading slash: CompareIncremental strips it back off.
			PathBefore: "/" + p,
			IsDir:      false,
		})
	}
	return out
}

// expandDirPut lists the remote directory and returns a synthetic put
// entry for each remote file found under it.
func expandDirPut(
	ev types.ChangeEntry,
	c Dirlister,
	remotePrefix string,
) ([]types.ChangeEntry, error) {
	dir := stripLeadingSlash(ev.PathAfter)
	// Absolute prefix passed to the server: base_path + client dir.
	// Must end with "/" (or be empty for root) so ListDir treats it as
	// a directory listing.
	absPrefix := remotePrefix + dir
	if absPrefix != "" && !strings.HasSuffix(absPrefix, "/") {
		absPrefix += "/"
	}

	files, err := c.ListAllRecursive(absPrefix)
	if err != nil {
		return nil, fmt.Errorf("expand put %q: %w", ev.PathAfter, err)
	}

	out := make([]types.ChangeEntry, 0, len(files))
	for relPath := range files {
		// Full client-relative path. If dir is empty (top-level put /),
		// relPath is already the full client path.
		full := relPath
		if dir != "" {
			full = dir + "/" + relPath
		}
		out = append(out, types.ChangeEntry{
			Seq:       ev.Seq,
			TokenID:   ev.TokenID,
			Action:    "put",
			PathAfter: "/" + full,
			IsDir:     false,
		})
	}
	return out, nil
}

// stripLeadingSlash strips a single leading "/" from a client-relative path.
// Server-emitted paths always start with "/"; archive keys and plan paths
// never do.
func stripLeadingSlash(p string) string {
	return strings.TrimPrefix(p, "/")
}

// pathUnderDir reports whether `path` is inside the directory `dir`
// (or equals it). An empty `dir` matches every path — this represents
// the "entire client scope" case, signalled by the server as PathBefore = "/".
func pathUnderDir(path, dir string) bool {
	if dir == "" {
		return true
	}
	return path == dir || strings.HasPrefix(path, dir+"/")
}

// ApplyDirSideEffects applies directory-level state changes in the
// order the server emitted them: creating empty directories for mkdir
// events, and pruning empty subtrees for dir-delete events. Preserving
// order matters — in a single poll, `delete /x` followed by `mkdir /x`
// must leave /x in place (the mkdir wins).
//
// For DirOpRmdir we walk the local subtree bottom-up and os.Remove
// every empty directory — non-empty dirs (untracked files, or files
// the sync isn't done with yet) are left alone because os.Remove fails
// on them. The sync root itself is always preserved, so an ancestor
// `delete /` (DirOp.Path == "") cleans the tree without removing the
// user's sync directory.
//
// All operations are best-effort: logged failures don't propagate.
func ApplyDirSideEffects(w io.Writer, localDir string, ops []DirOp) {
	for _, op := range ops {
		switch op.Kind {
		case DirOpMkdir:
			full := filepath.Join(localDir, filepath.FromSlash(op.Path))
			if err := os.MkdirAll(full, 0755); err != nil {
				fmt.Fprintf(w, "mkdir %s: %v\n", op.Path, err)
			}
		case DirOpRmdir:
			root := localDir
			if op.Path != "" {
				root = filepath.Join(localDir, filepath.FromSlash(op.Path))
			}
			removeEmptyDirsBottomUp(root, localDir)
		}
	}
}

// removeEmptyDirsBottomUp walks `root` and removes every empty
// directory it finds, deepest-first. Leaves non-empty directories and
// any files untouched. Never removes `keep` itself (the local sync
// root) so `dirDeletes = []string{""}` doesn't try to os.Remove the
// user's sync directory.
func removeEmptyDirsBottomUp(root, keep string) {
	var dirs []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Root missing, or transient read error — nothing to clean here.
			return nil
		}
		if info.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	// Sort deepest-first so "a/b/c" is processed before "a/b".
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		if d == keep {
			continue
		}
		// os.Remove only removes empty directories — exactly the
		// semantics we want; non-empty dirs are left alone.
		_ = os.Remove(d)
	}
}
