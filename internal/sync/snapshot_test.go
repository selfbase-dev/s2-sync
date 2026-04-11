package sync

import (
	"testing"

	"github.com/selfbase-dev/s2-cli/internal/types"
)

func int64Ptr(v int64) *int64 { return &v }

func TestSnapshotToRemoteFiles_FiltersDirsAndStripsLeadingSlash(t *testing.T) {
	items := []types.SnapshotItem{
		{Path: "/docs/", Type: "dir"},
		{Path: "/docs/a.txt", Type: "file", ContentVersion: 3, RevisionID: "rev_a", Size: int64Ptr(11), Hash: "h-a"},
		{Path: "/photos/", Type: "dir"},
		{Path: "/photos/cat.jpg", Type: "file", ContentVersion: 1, RevisionID: "rev_c", Size: int64Ptr(512), Hash: "h-c"},
	}

	remote := SnapshotToRemoteFiles(items)
	if len(remote) != 2 {
		t.Fatalf("got %d files, want 2 (dirs should be dropped)", len(remote))
	}
	a, ok := remote["docs/a.txt"]
	if !ok {
		t.Fatal(`missing "docs/a.txt" (leading slash should be stripped)`)
	}
	if a.RevisionID != "rev_a" || a.Hash != "h-a" || a.ContentVersion != 3 || a.Size != 11 {
		t.Errorf("docs/a.txt = %+v", a)
	}
	if a.Name != "a.txt" {
		t.Errorf("name = %q, want %q", a.Name, "a.txt")
	}
	if _, ok := remote["docs/"]; ok {
		t.Errorf("dir entry should not be present")
	}
}

func TestSnapshotToRemoteFiles_EmptyAndRootPaths(t *testing.T) {
	items := []types.SnapshotItem{
		{Path: "/top.txt", Type: "file", ContentVersion: 1, RevisionID: "rev_top", Hash: "h-top"},
	}
	remote := SnapshotToRemoteFiles(items)
	top, ok := remote["top.txt"]
	if !ok {
		t.Fatal("missing top.txt")
	}
	if top.Name != "top.txt" {
		t.Errorf("name = %q", top.Name)
	}
}

func TestPrefillArchiveForIdempotentApply_FillsMatches(t *testing.T) {
	archive := map[string]types.FileState{}
	local := map[string]types.LocalFile{
		"docs/a.txt":  {Hash: "h-a", Size: 10},
		"docs/b.txt":  {Hash: "h-b", Size: 20},
		"docs/new.md": {Hash: "h-new", Size: 5},
	}
	remote := map[string]types.RemoteFile{
		"docs/a.txt":     {Hash: "h-a", RevisionID: "rev_a", ContentVersion: 2},
		"docs/b.txt":     {Hash: "h-b-different", RevisionID: "rev_b", ContentVersion: 1},
		"docs/remote.md": {Hash: "h-r", RevisionID: "rev_r", ContentVersion: 1},
	}

	added := PrefillArchiveForIdempotentApply(archive, local, remote)
	if added != 1 {
		t.Fatalf("added = %d, want 1 (only docs/a.txt hashes match)", added)
	}
	if _, ok := archive["docs/a.txt"]; !ok {
		t.Errorf("docs/a.txt should be in archive after prefill")
	}
	if _, ok := archive["docs/b.txt"]; ok {
		t.Errorf("docs/b.txt should NOT be in archive (hash differs)")
	}
	if _, ok := archive["docs/new.md"]; ok {
		t.Errorf("docs/new.md should NOT be in archive (no remote entry)")
	}
}

func TestPrefillArchiveForIdempotentApply_DoesNotOverwriteExisting(t *testing.T) {
	archive := map[string]types.FileState{
		"docs/a.txt": {LocalHash: "h-old", ContentVersion: 1},
	}
	local := map[string]types.LocalFile{
		"docs/a.txt": {Hash: "h-a"},
	}
	remote := map[string]types.RemoteFile{
		"docs/a.txt": {Hash: "h-a", ContentVersion: 5},
	}

	added := PrefillArchiveForIdempotentApply(archive, local, remote)
	if added != 0 {
		t.Fatalf("added = %d, want 0 (archive entry already exists)", added)
	}
	if archive["docs/a.txt"].LocalHash != "h-old" {
		t.Errorf("existing archive entry was overwritten")
	}
}

func TestPrefillArchiveForIdempotentApply_SkipsEmptyRemoteHash(t *testing.T) {
	archive := map[string]types.FileState{}
	local := map[string]types.LocalFile{
		"docs/a.txt": {Hash: "h-a"},
	}
	// Legacy ListDir path: no hash on RemoteFile. Prefill must not
	// treat empty-vs-anything as a match.
	remote := map[string]types.RemoteFile{
		"docs/a.txt": {Hash: "", ContentVersion: 1},
	}

	added := PrefillArchiveForIdempotentApply(archive, local, remote)
	if added != 0 {
		t.Errorf("added = %d, want 0 (empty remote hash must not match)", added)
	}
}
