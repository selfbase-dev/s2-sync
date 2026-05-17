package sync

// dir_lifecycle_test.go pins the server-driven directory lifecycle
// behavior on the local FS: materialize empty dirs into existence
// from a snapshot, and collapse the local shell after a remote
// folder delete (subject to fail-safe conditions). Each test names
// the rule it covers so failures stay legible when the design is
// revisited.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/selfbase-dev/s2-sync/internal/auth"
	"github.com/selfbase-dev/s2-sync/internal/client"
	"github.com/selfbase-dev/s2-sync/internal/types"
)

// --- expandArchiveDelete RmdirLocal emission -------------------------

// Rule: a clean per-file delete batch (no PreserveLocalRename) appends
// a RmdirLocal post-action so the local shell collapses with the data.
func TestExpandArchiveDelete_EmitsRmdirOnCleanDelete(t *testing.T) {
	dir := t.TempDir()
	h := writeLocalFileExpectHash(t, dir, "music/album/track.mp3", "bytes")
	archive := map[string]types.FileState{
		"music/album/track.mp3": {LocalHash: h},
	}
	plans, err := expandArchiveDelete(archive, dir, "music/")
	if err != nil {
		t.Fatal(err)
	}
	sawRmdir := false
	for _, p := range plans {
		if p.Action == types.RmdirLocal && p.Path == "music" {
			sawRmdir = true
		}
	}
	if !sawRmdir {
		t.Fatalf("plans = %+v; want one with Action=RmdirLocal Path=music", plans)
	}
}

// Rule: a drifted local file (PreserveLocalRename surfacing) blocks
// the rmdir so the user keeps a place where the conflict copy lands.
func TestExpandArchiveDelete_SuppressesRmdirOnPreserve(t *testing.T) {
	dir := t.TempDir()
	writeLocalFile(t, dir, "music/track.mp3", "drifted")
	archive := map[string]types.FileState{
		"music/track.mp3": {LocalHash: "stale"},
	}
	plans, err := expandArchiveDelete(archive, dir, "music/")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plans {
		if p.Action == types.RmdirLocal {
			t.Errorf("rmdir must not be emitted when PreserveLocalRename plans exist; got %+v", plans)
		}
	}
}

// Rule: an untracked descendant (.DS_Store etc.) blocks the rmdir for
// the same reason — untracked content is conflict-class.
func TestExpandArchiveDelete_SuppressesRmdirOnUntrackedDescendant(t *testing.T) {
	dir := t.TempDir()
	h := writeLocalFileExpectHash(t, dir, "music/tracked.mp3", "t")
	writeLocalFile(t, dir, "music/.DS_Store", "ds")
	archive := map[string]types.FileState{
		"music/tracked.mp3": {LocalHash: h},
	}
	plans, err := expandArchiveDelete(archive, dir, "music/")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plans {
		if p.Action == types.RmdirLocal {
			t.Errorf("rmdir must not be emitted with untracked descendant; got %+v", plans)
		}
	}
}

// Rule: scope-wide delete (empty prefix) never rmdirs — the mount
// point must survive so the user keeps a place to sync next time.
func TestExpandArchiveDelete_SuppressesRmdirOnScopeRoot(t *testing.T) {
	dir := t.TempDir()
	h := writeLocalFileExpectHash(t, dir, "a.txt", "x")
	archive := map[string]types.FileState{
		"a.txt": {LocalHash: h},
	}
	plans, err := expandArchiveDelete(archive, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plans {
		if p.Action == types.RmdirLocal {
			t.Errorf("rmdir must not be emitted for scope-root delete; got %+v", plans)
		}
	}
}

// --- executor RmdirLocal --------------------------------------------

// Rule: the executor's RmdirLocal action removes the directory after
// the per-file deletes have drained, even though the plans arrive with
// the rmdir alphabetically sorted before its children.
func TestExecute_RmdirLocal_RemovesEmptyDir(t *testing.T) {
	dir := t.TempDir()
	h := writeLocalFileExpectHash(t, dir, "music/track.mp3", "bytes")
	archive := map[string]types.FileState{
		"music/track.mp3": {LocalHash: h},
	}
	st := testStateFromArchive(archive)

	plans := []types.SyncPlan{
		{Path: "music", Action: types.RmdirLocal},
		{Path: "music/track.mp3", Action: types.DeleteLocal},
	}
	res, err := execute(plans, dir, nil, st, false, executeDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) != 0 {
		t.Errorf("errors = %+v", res.Errors)
	}
	if exists(t, dir, "music") {
		t.Errorf("music dir shell should be gone")
	}
}

// Rule: rmdir post-action silently no-ops when an untracked file kept
// the dir non-empty between expansion and execution. The neighbouring
// file delete still succeeds; nothing escalates to an error.
func TestExecute_RmdirLocal_SkipsWhenNonEmpty(t *testing.T) {
	dir := t.TempDir()
	h := writeLocalFileExpectHash(t, dir, "music/track.mp3", "bytes")
	writeLocalFile(t, dir, "music/.DS_Store", "ds")
	archive := map[string]types.FileState{
		"music/track.mp3": {LocalHash: h},
	}
	st := testStateFromArchive(archive)

	// Simulate the executor receiving the rmdir alone — the plan
	// generator would have suppressed it given the untracked file, but
	// we want a unit test for the fail-safe regardless.
	plans := []types.SyncPlan{
		{Path: "music/track.mp3", Action: types.DeleteLocal},
		{Path: "music", Action: types.RmdirLocal},
	}
	res, err := execute(plans, dir, nil, st, false, executeDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) != 0 {
		t.Errorf("errors = %+v", res.Errors)
	}
	if !exists(t, dir, "music") {
		t.Errorf("music dir must survive when an untracked descendant remained")
	}
	if !exists(t, dir, "music/.DS_Store") {
		t.Errorf("untracked .DS_Store must not be touched")
	}
}

// Rule: dry-run never mutates the local FS even for rmdir post-actions.
func TestExecute_RmdirLocal_DryRunSkips(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "music"), 0755); err != nil {
		t.Fatal(err)
	}
	st := testStateFromArchive(nil)
	plans := []types.SyncPlan{{Path: "music", Action: types.RmdirLocal}}
	if _, err := execute(plans, dir, nil, st, true, executeDeps{}); err != nil {
		t.Fatal(err)
	}
	if !exists(t, dir, "music") {
		t.Errorf("dry-run rmdir must not delete the directory")
	}
}

// Rule: on a case-insensitive FS, a fold-equivalent live archive entry
// (different exact case) protects the dir shell — otherwise rmdir would
// hit the same inode and silently delete the live sibling's home.
func TestExecute_RmdirLocal_SkipsCaseFoldCollision(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("requires case-insensitive default FS")
	}
	dir := t.TempDir()
	// "Music" deleted; "music/live.mp3" still tracked → fold collision.
	if err := os.MkdirAll(filepath.Join(dir, "Music"), 0755); err != nil {
		t.Fatal(err)
	}
	archive := map[string]types.FileState{
		"music/live.mp3": {LocalHash: "h"},
	}
	st := testStateFromArchive(archive)
	plans := []types.SyncPlan{{Path: "Music", Action: types.RmdirLocal}}
	if _, err := execute(plans, dir, nil, st, false, executeDeps{}); err != nil {
		t.Fatal(err)
	}
	if !exists(t, dir, "Music") {
		t.Errorf("dir shell must survive when a fold-equivalent live archive entry exists")
	}
}

// Rule: when RmdirLocal sorts alphabetically before its DeleteLocal
// children, the executor must still run the deletes first. Tests the
// reorderRmdirsLast helper end-to-end.
func TestExecute_RmdirLocal_RunsAfterChildDeletes(t *testing.T) {
	dir := t.TempDir()
	h1 := writeLocalFileExpectHash(t, dir, "a/b/c/leaf.txt", "x")
	archive := map[string]types.FileState{
		"a/b/c/leaf.txt": {LocalHash: h1},
	}
	st := testStateFromArchive(archive)

	// Three nested rmdirs interleaved with the leaf delete. Executor
	// must run them deepest-first AFTER the file delete so each rmdir
	// sees an empty directory.
	plans := []types.SyncPlan{
		{Path: "a", Action: types.RmdirLocal},
		{Path: "a/b", Action: types.RmdirLocal},
		{Path: "a/b/c", Action: types.RmdirLocal},
		{Path: "a/b/c/leaf.txt", Action: types.DeleteLocal},
	}
	res, err := execute(plans, dir, nil, st, false, executeDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) != 0 {
		t.Errorf("errors = %+v", res.Errors)
	}
	for _, p := range []string{"a", "a/b", "a/b/c", "a/b/c/leaf.txt"} {
		if exists(t, dir, p) {
			t.Errorf("%s should have been removed", p)
		}
	}
}

// --- MkdirLocal ------------------------------------------------------

// Rule: MkdirLocal materializes a directory with no file payload
// (snapshot dir item) so the user sees the empty folder they created
// on the web UI.
func TestExecute_MkdirLocal_CreatesDirChain(t *testing.T) {
	dir := t.TempDir()
	st := testStateFromArchive(nil)
	plans := []types.SyncPlan{{Path: "photos/2026/empty", Action: types.MkdirLocal}}
	res, err := execute(plans, dir, nil, st, false, executeDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) != 0 {
		t.Errorf("errors = %+v", res.Errors)
	}
	info, err := os.Stat(filepath.Join(dir, "photos/2026/empty"))
	if err != nil || !info.IsDir() {
		t.Errorf("photos/2026/empty should exist as a directory; err=%v", err)
	}
}

// Rule: MkdirLocal is idempotent — running it again on an existing dir
// must not error.
func TestExecute_MkdirLocal_Idempotent(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "preset"), 0755); err != nil {
		t.Fatal(err)
	}
	st := testStateFromArchive(nil)
	plans := []types.SyncPlan{{Path: "preset", Action: types.MkdirLocal}}
	res, err := execute(plans, dir, nil, st, false, executeDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) != 0 {
		t.Errorf("errors = %+v", res.Errors)
	}
}

// --- MaterializeDirPlans noise filtering ----------------------------

// Rule: dirs implicitly created by an in-flight file pull (parent dir
// of a remote file) are skipped — otherwise a healthy folder full of
// files would emit one redundant MkdirLocal per ancestor.
func TestMaterializeDirPlans_SkipsImplicitParents(t *testing.T) {
	remoteFiles := map[string]types.RemoteFile{
		"photos/2026/cat.jpg": {Hash: "h"},
	}
	dirs := []string{"photos", "photos/2026", "photos/2026/empty"}
	plans := MaterializeDirPlans(dirs, remoteFiles, "")
	if len(plans) != 1 || plans[0].Path != "photos/2026/empty" {
		t.Errorf("plans = %+v, want one MkdirLocal for the leaf-empty dir", plans)
	}
}

// --- Bootstrap dir set ----------------------------------------------

// Rule: BootstrapWithDirs surfaces dir-only nodes alongside files so
// initial sync can materialize empty folders.
func TestBootstrapWithDirs_ReturnsLiveDirs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/changes/latest":
			_ = json.NewEncoder(w).Encode(types.LatestCursorResponse{Cursor: "s0"})
		case "/api/v1/snapshot":
			_ = json.NewEncoder(w).Encode(types.SnapshotResponse{
				Items: []types.SnapshotItem{
					{Path: "/inbox/", Type: "dir"},
					{Path: "/inbox/today/", Type: "dir"},
					{Path: "/inbox/today/note.md", Type: "file", RevisionID: "r1", Hash: "h", ContentVersion: 1, Size: int64Ptr(2)},
					{Path: "/empty/", Type: "dir"},
				},
				Cursor: "snap",
			})
		case "/api/v1/changes":
			_ = json.NewEncoder(w).Encode(types.ChangesResponse{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := client.New(srv.URL, auth.NewStaticSource("s2_test"))
	files, dirs, cursor, err := BootstrapWithDirs(c)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != "s0" {
		t.Errorf("cursor = %q, want s0", cursor)
	}
	if _, ok := files["inbox/today/note.md"]; !ok {
		t.Errorf("expected note.md in files, got %v", files)
	}
	wantDirs := map[string]bool{"inbox": true, "inbox/today": true, "empty": true}
	for _, d := range dirs {
		if !wantDirs[d] {
			t.Errorf("unexpected dir %q in bootstrap output", d)
		}
		delete(wantDirs, d)
	}
	if len(wantDirs) != 0 {
		t.Errorf("missing dirs: %v", wantDirs)
	}
}

// --- HandleIncrementalDirEvents subtree put materializes dirs --------

// Rule: a remote "put" event that fetches a subtree snapshot must
// expose dir-only nodes to MaterializeDirPlans, so a remotely-created
// empty folder appears locally without ever owning a file.
func TestHandleIncrementalDirEvents_SubtreePutMaterializesEmptyDirs(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/snapshot" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(types.SnapshotResponse{
			Items: []types.SnapshotItem{
				{Path: "/vacation/", Type: "dir"},
				{Path: "/vacation/2026/", Type: "dir"},
				{Path: "/vacation/2026/empty/", Type: "dir"},
			},
		})
	}))
	defer srv.Close()
	c := client.New(srv.URL, auth.NewStaticSource("s2_test"))

	changes := []types.ChangeEntry{{Action: "put", IsDir: true, PathAfter: "/vacation"}}
	outcome, err := HandleIncrementalDirEvents(c, dir, testStateFromArchive(nil), changes)
	if err != nil {
		t.Fatal(err)
	}
	plans := outcome.SubtreeComparePlansForLocalRoot(
		map[string]types.LocalFile{},
		map[string]types.FileState{},
		dir,
	)
	wantDirs := map[string]bool{"vacation": true, "vacation/2026": true, "vacation/2026/empty": true}
	for _, p := range plans {
		if p.Action != types.MkdirLocal {
			continue
		}
		if !wantDirs[p.Path] {
			t.Errorf("unexpected MkdirLocal %q", p.Path)
		}
		delete(wantDirs, p.Path)
	}
	if len(wantDirs) != 0 {
		t.Errorf("missing MkdirLocal plans: %v", wantDirs)
	}
}

// --- end-to-end through execute() — full lifecycle scenarios --------

// Scenario: web UI deletes /Music; CLI runs incremental sync — local
// shell collapses with the file deletes. No extra error surface, no
// remnant ./Music/ folder.
func TestDirLifecycle_WebFolderDelete_CollapsesLocalShell(t *testing.T) {
	dir := t.TempDir()
	h := writeLocalFileExpectHash(t, dir, "Music/track.mp3", "bytes")
	st := testStateFromArchive(map[string]types.FileState{
		"Music/track.mp3": {LocalHash: h},
	})

	// Simulate the dir-delete event being processed by
	// HandleIncrementalDirEvents (no network: the per-file expansion
	// runs against the in-memory archive).
	c := client.New("http://invalid", auth.NewStaticSource("s2_test"))
	outcome, err := HandleIncrementalDirEvents(c, dir, st, []types.ChangeEntry{
		{Action: "delete", IsDir: true, PathBefore: "/Music"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, execErr := execute(outcome.ArchiveWalkPlans, dir, nil, st, false, executeDeps{}); execErr != nil {
		t.Fatal(execErr)
	}
	if exists(t, dir, "Music") {
		t.Errorf("Music shell should be gone after web folder delete")
	}
}

// Scenario: web UI creates an empty folder. CLI initial sync (via
// BootstrapWithDirs + MaterializeDirPlans) creates the local folder
// without any file ever owning it.
func TestDirLifecycle_WebEmptyFolder_AppearsLocally(t *testing.T) {
	dir := t.TempDir()
	plans := MaterializeDirPlans([]string{"inbox/empty"}, nil, dir)
	if len(plans) != 1 {
		t.Fatalf("plans = %+v, want one MkdirLocal", plans)
	}
	st := testStateFromArchive(nil)
	if _, err := execute(plans, dir, nil, st, false, executeDeps{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "inbox/empty"))
	if err != nil || !info.IsDir() {
		t.Errorf("inbox/empty should exist; err=%v", err)
	}
}

// Scenario: a file move on the server leaves the source dir empty.
// The dir row stays live (only the file moved), so RmdirLocal is NEVER
// emitted — we don't kill folders the user can still see on the web
// because of an incidental file shuffle.
func TestDirLifecycle_FileMove_DoesNotRmdirSourceDir(t *testing.T) {
	dir := t.TempDir()
	h := writeLocalFileExpectHash(t, dir, "music/track.mp3", "bytes")
	st := testStateFromArchive(map[string]types.FileState{
		"music/track.mp3": {LocalHash: h},
	})

	// Move event in the file changes list (not a dir delete). Drive
	// it through CompareIncremental: that path never invokes
	// expandArchiveDelete and therefore never emits RmdirLocal.
	plans := CompareIncremental(
		map[string]types.LocalFile{"music/track.mp3": {Hash: h}},
		st.Files,
		[]types.ChangeEntry{
			{Action: "move", IsDir: false, PathBefore: "/music/track.mp3", PathAfter: "/archive/track.mp3", Hash: "bytes", RevisionID: "r1"},
		},
	)
	for _, p := range plans {
		if p.Action == types.RmdirLocal {
			t.Errorf("file move must not produce RmdirLocal; got %+v", plans)
		}
	}
}

// Scenario: --max-delete aborts before execute runs, so RmdirLocal is
// never reached. Verified at the runner level — countPlanDeletes does
// not count RmdirLocal (rmdir is a post-action, not a delete itself).
func TestDirLifecycle_MaxDeleteCounter_IgnoresRmdir(t *testing.T) {
	plans := []types.SyncPlan{
		{Path: "music/a.mp3", Action: types.DeleteLocal},
		{Path: "music/b.mp3", Action: types.DeleteLocal},
		{Path: "music", Action: types.RmdirLocal},
	}
	got := countPlanDeletes(plans, nil)
	if got != 2 {
		t.Errorf("countPlanDeletes = %d, want 2 (rmdir is a post-action, not a delete)", got)
	}
}

// --- Defensive: malformed event paths -------------------------------

// Rule: dir snapshot items whose path normalizes to "" (the scope root
// itself) must NOT be emitted as MkdirLocal — that would target the
// sync root and is a no-op at best, a confusing error at worst.
func TestSnapshotToRemoteDirs_DropsScopeRootEntry(t *testing.T) {
	items := []types.SnapshotItem{
		{Path: "/", Type: "dir"},
		{Path: "", Type: "dir"},
		{Path: "/keep/", Type: "dir"},
	}
	dirs := SnapshotToRemoteDirs(items)
	if len(dirs) != 1 || dirs[0] != "keep" {
		t.Errorf("dirs = %v, want [keep]", dirs)
	}
}
