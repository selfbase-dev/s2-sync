package sync

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/selfbase-dev/s2-cli/internal/types"
)

// fakeLister is a test double for client.Client that returns a fixed
// map keyed by the absolute prefix passed to ListAllRecursive.
type fakeLister struct {
	// responses maps an exact prefix → files relative to that prefix.
	// Use "" for a listing at the top (client root + remotePrefix).
	responses map[string]map[string]types.RemoteFile
	// calls records every prefix passed to ListAllRecursive in order.
	calls []string
}

func (f *fakeLister) ListAllRecursive(prefix string) (map[string]types.RemoteFile, error) {
	f.calls = append(f.calls, prefix)
	if r, ok := f.responses[prefix]; ok {
		return r, nil
	}
	return map[string]types.RemoteFile{}, nil
}

// --- ExpandDirEvents: delete (is_dir) ---

func TestExpandDirEvents_DeleteIsDir_ArchivePrefixMatch(t *testing.T) {
	archive := map[string]types.FileState{
		"docs/a.txt":         {LocalHash: "h1"},
		"docs/sub/b.txt":     {LocalHash: "h2"},
		"other/c.txt":        {LocalHash: "h3"},
		"docsnotaprefix.txt": {LocalHash: "h4"}, // must NOT match "docs/"
	}
	events := []types.ChangeEntry{
		{Seq: 10, Action: "delete", PathBefore: "/docs", IsDir: true},
	}
	got, dirOps, err := ExpandDirEvents(events, archive, &fakeLister{}, "")
	if err != nil {
		t.Fatalf("ExpandDirEvents: %v", err)
	}
	// delete(is_dir) must emit exactly one DirOpRmdir for the target.
	if len(dirOps) != 1 || dirOps[0].Kind != DirOpRmdir || dirOps[0].Path != "docs" {
		t.Errorf("dirOps = %+v, want [{Rmdir docs}]", dirOps)
	}

	var paths []string
	for _, e := range got {
		if e.Action != "delete" {
			t.Errorf("action = %q, want delete", e.Action)
		}
		if e.Seq != 10 {
			t.Errorf("seq inherited wrong: got %d", e.Seq)
		}
		if e.IsDir {
			t.Error("synthetic entry should be is_dir=false")
		}
		paths = append(paths, e.PathBefore)
	}
	sort.Strings(paths)
	want := []string{"/docs/a.txt", "/docs/sub/b.txt"}
	if !equalStrings(paths, want) {
		t.Errorf("paths = %v, want %v", paths, want)
	}
}

func TestExpandDirEvents_DeleteRoot_MatchesEverything(t *testing.T) {
	archive := map[string]types.FileState{
		"a.txt":       {LocalHash: "h1"},
		"sub/b.txt":   {LocalHash: "h2"},
		"deep/c/d.md": {LocalHash: "h3"},
	}
	events := []types.ChangeEntry{
		{Seq: 20, Action: "delete", PathBefore: "/", IsDir: true},
	}
	got, _, err := ExpandDirEvents(events, archive, &fakeLister{}, "")
	if err != nil {
		t.Fatalf("ExpandDirEvents: %v", err)
	}
	if len(got) != len(archive) {
		t.Errorf("got %d synthetic entries, want %d (archive size)", len(got), len(archive))
	}
}

// --- ExpandDirEvents: put (is_dir) ---

func TestExpandDirEvents_PutIsDir_ListsRemoteAndSynthesizes(t *testing.T) {
	lister := &fakeLister{
		responses: map[string]map[string]types.RemoteFile{
			"docs/": {
				"a.txt":     {Size: 1},
				"sub/b.txt": {Size: 2},
			},
		},
	}
	events := []types.ChangeEntry{
		{Seq: 30, TokenID: "tk1", Action: "put", PathAfter: "/docs", IsDir: true},
	}
	got, _, err := ExpandDirEvents(events, nil, lister, "")
	if err != nil {
		t.Fatalf("ExpandDirEvents: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d entries, want 2", len(got))
	}
	paths := make([]string, 0, len(got))
	for _, e := range got {
		if e.Action != "put" {
			t.Errorf("action = %q, want put", e.Action)
		}
		if e.Seq != 30 || e.TokenID != "tk1" {
			t.Errorf("seq/token not inherited: %+v", e)
		}
		paths = append(paths, e.PathAfter)
	}
	sort.Strings(paths)
	want := []string{"/docs/a.txt", "/docs/sub/b.txt"}
	if !equalStrings(paths, want) {
		t.Errorf("paths = %v, want %v", paths, want)
	}
}

func TestExpandDirEvents_PutRoot_ListsFromRemotePrefix(t *testing.T) {
	// base_path = "/photos/" → remotePrefix = "photos/"
	// put / (client root) → list "photos/" on the server
	lister := &fakeLister{
		responses: map[string]map[string]types.RemoteFile{
			"photos/": {
				"a.jpg": {Size: 1},
			},
		},
	}
	events := []types.ChangeEntry{
		{Seq: 40, Action: "put", PathAfter: "/", IsDir: true},
	}
	got, _, err := ExpandDirEvents(events, nil, lister, "photos/")
	if err != nil {
		t.Fatalf("ExpandDirEvents: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if got[0].PathAfter != "/a.jpg" {
		t.Errorf("path = %q, want /a.jpg", got[0].PathAfter)
	}
	if len(lister.calls) != 1 || lister.calls[0] != "photos/" {
		t.Errorf("list calls = %v, want [photos/]", lister.calls)
	}
}

// --- ExpandDirEvents: move (is_dir) → decompose to delete + put ---

func TestExpandDirEvents_MoveIsDir_DecomposesToDeletePlusPut(t *testing.T) {
	archive := map[string]types.FileState{
		"a/x.txt": {LocalHash: "h1"},
	}
	lister := &fakeLister{
		responses: map[string]map[string]types.RemoteFile{
			"b/": {
				"x.txt": {Size: 1},
			},
		},
	}
	events := []types.ChangeEntry{
		{Seq: 50, Action: "move", PathBefore: "/a", PathAfter: "/b", IsDir: true},
	}
	got, _, err := ExpandDirEvents(events, archive, lister, "")
	if err != nil {
		t.Fatalf("ExpandDirEvents: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	// Expect one delete and one put.
	var seenDelete, seenPut bool
	for _, e := range got {
		switch e.Action {
		case "delete":
			if e.PathBefore != "/a/x.txt" {
				t.Errorf("delete path = %q, want /a/x.txt", e.PathBefore)
			}
			seenDelete = true
		case "put":
			if e.PathAfter != "/b/x.txt" {
				t.Errorf("put path = %q, want /b/x.txt", e.PathAfter)
			}
			seenPut = true
		}
	}
	if !seenDelete || !seenPut {
		t.Errorf("missing delete (%v) or put (%v)", seenDelete, seenPut)
	}
}

// --- ExpandDirEvents: mkdir → collected as empty-dir path ---

func TestExpandDirEvents_MkdirIsDir_CollectsMkdirs(t *testing.T) {
	events := []types.ChangeEntry{
		{Seq: 60, Action: "mkdir", PathAfter: "/empty", IsDir: true},
		{Seq: 61, Action: "mkdir", PathAfter: "/nested/empty", IsDir: true},
	}
	fileEvents, dirOps, err := ExpandDirEvents(events, nil, &fakeLister{}, "")
	if err != nil {
		t.Fatalf("ExpandDirEvents: %v", err)
	}
	if len(fileEvents) != 0 {
		t.Errorf("mkdir should not produce file events, got %d", len(fileEvents))
	}
	if len(dirOps) != 2 {
		t.Fatalf("dirOps = %+v, want 2 entries", dirOps)
	}
	for i, want := range []struct {
		kind DirOpKind
		path string
	}{{DirOpMkdir, "empty"}, {DirOpMkdir, "nested/empty"}} {
		if dirOps[i].Kind != want.kind || dirOps[i].Path != want.path {
			t.Errorf("dirOps[%d] = %+v, want %+v", i, dirOps[i], want)
		}
	}
}

// --- ExpandDirEvents: non is_dir events pass through unchanged ---

func TestExpandDirEvents_FileEventsPassThrough(t *testing.T) {
	events := []types.ChangeEntry{
		{Seq: 70, Action: "put", PathAfter: "/a.txt"},
		{Seq: 71, Action: "delete", PathBefore: "/b.txt"},
	}
	got, _, err := ExpandDirEvents(events, nil, &fakeLister{}, "")
	if err != nil {
		t.Fatalf("ExpandDirEvents: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2 passthrough", len(got))
	}
}

// --- ApplyDirSideEffects ---

func TestApplyDirSideEffects_Mkdir_CreatesEmptyDirs(t *testing.T) {
	local := t.TempDir()
	ApplyDirSideEffects(discardWriter{}, local, []DirOp{
		{Kind: DirOpMkdir, Path: "empty"},
		{Kind: DirOpMkdir, Path: "nested/empty"},
	})

	for _, rel := range []string{"empty", "nested/empty"} {
		p := filepath.Join(local, filepath.FromSlash(rel))
		info, err := os.Stat(p)
		if err != nil {
			t.Errorf("dir %q not created: %v", rel, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%q is not a directory", rel)
		}
	}
}

func TestApplyDirSideEffects_Rmdir_RemovesEmptyDir(t *testing.T) {
	// Pure empty-dir delete: mkdir /empty → delete /empty must leave
	// nothing behind. (Codex review finding — first fix round.)
	local := t.TempDir()
	if err := os.MkdirAll(filepath.Join(local, "empty"), 0755); err != nil {
		t.Fatal(err)
	}

	ApplyDirSideEffects(discardWriter{}, local, []DirOp{
		{Kind: DirOpRmdir, Path: "empty"},
	})

	if _, err := os.Stat(filepath.Join(local, "empty")); !os.IsNotExist(err) {
		t.Errorf("empty dir should be removed, err = %v", err)
	}
}

func TestApplyDirSideEffects_Rmdir_NestedEmptyDirs(t *testing.T) {
	// delete /a with layout a/b/c (all empty) removes the whole subtree
	// bottom-up via os.Remove.
	local := t.TempDir()
	if err := os.MkdirAll(filepath.Join(local, "a/b/c"), 0755); err != nil {
		t.Fatal(err)
	}

	ApplyDirSideEffects(discardWriter{}, local, []DirOp{
		{Kind: DirOpRmdir, Path: "a"},
	})

	if _, err := os.Stat(filepath.Join(local, "a")); !os.IsNotExist(err) {
		t.Errorf("dir 'a' should be removed, err = %v", err)
	}
}

func TestApplyDirSideEffects_Rmdir_NonEmptyDirsRemain(t *testing.T) {
	// delete /a where a/keep.txt still exists (untracked user file or a
	// file the sync hasn't removed yet) must leave the tree alone.
	local := t.TempDir()
	if err := os.MkdirAll(filepath.Join(local, "a/sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "a/keep.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	ApplyDirSideEffects(discardWriter{}, local, []DirOp{
		{Kind: DirOpRmdir, Path: "a"},
	})

	// a/sub was empty so it should be gone.
	if _, err := os.Stat(filepath.Join(local, "a/sub")); !os.IsNotExist(err) {
		t.Errorf("empty child 'a/sub' should be removed, err = %v", err)
	}
	// a still has keep.txt, so os.Remove fails and the dir stays.
	if _, err := os.Stat(filepath.Join(local, "a/keep.txt")); err != nil {
		t.Errorf("untracked 'a/keep.txt' should remain, err = %v", err)
	}
}

func TestApplyDirSideEffects_Rmdir_RootDoesNotDeleteLocalDir(t *testing.T) {
	// `delete /` (DirOpRmdir with Path="") must never try to remove the
	// user's sync root itself, even if it's empty.
	local := t.TempDir()

	ApplyDirSideEffects(discardWriter{}, local, []DirOp{
		{Kind: DirOpRmdir, Path: ""},
	})

	if _, err := os.Stat(local); err != nil {
		t.Errorf("sync root must remain, err = %v", err)
	}
}

func TestApplyDirSideEffects_Order_MkdirAfterRmdir_MkdirWins(t *testing.T) {
	// Codex review finding (second fix round): a `delete /x` followed
	// by a `mkdir /x` in the same poll must leave /x in place — the
	// later event wins. Regression guard for the fixed-order bug.
	local := t.TempDir()
	if err := os.MkdirAll(filepath.Join(local, "x"), 0755); err != nil {
		t.Fatal(err)
	}

	ApplyDirSideEffects(discardWriter{}, local, []DirOp{
		{Kind: DirOpRmdir, Path: "x"},
		{Kind: DirOpMkdir, Path: "x"},
	})

	if info, err := os.Stat(filepath.Join(local, "x")); err != nil || !info.IsDir() {
		t.Errorf("x should exist after mkdir-after-rmdir, err = %v", err)
	}
}

func TestApplyDirSideEffects_Order_RmdirAfterMkdir_RmdirWins(t *testing.T) {
	// Opposite ordering: mkdir then rmdir. The dir should be removed.
	local := t.TempDir()

	ApplyDirSideEffects(discardWriter{}, local, []DirOp{
		{Kind: DirOpMkdir, Path: "y"},
		{Kind: DirOpRmdir, Path: "y"},
	})

	if _, err := os.Stat(filepath.Join(local, "y")); !os.IsNotExist(err) {
		t.Errorf("y should be removed, err = %v", err)
	}
}

// --- helpers ---

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Make sure fakeLister satisfies the Dirlister interface at compile time.
var _ Dirlister = (*fakeLister)(nil)
