package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/selfbase-dev/s2-cli/internal/types"
)

// --- helpers ---

func writeLocalFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func writeLocalFileExpectHash(t *testing.T, dir, rel, content string) string {
	t.Helper()
	writeLocalFile(t, dir, rel, content)
	h, err := hashFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func exists(t *testing.T, dir, rel string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
	return err == nil
}

// --- normalize helpers ---

func TestNormalizeDirPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/", ""},
		{"/foo", "foo"},
		{"/foo/", "foo"},
		{"foo/bar/", "foo/bar"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := normalizeDirPath(tc.in); got != tc.want {
			t.Errorf("normalizeDirPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeDirPrefix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/", ""},
		{"", ""},
		{"/foo", "foo/"},
		{"/foo/", "foo/"},
		{"foo/bar", "foo/bar/"},
	}
	for _, tc := range cases {
		if got := normalizeDirPrefix(tc.in); got != tc.want {
			t.Errorf("normalizeDirPrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- expandArchiveDelete ---

func TestExpandArchiveDelete_UnchangedLocal(t *testing.T) {
	dir := t.TempDir()
	h := writeLocalFileExpectHash(t, dir, "docs/a.txt", "hello")
	writeLocalFileExpectHash(t, dir, "photos/b.jpg", "other")

	archive := map[string]types.FileState{
		"docs/a.txt":   {LocalHash: h},
		"photos/b.jpg": {LocalHash: "h-photos"},
	}

	plans := expandArchiveDelete(archive, dir, "docs/")
	if len(plans) != 1 {
		t.Fatalf("got %d plans, want 1", len(plans))
	}
	if plans[0].Path != "docs/a.txt" || plans[0].Action != types.DeleteLocal {
		t.Errorf("plan = %+v", plans[0])
	}
}

func TestExpandArchiveDelete_LocalEditedBecomesConflict(t *testing.T) {
	dir := t.TempDir()
	writeLocalFile(t, dir, "docs/a.txt", "hello")

	// Archive records the ORIGINAL hash; local file no longer matches.
	archive := map[string]types.FileState{
		"docs/a.txt": {LocalHash: "h-stale"},
	}

	plans := expandArchiveDelete(archive, dir, "docs/")
	if len(plans) != 1 {
		t.Fatalf("got %d plans, want 1", len(plans))
	}
	if plans[0].Action != types.Conflict {
		t.Errorf("action = %v, want Conflict (local edited since last sync)", plans[0].Action)
	}
}

func TestExpandArchiveDelete_EmptyPrefixMatchesEverything(t *testing.T) {
	dir := t.TempDir()
	h1 := writeLocalFileExpectHash(t, dir, "docs/a.txt", "one")
	h2 := writeLocalFileExpectHash(t, dir, "photos/b.jpg", "two")
	archive := map[string]types.FileState{
		"docs/a.txt":   {LocalHash: h1},
		"photos/b.jpg": {LocalHash: h2},
	}

	plans := expandArchiveDelete(archive, dir, "")
	if len(plans) != 2 {
		t.Fatalf("got %d plans, want 2 (empty prefix = scope wipe)", len(plans))
	}
}

func TestExpandArchiveDelete_SimilarPrefixNotConfused(t *testing.T) {
	dir := t.TempDir()
	h1 := writeLocalFileExpectHash(t, dir, "photos/a.jpg", "a")
	h2 := writeLocalFileExpectHash(t, dir, "photosExtra/b.jpg", "b")
	archive := map[string]types.FileState{
		"photos/a.jpg":      {LocalHash: h1},
		"photosExtra/b.jpg": {LocalHash: h2},
	}
	plans := expandArchiveDelete(archive, dir, "photos/")
	if len(plans) != 1 {
		t.Fatalf("got %d plans, want 1 (photosExtra must not match)", len(plans))
	}
	if plans[0].Path != "photos/a.jpg" {
		t.Errorf("plan path = %q", plans[0].Path)
	}
}

// --- expandArchiveMove ---

func TestExpandArchiveMove_RenamesUnchangedFiles(t *testing.T) {
	dir := t.TempDir()
	h := writeLocalFileExpectHash(t, dir, "old/a.txt", "hello")

	archive := map[string]types.FileState{
		"old/a.txt": {LocalHash: h, ContentVersion: 1, Size: 5},
	}

	plans, mutated, err := expandArchiveMove(archive, dir, "old/", "new/")
	if err != nil {
		t.Fatal(err)
	}
	if !mutated {
		t.Error("mutated = false, want true")
	}
	if len(plans) != 0 {
		t.Fatalf("got %d plans, want 0 (pure rename emits no plans)", len(plans))
	}
	if _, ok := archive["old/a.txt"]; ok {
		t.Error("old/a.txt should have been rekeyed out of archive")
	}
	if got, ok := archive["new/a.txt"]; !ok || got.LocalHash != h {
		t.Errorf("archive[new/a.txt] = %+v (ok=%v)", got, ok)
	}
	if !exists(t, dir, "new/a.txt") {
		t.Error("new/a.txt should exist after rename")
	}
	if exists(t, dir, "old/a.txt") {
		t.Error("old/a.txt should be gone after rename")
	}
}

func TestExpandArchiveMove_LocalEditedBecomesConflict(t *testing.T) {
	dir := t.TempDir()
	writeLocalFile(t, dir, "old/a.txt", "local-edit")

	archive := map[string]types.FileState{
		"old/a.txt": {LocalHash: "h-stale"},
	}

	plans, mutated, err := expandArchiveMove(archive, dir, "old/", "new/")
	if err != nil {
		t.Fatal(err)
	}
	if mutated {
		t.Error("mutated = true, want false (conflict should leave tree alone)")
	}
	if len(plans) != 1 || plans[0].Action != types.Conflict {
		t.Errorf("plans = %+v", plans)
	}
	if !exists(t, dir, "old/a.txt") {
		t.Error("old/a.txt should still exist on conflict")
	}
	if _, ok := archive["old/a.txt"]; !ok {
		t.Error("archive entry should remain on conflict")
	}
}

// --- MergePlansByPath ---

func TestMergePlansByPath_DirEventsOverrideIncremental(t *testing.T) {
	incremental := []types.SyncPlan{
		{Path: "a.txt", Action: types.Push},
		{Path: "b.txt", Action: types.DeleteLocal},
	}
	dirEvents := []types.SyncPlan{
		{Path: "a.txt", Action: types.Pull, RevisionID: "rev_a"},
		{Path: "c.txt", Action: types.Pull, RevisionID: "rev_c"},
	}
	merged := MergePlansByPath(incremental, dirEvents)
	if len(merged) != 3 {
		t.Fatalf("got %d plans, want 3", len(merged))
	}
	byPath := make(map[string]types.SyncPlan)
	for _, p := range merged {
		byPath[p.Path] = p
	}
	if byPath["a.txt"].Action != types.Pull || byPath["a.txt"].RevisionID != "rev_a" {
		t.Errorf("a.txt = %+v, want dir-event override", byPath["a.txt"])
	}
	if byPath["b.txt"].Action != types.DeleteLocal {
		t.Errorf("b.txt = %+v, want DeleteLocal (incremental only)", byPath["b.txt"])
	}
}
