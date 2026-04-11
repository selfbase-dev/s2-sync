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

// --- expandArchiveDelete ---

func TestExpandArchiveDelete_UnchangedLocal(t *testing.T) {
	dir := t.TempDir()
	h := writeLocalFileExpectHash(t, dir, "docs/a.txt", "hello")
	writeLocalFileExpectHash(t, dir, "photos/b.jpg", "other")

	archive := map[string]types.FileState{
		"docs/a.txt":   {LocalHash: h},
		"photos/b.jpg": {LocalHash: "h-photos"},
	}

	plans, err := expandArchiveDelete(archive, dir, "docs/")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 {
		t.Fatalf("got %d plans, want 1", len(plans))
	}
	if plans[0].Path != "docs/a.txt" || plans[0].Action != types.DeleteLocal {
		t.Errorf("plan = %+v", plans[0])
	}
}

func TestExpandArchiveDelete_LocalEditedBecomesPreserveRename(t *testing.T) {
	dir := t.TempDir()
	writeLocalFile(t, dir, "docs/a.txt", "hello")

	// Archive records the ORIGINAL hash; local file no longer matches.
	archive := map[string]types.FileState{
		"docs/a.txt": {LocalHash: "h-stale"},
	}

	plans, err := expandArchiveDelete(archive, dir, "docs/")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 {
		t.Fatalf("got %d plans, want 1", len(plans))
	}
	if plans[0].Action != types.PreserveLocalRename {
		t.Errorf("action = %v, want PreserveLocalRename (locally edited)", plans[0].Action)
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

	plans, err := expandArchiveDelete(archive, dir, "")
	if err != nil {
		t.Fatal(err)
	}
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
	plans, err := expandArchiveDelete(archive, dir, "photos/")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 {
		t.Fatalf("got %d plans, want 1 (photosExtra must not match)", len(plans))
	}
	if plans[0].Path != "photos/a.jpg" {
		t.Errorf("plan path = %q", plans[0].Path)
	}
}

// Codex blocker #3: a local-only file under the deleted prefix (not in
// archive) must be preserved as a conflict copy. Without this fix it
// would be picked up by CompareIncremental as "local new" and pushed
// back, resurrecting the subtree.
func TestExpandArchiveDelete_LocalOnlyDescendantPreserved(t *testing.T) {
	dir := t.TempDir()
	h := writeLocalFileExpectHash(t, dir, "docs/tracked.txt", "t")
	writeLocalFile(t, dir, "docs/untracked.txt", "u")

	archive := map[string]types.FileState{
		"docs/tracked.txt": {LocalHash: h},
	}

	plans, err := expandArchiveDelete(archive, dir, "docs/")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Fatalf("got %d plans, want 2 (one tracked + one untracked)", len(plans))
	}
	var tracked, untracked types.SyncPlan
	for _, p := range plans {
		switch p.Path {
		case "docs/tracked.txt":
			tracked = p
		case "docs/untracked.txt":
			untracked = p
		}
	}
	if tracked.Action != types.DeleteLocal {
		t.Errorf("tracked action = %v, want DeleteLocal", tracked.Action)
	}
	if untracked.Action != types.PreserveLocalRename {
		t.Errorf("untracked action = %v, want PreserveLocalRename", untracked.Action)
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

func TestExpandArchiveMove_LocalEditedBecomesPreserveRename(t *testing.T) {
	dir := t.TempDir()
	writeLocalFile(t, dir, "old/a.txt", "local-edit")

	archive := map[string]types.FileState{
		"old/a.txt": {LocalHash: "h-stale"},
	}

	plans, _, err := expandArchiveMove(archive, dir, "old/", "new/")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Action != types.PreserveLocalRename {
		t.Errorf("plans = %+v, want one PreserveLocalRename", plans)
	}
	if !exists(t, dir, "old/a.txt") {
		t.Error("old/a.txt should still exist on conflict preserve")
	}
	if _, ok := archive["old/a.txt"]; ok {
		t.Error("archive entry should be removed on conflict preserve")
	}
}

// Codex blocker #3 variant for move: a local-only descendant under the
// old prefix should be renamed to the new prefix alongside the tracked
// files, so it doesn't get pushed back under the old location.
func TestExpandArchiveMove_LocalOnlyDescendantIsRenamed(t *testing.T) {
	dir := t.TempDir()
	h := writeLocalFileExpectHash(t, dir, "old/tracked.txt", "t")
	writeLocalFile(t, dir, "old/untracked.txt", "u")

	archive := map[string]types.FileState{
		"old/tracked.txt": {LocalHash: h},
	}

	_, mutated, err := expandArchiveMove(archive, dir, "old/", "new/")
	if err != nil {
		t.Fatal(err)
	}
	if !mutated {
		t.Error("mutated = false, want true")
	}
	if !exists(t, dir, "new/tracked.txt") {
		t.Error("new/tracked.txt should exist")
	}
	if !exists(t, dir, "new/untracked.txt") {
		t.Error("new/untracked.txt should exist (local-only descendant must be renamed)")
	}
	if exists(t, dir, "old/tracked.txt") || exists(t, dir, "old/untracked.txt") {
		t.Error("old/ should be empty after rename")
	}
}

// --- MergePlansByPath ---

func TestMergePlansByPath_LastWriterWins(t *testing.T) {
	// First list has one entry; second list overrides it; third list
	// overrides the second. This matches the incremental sync merge
	// order (file events → subtree compare → archive walk).
	first := []types.SyncPlan{{Path: "a.txt", Action: types.Push}}
	second := []types.SyncPlan{{Path: "a.txt", Action: types.Pull, RevisionID: "rev_a"}}
	third := []types.SyncPlan{{Path: "a.txt", Action: types.DeleteLocal}}

	merged := MergePlansByPath(first, second, third)
	if len(merged) != 1 {
		t.Fatalf("got %d plans, want 1", len(merged))
	}
	if merged[0].Action != types.DeleteLocal {
		t.Errorf("action = %v, want DeleteLocal (last list wins)", merged[0].Action)
	}
}

func TestMergePlansByPath_UnionKeepsDisjointPaths(t *testing.T) {
	a := []types.SyncPlan{{Path: "a.txt", Action: types.Push}}
	b := []types.SyncPlan{{Path: "b.txt", Action: types.Pull}}
	c := []types.SyncPlan{{Path: "c.txt", Action: types.DeleteLocal}}

	merged := MergePlansByPath(a, b, c)
	if len(merged) != 3 {
		t.Fatalf("got %d plans, want 3", len(merged))
	}
}
