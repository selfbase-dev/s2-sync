package sync

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

// fakeBootstrapServer builds a mock that supports the full Bootstrap
// protocol: /api/changes/latest (pin S0), /api/snapshot (fetch), and
// /api/changes?after= (converge with empty changes).
func fakeBootstrapServer(t *testing.T, snapshot types.SnapshotResponse, latestCursor string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/changes/latest":
			_ = json.NewEncoder(w).Encode(types.LatestCursorResponse{Cursor: latestCursor})
		case "/api/snapshot":
			_ = json.NewEncoder(w).Encode(snapshot)
		case "/api/changes":
			_ = json.NewEncoder(w).Encode(types.ChangesResponse{})
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
		}
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
	outcome, err := HandleIncrementalDirEvents(c, dir, testStateFromArchive(archive), changes)
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
	outcome, err := HandleIncrementalDirEvents(c, dir, testStateFromArchive(archive), changes)
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
	outcome, err := HandleIncrementalDirEvents(c, dir, testStateFromArchive(archive), changes)
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
	outcome, err := HandleIncrementalDirEvents(c, dir, testStateFromArchive(archive), changes)
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
	outcome, err := HandleIncrementalDirEvents(c, dir, testStateFromArchive(archive), changes)
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
// runs the full Bootstrap protocol (ADR 0046) and REPLACES the primary
// cursor with the converged cursor.
func TestHandleIncrementalDirEvents_PutScopeRootReplacesCursor(t *testing.T) {
	dir := t.TempDir()

	snapshot := types.SnapshotResponse{
		Items: []types.SnapshotItem{
			{Path: "/photos/a.jpg", Type: "file", RevisionID: "rev_a", Hash: "h-a", ContentVersion: 1, Size: int64Ptr(4)},
		},
		Cursor: "cursor_snapshot",
	}
	srv := fakeBootstrapServer(t, snapshot, "cursor_s0")
	defer srv.Close()
	c := client.New(srv.URL, "s2_test")

	archive := map[string]types.FileState{}

	changes := []types.ChangeEntry{
		{Action: "put", IsDir: true, PathAfter: "/"},
	}
	outcome, err := HandleIncrementalDirEvents(c, dir, testStateFromArchive(archive), changes)
	if err != nil {
		t.Fatal(err)
	}
	// Bootstrap converges: pinned cursor_s0, snapshot succeeded, poll
	// returns empty → converged cursor is cursor_s0 (not snapshot cursor).
	if outcome.NewPrimaryCursor != "cursor_s0" {
		t.Errorf("NewPrimaryCursor = %q, want %q (converged cursor from Bootstrap)", outcome.NewPrimaryCursor, "cursor_s0")
	}
	plans := outcome.SubtreeComparePlans(map[string]types.LocalFile{}, archive)
	if len(plans) != 1 || plans[0].Action != types.Pull {
		t.Errorf("plans = %+v", plans)
	}
}

// TestHandleIncrementalDirEvents_PutScopeRootClearsStalePlans is a
// regression test: when a batch has `delete /foo` followed by `put /`,
// the scope-root bootstrap must clear the stale DeleteLocal plans from
// the earlier delete. Otherwise MergePlansByPath (archiveWalk wins)
// would erroneously delete files that the bootstrap shows still exist.
func TestHandleIncrementalDirEvents_PutScopeRootClearsStalePlans(t *testing.T) {
	dir := t.TempDir()
	h := writeLocalFileExpectHash(t, dir, "foo/a.txt", "content")

	archive := map[string]types.FileState{
		"foo/a.txt": {LocalHash: h, ContentVersion: 1},
	}

	snapshot := types.SnapshotResponse{
		Items: []types.SnapshotItem{
			{Path: "/foo/a.txt", Type: "file", RevisionID: "rev_a", Hash: "h-a", ContentVersion: 2, Size: int64Ptr(7)},
		},
		Cursor: "cursor_snap",
	}
	srv := fakeBootstrapServer(t, snapshot, "cursor_s0")
	defer srv.Close()
	c := client.New(srv.URL, "s2_test")

	changes := []types.ChangeEntry{
		{Action: "delete", IsDir: true, PathBefore: "/foo"},
		{Action: "put", IsDir: true, PathAfter: "/"},
	}
	outcome, err := HandleIncrementalDirEvents(c, dir, testStateFromArchive(archive), changes)
	if err != nil {
		t.Fatal(err)
	}
	// The delete produced ArchiveWalkPlans, but the subsequent put /
	// must have cleared them.
	if len(outcome.ArchiveWalkPlans) != 0 {
		t.Errorf("ArchiveWalkPlans = %d, want 0 (scope-root put must clear stale plans)", len(outcome.ArchiveWalkPlans))
	}
	if outcome.NewPrimaryCursor == "" {
		t.Error("NewPrimaryCursor should be set by scope-root bootstrap")
	}
	// The bootstrap snapshot contains foo/a.txt, so Compare should
	// produce a plan for it (Pull since archive was mutated by delete).
	plans := outcome.SubtreeComparePlans(
		map[string]types.LocalFile{"foo/a.txt": {Hash: h}},
		archive,
	)
	for _, p := range plans {
		if p.Path == "foo/a.txt" && p.Action == types.DeleteLocal {
			t.Errorf("foo/a.txt should NOT be DeleteLocal — scope-root bootstrap is authoritative")
		}
	}
}

// TestHandleIncrementalDirEvents_PutSubtree413Fallback exercises the
// 413 fallback path: /api/snapshot returns 413, FetchRemoteMap falls
// back to ListDir recursive descent. No cursor update for subtree.
func TestHandleIncrementalDirEvents_PutSubtree413Fallback(t *testing.T) {
	dir := t.TempDir()
	hash := "abc123"
	cv := int64(2)
	revID := "rev_x"
	size := int64(11)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/snapshot":
			w.WriteHeader(413)
		case strings.HasSuffix(r.URL.Path, "/vacation/"):
			_ = json.NewEncoder(w).Encode(types.ListResponse{
				Items: []types.FileItem{
					{Name: "photo.jpg", Type: "file", Hash: &hash, ContentVersion: &cv, RevisionID: &revID, Size: &size},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := client.New(srv.URL, "s2_test")

	archive := map[string]types.FileState{}
	changes := []types.ChangeEntry{
		{Action: "put", IsDir: true, PathAfter: "/vacation"},
	}
	outcome, err := HandleIncrementalDirEvents(c, dir, testStateFromArchive(archive), changes)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NewPrimaryCursor != "" {
		t.Errorf("NewPrimaryCursor = %q, want empty for subtree 413 fallback", outcome.NewPrimaryCursor)
	}
	if len(outcome.SubtreeSnapshots) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(outcome.SubtreeSnapshots))
	}
	snap := outcome.SubtreeSnapshots[0]
	if snap.Prefix != "vacation/" {
		t.Errorf("prefix = %q, want %q", snap.Prefix, "vacation/")
	}
	if len(snap.Remote) != 1 {
		t.Fatalf("remote files = %d, want 1", len(snap.Remote))
	}
	if _, ok := snap.Remote["vacation/photo.jpg"]; !ok {
		t.Errorf("expected vacation/photo.jpg in remote map, got %v", snap.Remote)
	}
}

// TestHandleIncrementalDirEvents_PutSubtree413Then404 exercises the
// 413→ListDir→404 path: snapshot returns 413, then ListDir returns
// 404 (subtree deleted during the fallback). The wrapped error must
// still be recognized via errors.Is and silently dropped.
func TestHandleIncrementalDirEvents_PutSubtree413Then404(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/snapshot" {
			w.WriteHeader(413)
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	c := client.New(srv.URL, "s2_test")

	changes := []types.ChangeEntry{
		{Action: "put", IsDir: true, PathAfter: "/vanished"},
	}
	outcome, err := HandleIncrementalDirEvents(c, dir, testStateFromArchive(nil), changes)
	if err != nil {
		t.Fatalf("expected silent drop on 413→ListDir→404, got: %v", err)
	}
	if len(outcome.SubtreeSnapshots) != 0 {
		t.Errorf("snapshots = %+v, want none", outcome.SubtreeSnapshots)
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
	outcome, err := HandleIncrementalDirEvents(c, dir, testStateFromArchive(nil), changes)
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
	_, err := HandleIncrementalDirEvents(c, dir, testStateFromArchive(archive), changes)
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
	outcome, err := HandleIncrementalDirEvents(c, dir, testStateFromArchive(archive), changes)
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
