package sync

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/selfbase-dev/s2-cli/internal/client"
	"github.com/selfbase-dev/s2-cli/internal/types"
)

// fakeSnapshotServer builds an httptest.Server whose /api/snapshot
// endpoint serves predetermined SnapshotResponse payloads keyed by the
// `?path=` query (empty string = scope root).
func fakeSnapshotServer(t *testing.T, responses map[string]types.SnapshotResponse) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/snapshot" {
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		key := r.URL.Query().Get("path")
		resp, ok := responses[key]
		if !ok {
			http.Error(w, "no response for "+key, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

// TestHandleIncrementalDirEvents_Mkdir exercises ADR 0040 case #13:
// mkdir → os.MkdirAll, no plans, no network traffic.
func TestHandleIncrementalDirEvents_Mkdir(t *testing.T) {
	dir := t.TempDir()
	c := client.New("http://invalid", "s2_test") // should never be called
	archive := map[string]types.FileState{}

	changes := []types.ChangeEntry{
		{Action: "mkdir", IsDir: true, PathAfter: "/new/nested"},
	}
	outcome, err := HandleIncrementalDirEvents(c, dir, archive, changes)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.LocalChanged {
		t.Error("LocalChanged = false, want true")
	}
	if len(outcome.ArchiveWalkPlans) != 0 {
		t.Errorf("plans = %+v, want none", outcome.ArchiveWalkPlans)
	}
	if !exists(t, dir, "new/nested") {
		t.Error("new/nested directory should exist")
	}
}

// TestHandleIncrementalDirEvents_DeleteInScope exercises ADR 0040 case
// #1: a scope-internal dir delete walks the archive by prefix and
// produces DeleteLocal plans for untouched files.
func TestHandleIncrementalDirEvents_DeleteInScope(t *testing.T) {
	dir := t.TempDir()
	c := client.New("http://invalid", "s2_test")
	h1 := writeLocalFileExpectHash(t, dir, "vacation/a.jpg", "a-bytes")
	h2 := writeLocalFileExpectHash(t, dir, "vacation/sub/b.jpg", "b-bytes")
	h3 := writeLocalFileExpectHash(t, dir, "photos/keep.jpg", "k-bytes")

	archive := map[string]types.FileState{
		"vacation/a.jpg":     {LocalHash: h1, ContentVersion: 1},
		"vacation/sub/b.jpg": {LocalHash: h2, ContentVersion: 1},
		"photos/keep.jpg":    {LocalHash: h3, ContentVersion: 1},
	}

	changes := []types.ChangeEntry{
		{Action: "delete", IsDir: true, PathBefore: "/vacation"},
	}
	outcome, err := HandleIncrementalDirEvents(c, dir, archive, changes)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.ArchiveWalkPlans) != 2 {
		t.Fatalf("plans = %d, want 2", len(outcome.ArchiveWalkPlans))
	}
	for _, p := range outcome.ArchiveWalkPlans {
		if p.Action != types.DeleteLocal {
			t.Errorf("plan %s: action = %v, want DeleteLocal", p.Path, p.Action)
		}
		if p.Path == "photos/keep.jpg" {
			t.Errorf("photos/keep.jpg should not be in plans")
		}
	}
}

// TestHandleIncrementalDirEvents_DeleteScopeWide exercises ADR 0040
// cases #2/#3/#8/#9: `delete /` sweeps every archive entry.
func TestHandleIncrementalDirEvents_DeleteScopeWide(t *testing.T) {
	dir := t.TempDir()
	c := client.New("http://invalid", "s2_test")
	h1 := writeLocalFileExpectHash(t, dir, "a.txt", "a")
	h2 := writeLocalFileExpectHash(t, dir, "b/c.txt", "c")

	archive := map[string]types.FileState{
		"a.txt":   {LocalHash: h1},
		"b/c.txt": {LocalHash: h2},
	}

	changes := []types.ChangeEntry{
		{Action: "delete", IsDir: true, PathBefore: "/"},
	}
	outcome, err := HandleIncrementalDirEvents(c, dir, archive, changes)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.ArchiveWalkPlans) != 2 {
		t.Fatalf("plans = %d, want 2 (both archive entries)", len(outcome.ArchiveWalkPlans))
	}
}

// TestHandleIncrementalDirEvents_MoveInScope exercises ADR 0040 case #5:
// scope-internal move renames archive entries + local files.
func TestHandleIncrementalDirEvents_MoveInScope(t *testing.T) {
	dir := t.TempDir()
	c := client.New("http://invalid", "s2_test")
	h := writeLocalFileExpectHash(t, dir, "old/a.txt", "content")

	archive := map[string]types.FileState{
		"old/a.txt": {LocalHash: h, ContentVersion: 1},
	}

	changes := []types.ChangeEntry{
		{Action: "move", IsDir: true, PathBefore: "/old", PathAfter: "/new"},
	}
	outcome, err := HandleIncrementalDirEvents(c, dir, archive, changes)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.LocalChanged {
		t.Error("LocalChanged = false, want true (rename happened)")
	}
	if len(outcome.ArchiveWalkPlans) != 0 {
		t.Errorf("plans = %+v, want none (clean rename emits no plan)", outcome.ArchiveWalkPlans)
	}
	if _, ok := archive["old/a.txt"]; ok {
		t.Error("old/a.txt should have been rekeyed")
	}
	if _, ok := archive["new/a.txt"]; !ok {
		t.Error("new/a.txt should be in archive")
	}
	if !exists(t, dir, "new/a.txt") {
		t.Error("new/a.txt should exist on disk")
	}
}

// TestHandleIncrementalDirEvents_PutSubtreeSnapshot exercises ADR 0040
// cases #7/#11: scope-external → internal move / restore_trash of a
// subtree triggers /api/snapshot?path=X. The outcome carries the
// snapshot response and SubtreeComparePlans (called AFTER the caller's
// re-walk) produces the file-level plans.
func TestHandleIncrementalDirEvents_PutSubtreeSnapshot(t *testing.T) {
	dir := t.TempDir()

	srv := fakeSnapshotServer(t, map[string]types.SnapshotResponse{
		"/vacation": {
			Items: []types.SnapshotItem{
				{Path: "/vacation/a.jpg", Type: "file", RevisionID: "rev_a", Hash: "h-a", ContentVersion: 1, Size: int64Ptr(7)},
				{Path: "/vacation/b.jpg", Type: "file", RevisionID: "rev_b", Hash: "h-b", ContentVersion: 1, Size: int64Ptr(9)},
			},
			Cursor: "cursor_sub",
		},
	})
	defer srv.Close()
	c := client.New(srv.URL, "s2_test")

	archive := map[string]types.FileState{}

	changes := []types.ChangeEntry{
		{Action: "put", IsDir: true, PathAfter: "/vacation"},
	}
	outcome, err := HandleIncrementalDirEvents(c, dir, archive, changes)
	if err != nil {
		t.Fatal(err)
	}
	// Subtree snapshot must NOT replace the primary cursor (ADR 0040
	// §cursor semantics).
	if outcome.NewPrimaryCursor != "" {
		t.Errorf("NewPrimaryCursor = %q, want empty for subtree snapshot", outcome.NewPrimaryCursor)
	}
	if len(outcome.SubtreeSnapshots) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(outcome.SubtreeSnapshots))
	}
	// Running the (empty) local compare should produce Pull plans for
	// both snapshot items.
	plans := outcome.SubtreeComparePlans(map[string]types.LocalFile{}, archive)
	if len(plans) != 2 {
		t.Fatalf("compare plans = %d, want 2", len(plans))
	}
	for _, p := range plans {
		if p.Action != types.Pull {
			t.Errorf("plan %s: action = %v, want Pull", p.Path, p.Action)
		}
		if p.RevisionID == "" {
			t.Errorf("plan %s: empty revision id", p.Path)
		}
	}
}

// TestHandleIncrementalDirEvents_PutScopeRootReplacesCursor exercises
// ADR 0040 cases #10/#12: scope-root put (ancestor enter / restore)
// triggers /api/snapshot and REPLACES the primary cursor wholesale.
func TestHandleIncrementalDirEvents_PutScopeRootReplacesCursor(t *testing.T) {
	dir := t.TempDir()

	srv := fakeSnapshotServer(t, map[string]types.SnapshotResponse{
		"": {
			Items: []types.SnapshotItem{
				{Path: "/photos/a.jpg", Type: "file", RevisionID: "rev_a", Hash: "h-a", ContentVersion: 1, Size: int64Ptr(4)},
			},
			Cursor: "cursor_root",
		},
	})
	defer srv.Close()
	c := client.New(srv.URL, "s2_test")

	archive := map[string]types.FileState{}

	changes := []types.ChangeEntry{
		{Action: "put", IsDir: true, PathAfter: "/"},
	}
	outcome, err := HandleIncrementalDirEvents(c, dir, archive, changes)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NewPrimaryCursor != "cursor_root" {
		t.Errorf("NewPrimaryCursor = %q, want %q", outcome.NewPrimaryCursor, "cursor_root")
	}
	plans := outcome.SubtreeComparePlans(map[string]types.LocalFile{}, archive)
	if len(plans) != 1 || plans[0].Action != types.Pull {
		t.Errorf("plans = %+v", plans)
	}
}

// TestHandleIncrementalDirEvents_PutSubtreeDeletedRace simulates a race
// where the subtree we were told to fetch has already been deleted by
// the time we ask (404). The event is silently dropped — the next poll
// will carry the delete and clean up the archive.
func TestHandleIncrementalDirEvents_PutSubtreeDeletedRace(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()
	c := client.New(srv.URL, "s2_test")

	changes := []types.ChangeEntry{
		{Action: "put", IsDir: true, PathAfter: "/gone"},
	}
	outcome, err := HandleIncrementalDirEvents(c, dir, map[string]types.FileState{}, changes)
	if err != nil {
		t.Fatalf("unexpected error on 404 race: %v", err)
	}
	if len(outcome.SubtreeSnapshots) != 0 {
		t.Errorf("snapshots = %+v, want none", outcome.SubtreeSnapshots)
	}
	if len(outcome.ArchiveWalkPlans) != 0 {
		t.Errorf("plans = %+v, want none", outcome.ArchiveWalkPlans)
	}
}

// TestHandleIncrementalDirEvents_PathTraversalRejected covers codex
// blocker #1: server-supplied paths must be validated before they
// reach filepath.Join. A malicious "/.." payload must not escape the
// sync root.
func TestHandleIncrementalDirEvents_PathTraversalRejected(t *testing.T) {
	dir := t.TempDir()
	c := client.New("http://invalid", "s2_test")
	archive := map[string]types.FileState{}

	changes := []types.ChangeEntry{
		{Action: "mkdir", IsDir: true, PathAfter: "/../escape"},
	}
	_, err := HandleIncrementalDirEvents(c, dir, archive, changes)
	if err == nil {
		t.Fatal("expected error for traversal path, got nil")
	}
}

// TestHandleIncrementalDirEvents_DeleteWithUntrackedDescendant covers
// codex blocker #3 for delete: a local-only file under the deleted
// prefix must be preserved as a conflict copy, not resurrected.
func TestHandleIncrementalDirEvents_DeleteWithUntrackedDescendant(t *testing.T) {
	dir := t.TempDir()
	c := client.New("http://invalid", "s2_test")
	h := writeLocalFileExpectHash(t, dir, "docs/tracked.txt", "t")
	writeLocalFile(t, dir, "docs/untracked.txt", "u")

	archive := map[string]types.FileState{
		"docs/tracked.txt": {LocalHash: h},
	}

	changes := []types.ChangeEntry{
		{Action: "delete", IsDir: true, PathBefore: "/docs"},
	}
	outcome, err := HandleIncrementalDirEvents(c, dir, archive, changes)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.ArchiveWalkPlans) != 2 {
		t.Fatalf("plans = %d, want 2 (tracked + untracked)", len(outcome.ArchiveWalkPlans))
	}
	var actions []types.SyncAction
	for _, p := range outcome.ArchiveWalkPlans {
		actions = append(actions, p.Action)
	}
	hasDeleteLocal := false
	hasPreserve := false
	for _, a := range actions {
		if a == types.DeleteLocal {
			hasDeleteLocal = true
		}
		if a == types.PreserveLocalRename {
			hasPreserve = true
		}
	}
	if !hasDeleteLocal || !hasPreserve {
		t.Errorf("actions = %+v, want one DeleteLocal + one PreserveLocalRename", actions)
	}
}
