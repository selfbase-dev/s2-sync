//go:build e2e

// E2E tests for sync cycle against a real S2 server.
// Run with: S2_ENDPOINT=http://localhost:8787 S2_TOKEN=s2_xxx \
//          [S2_BASE_PATH=/scope/] go test -tags e2e ./internal/sync/
//
// S2_TOKEN is the parent token used to spawn per-test child tokens via
// delegation. S2_BASE_PATH is its absolute base_path (the API never
// exposes it to clients); defaults to "/" when unset, which works for a
// root-scoped parent token.
//
// Scenario IDs correspond to ADR 0032.

package sync

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/selfbase-dev/s2-sync/internal/auth"
	"github.com/selfbase-dev/s2-sync/internal/client"
	"github.com/selfbase-dev/s2-sync/internal/types"
)

// parentBasePath returns S2_BASE_PATH (defaulting to "/"), with a
// trailing slash so callers can concatenate child paths. The server
// keeps base_path opaque, so the test runner has to declare it.
func parentBasePath() string {
	bp := os.Getenv("S2_BASE_PATH")
	if bp == "" {
		bp = "/"
	}
	if !strings.HasSuffix(bp, "/") {
		bp += "/"
	}
	return bp
}

// --- Scenario registry (ADR 0032 テスト漏れ防止) ---

var allScenarios = []string{
	"S01", "S02", "S03", "S04",
	"S07", "S08", "S09",
	"S10", "S11", "S12", "S13", "S14", "S15", "S16", "S17", "S18",
	"S19", "S21", "S22",
	"S25",
	"S31", "S32",
}

var implementedScenarios = map[string]bool{}

func markScenario(id string) {
	implementedScenarios[id] = true
}

// TestMain runs all tests, then checks scenario coverage.
// Coverage check only triggers when scenario tests actually ran.
func TestMain(m *testing.M) {
	code := m.Run()
	if len(implementedScenarios) > 0 {
		for _, id := range allScenarios {
			if !implementedScenarios[id] {
				fmt.Fprintf(os.Stderr, "COVERAGE GAP: scenario %s has no test\n", id)
				if code == 0 {
					code = 1
				}
			}
		}
	}
	os.Exit(code)
}

// --- Test environment ---

type testEnv struct {
	t              *testing.T
	client         *client.Client
	localDir       string
	basePath       string // token's virtual root (e.g. "/" or "/e2e-scope/")
	identity       Identity
	skipSelfFilter bool // disable self-change filter (for testing remote changes with same token)
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	endpoint := os.Getenv("S2_ENDPOINT")
	token := os.Getenv("S2_TOKEN")
	if endpoint == "" || token == "" {
		t.Fatal("S2_ENDPOINT and S2_TOKEN must be set")
	}

	// Use the configured parent base_path (server keeps it opaque).
	rc := client.New(endpoint, auth.NewStaticSource(token))
	parentBase := parentBasePath()

	// Create a child token with a unique base_path for test isolation.
	// Each test gets its own virtual root so tests don't interfere with each other.
	uniqueBasePath := parentBase + "e2e-test/" + time.Now().Format("20060102-150405") + "-" + t.Name() + "/"
	childResp, err := rc.CreateToken("e2e-"+t.Name(), uniqueBasePath, true, []types.AccessPath{{Path: "/", CanRead: true, CanWrite: true}})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	c := client.New(endpoint, auth.NewStaticSource(childResp.RawToken))
	localDir := t.TempDir()

	t.Cleanup(func() {
		cleanRemote(c, "")
	})

	childTI, err := c.Introspect()
	if err != nil {
		t.Fatalf("child Introspect: %v", err)
	}

	return &testEnv{
		t:        t,
		client:   c,
		localDir: localDir,
		basePath: uniqueBasePath,
		identity: Identity{Endpoint: endpoint, UserID: childTI.UserID, TokenID: childTI.TokenID},
	}
}

func cleanRemote(c *client.Client, prefix string) {
	files, err := c.ListAllRecursive(prefix)
	if err != nil {
		return
	}
	for path := range files {
		_, _ = c.Delete(prefix + path)
	}
}

// writeLocal creates a local file with content.
func (e *testEnv) writeLocal(relPath, content string) {
	e.t.Helper()
	fullPath := filepath.Join(e.localDir, filepath.FromSlash(relPath))
	os.MkdirAll(filepath.Dir(fullPath), 0755)
	if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
		e.t.Fatalf("writeLocal(%s): %v", relPath, err)
	}
}

// readLocal reads a local file's content.
func (e *testEnv) readLocal(relPath string) string {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.localDir, filepath.FromSlash(relPath)))
	if err != nil {
		e.t.Fatalf("readLocal(%s): %v", relPath, err)
	}
	return string(data)
}

// localExists checks if a local file exists.
func (e *testEnv) localExists(relPath string) bool {
	_, err := os.Stat(filepath.Join(e.localDir, filepath.FromSlash(relPath)))
	return err == nil
}

// localHasConflict checks if any .sync-conflict-* file exists for the given base name.
func (e *testEnv) localHasConflict(baseName string) (string, bool) {
	e.t.Helper()
	entries, _ := os.ReadDir(e.localDir)
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".sync-conflict-") && strings.Contains(entry.Name(), strings.TrimSuffix(baseName, filepath.Ext(baseName))) {
			return entry.Name(), true
		}
	}
	return "", false
}

// putRemote uploads content to the remote server.
func (e *testEnv) putRemote(relPath, content string) {
	e.t.Helper()
	_, err := e.client.Upload(relPath, strings.NewReader(content), "", -1)
	if err != nil {
		e.t.Fatalf("putRemote(%s): %v", relPath, err)
	}
}

// readRemote downloads a file from the remote server.
func (e *testEnv) readRemote(relPath string) string {
	e.t.Helper()
	dl, err := e.client.Download(relPath)
	if err != nil {
		e.t.Fatalf("readRemote(%s): %v", relPath, err)
	}
	defer dl.Body.Close()
	data, _ := io.ReadAll(dl.Body)
	return string(data)
}

// remoteExists checks if a file exists on the remote server.
func (e *testEnv) remoteExists(relPath string) bool {
	_, _, err := e.client.HeadFile(relPath)
	return err == nil
}

// deleteRemote deletes a file on the remote server.
func (e *testEnv) deleteRemote(relPath string) {
	e.t.Helper()
	if _, err := e.client.Delete(relPath); err != nil {
		e.t.Fatalf("deleteRemote(%s): %v", relPath, err)
	}
}

// moveRemote moves a file on the remote server.
func (e *testEnv) moveRemote(from, to string) {
	e.t.Helper()
	if _, err := e.client.Move(from, to); err != nil {
		e.t.Fatalf("moveRemote(%s → %s): %v", from, to, err)
	}
}

// sync runs a full sync cycle (initial or incremental based on state).
func (e *testEnv) sync() *ExecuteResult {
	e.t.Helper()
	state, err := LoadState(e.localDir, e.identity)
	if err != nil {
		e.t.Fatalf("LoadState: %v", err)
	}
	defer state.Close()

	if state.Cursor == "" {
		return e.initialSync(state)
	}
	return e.incrementalSync(state)
}

func (e *testEnv) initialSync(state *State) *ExecuteResult {
	e.t.Helper()
	state.ClearFiles()

	exclude := LoadExclude(e.localDir)
	walkRes, err := Walk(e.localDir, exclude)
	if err != nil {
		e.t.Fatalf("Walk: %v", err)
	}
	localFiles := walkRes.Files

	caseInsensitive := IsCaseInsensitiveFS(e.localDir)

	// ADR 0039/0040: atomic snapshot + cursor.
	remoteFiles, snapshotCursor, err := FetchSnapshotAsRemoteFiles(e.client, "")
	if err != nil {
		e.t.Fatalf("Snapshot: %v", err)
	}
	remoteFiles, _ = NormalizeRemoteMap(remoteFiles, caseInsensitive)
	PrefillArchiveForIdempotentApply(state, localFiles, remoteFiles)

	plans := Compare(localFiles, remoteFiles, state.Files)
	plans = MergeCaseOnlyRenames(plans, localFiles, state.Files)
	plans = NeutralizeLocalRemoteCaseCollisions(plans, localFiles, state.Files, caseInsensitive)
	result, err := Execute(plans, e.localDir, e.client, state, false)
	if err != nil {
		e.t.Fatalf("Execute: %v", err)
	}

	if snapshotCursor != "" {
		state.Cursor = snapshotCursor
	}
	if err := state.Save(); err != nil {
		e.t.Fatalf("Save: %v", err)
	}
	return result
}

func (e *testEnv) incrementalSync(state *State) *ExecuteResult {
	e.t.Helper()
	exclude := LoadExclude(e.localDir)
	walkRes, err := Walk(e.localDir, exclude)
	if err != nil {
		e.t.Fatalf("Walk: %v", err)
	}
	localFiles := walkRes.Files
	caseInsensitive := IsCaseInsensitiveFS(e.localDir)

	resp, err := e.client.PollChanges(state.Cursor)
	if err == client.ErrCursorGone {
		state.Cursor = ""
		return e.initialSync(state)
	}
	if err != nil {
		e.t.Fatalf("PollChanges: %v", err)
	}

	// Filter self-changes (disabled when skipSelfFilter is set,
	// since test uses same token for both local sync and remote changes).
	// Server returns base_path-relative client paths — no prefix stripping needed.
	var remoteChanges []types.ChangeEntry
	for _, ch := range resp.Changes {
		if !e.skipSelfFilter {
			if state.IsPushedSeq(ch.Seq) {
				continue
			}
		}
		remoteChanges = append(remoteChanges, ch)
	}

	// Hybrid strategy: split dir events (archive walk / snapshot fetch)
	// from file events (CompareIncremental) — see ADR 0040.
	var dirChanges, fileChanges []types.ChangeEntry
	for _, ch := range remoteChanges {
		if ch.IsDir {
			dirChanges = append(dirChanges, ch)
		} else {
			fileChanges = append(fileChanges, ch)
		}
	}

	dirOutcome, err := HandleIncrementalDirEvents(
		e.client, e.localDir, state, dirChanges,
	)
	if err != nil {
		e.t.Fatalf("HandleIncrementalDirEvents: %v", err)
	}
	for i := range dirOutcome.SubtreeSnapshots {
		filtered, _ := NormalizeRemoteMap(dirOutcome.SubtreeSnapshots[i].Remote, caseInsensitive)
		dirOutcome.SubtreeSnapshots[i].Remote = filtered
	}
	if dirOutcome.LocalChanged || len(dirOutcome.SubtreeSnapshots) > 0 {
		walkRes, err = Walk(e.localDir, exclude)
		if err != nil {
			e.t.Fatalf("Walk (post dir events): %v", err)
		}
		localFiles = walkRes.Files
	}

	subtreePlans := dirOutcome.SubtreeComparePlans(localFiles, state.Files)
	fileLevelPlans := CompareIncremental(localFiles, state.Files, fileChanges)
	plans := MergePlansByPath(
		fileLevelPlans,
		subtreePlans,
		dirOutcome.ArchiveWalkPlans,
	)
	plans = MergeCaseOnlyRenames(plans, localFiles, state.Files)
	plans = NeutralizeLocalRemoteCaseCollisions(plans, localFiles, state.Files, caseInsensitive)

	result, err := Execute(plans, e.localDir, e.client, state, false)
	if err != nil {
		e.t.Fatalf("Execute: %v", err)
	}

	if dirOutcome.NewPrimaryCursor != "" {
		state.Cursor = dirOutcome.NewPrimaryCursor
	} else if resp.NextCursor != "" {
		state.Cursor = resp.NextCursor
	}
	if err := state.Save(); err != nil {
		e.t.Fatalf("Save: %v", err)
	}
	return result
}

// --- Initial sync scenarios ---

func TestS01_InitialSync_LocalOnly(t *testing.T) {
	markScenario("S01")
	env := newTestEnv(t)
	env.writeLocal("hello.txt", "hello world")

	result := env.sync()
	if result.Pushed != 1 {
		t.Errorf("pushed = %d, want 1", result.Pushed)
	}
	if !env.remoteExists("hello.txt") {
		t.Error("remote hello.txt should exist")
	}
}

func TestS02_InitialSync_RemoteOnly(t *testing.T) {
	markScenario("S02")
	env := newTestEnv(t)
	env.putRemote("remote.txt", "remote content")

	result := env.sync()
	if result.Pulled != 1 {
		t.Errorf("pulled = %d, want 1", result.Pulled)
	}
	if got := env.readLocal("remote.txt"); got != "remote content" {
		t.Errorf("local content = %q", got)
	}
}

func TestS03_InitialSync_BothSame(t *testing.T) {
	markScenario("S03")
	env := newTestEnv(t)
	env.writeLocal("same.txt", "same content")
	env.putRemote("same.txt", "same content")

	env.sync()
	if _, found := env.localHasConflict("same.txt"); found {
		t.Error("should not create conflict file for identical content")
	}
}

func TestS04_InitialSync_BothDifferent(t *testing.T) {
	markScenario("S04")
	env := newTestEnv(t)
	env.writeLocal("diff.txt", "local version")
	env.putRemote("diff.txt", "remote version")

	env.sync()

	// Local wins
	if got := env.readLocal("diff.txt"); got != "local version" {
		t.Errorf("local content = %q, want 'local version'", got)
	}
	// Remote version saved as conflict file
	if _, found := env.localHasConflict("diff.txt"); !found {
		t.Error("should create .sync-conflict-* file")
	}
	// Remote updated to local version
	if got := env.readRemote("diff.txt"); got != "local version" {
		t.Errorf("remote content = %q, want 'local version'", got)
	}
}

// --- Incremental sync scenarios ---

func TestS07_Incremental_LocalAdd(t *testing.T) {
	markScenario("S07")
	env := newTestEnv(t)
	env.writeLocal("existing.txt", "existing")
	env.sync() // initial

	env.writeLocal("new.txt", "new file")
	result := env.sync() // incremental
	if result.Pushed < 1 {
		t.Errorf("pushed = %d, want >= 1", result.Pushed)
	}
	if !env.remoteExists("new.txt") {
		t.Error("remote new.txt should exist")
	}
}

func TestS08_Incremental_LocalEdit(t *testing.T) {
	markScenario("S08")
	env := newTestEnv(t)
	env.writeLocal("file.txt", "original")
	env.sync()

	env.writeLocal("file.txt", "edited")
	result := env.sync()
	if result.Pushed < 1 {
		t.Errorf("pushed = %d", result.Pushed)
	}
	if got := env.readRemote("file.txt"); got != "edited" {
		t.Errorf("remote = %q, want 'edited'", got)
	}
}

func TestS09_Incremental_LocalDelete(t *testing.T) {
	markScenario("S09")
	env := newTestEnv(t)
	env.writeLocal("file.txt", "content")
	env.sync()

	os.Remove(filepath.Join(env.localDir, "file.txt"))
	result := env.sync()
	if result.Deleted < 1 {
		t.Errorf("deleted = %d", result.Deleted)
	}
	if env.remoteExists("file.txt") {
		t.Error("remote file.txt should be deleted")
	}
}

func TestS10_Incremental_RemoteAdd(t *testing.T) {
	markScenario("S10")
	env := newTestEnv(t)
	env.skipSelfFilter = true // same token simulates remote; disable self-change filter
	env.writeLocal("existing.txt", "existing")
	env.sync()

	env.putRemote("remote_new.txt", "from remote")
	result := env.sync()
	if result.Pulled < 1 {
		t.Errorf("pulled = %d", result.Pulled)
	}
	if got := env.readLocal("remote_new.txt"); got != "from remote" {
		t.Errorf("local = %q", got)
	}
}

func TestS11_Incremental_RemoteEdit(t *testing.T) {
	markScenario("S11")
	env := newTestEnv(t)
	env.skipSelfFilter = true // same token simulates remote; disable self-change filter
	env.writeLocal("file.txt", "original")
	env.sync()

	env.putRemote("file.txt", "updated by remote")
	result := env.sync()
	if result.Pulled < 1 {
		t.Errorf("pulled = %d", result.Pulled)
	}
	if got := env.readLocal("file.txt"); got != "updated by remote" {
		t.Errorf("local = %q", got)
	}
}

func TestS12_Incremental_RemoteDelete(t *testing.T) {
	markScenario("S12")
	env := newTestEnv(t)
	env.skipSelfFilter = true // same token simulates remote; disable self-change filter
	env.writeLocal("file.txt", "content")
	env.sync()

	env.deleteRemote("file.txt")
	result := env.sync()
	if result.Deleted < 1 {
		t.Errorf("deleted = %d", result.Deleted)
	}
	if env.localExists("file.txt") {
		t.Error("local file.txt should be deleted")
	}
}

func TestS13_Incremental_BothEdit(t *testing.T) {
	markScenario("S13")
	env := newTestEnv(t)
	env.skipSelfFilter = true // same token simulates remote; disable self-change filter
	env.writeLocal("file.txt", "original")
	env.sync()

	env.writeLocal("file.txt", "local edit")
	env.putRemote("file.txt", "remote edit")
	env.sync()

	if got := env.readLocal("file.txt"); got != "local edit" {
		t.Errorf("local = %q, want 'local edit'", got)
	}
	if _, found := env.localHasConflict("file.txt"); !found {
		t.Error("should create conflict file")
	}
}

func TestS14_Incremental_LocalDeleteRemoteEdit(t *testing.T) {
	markScenario("S14")
	env := newTestEnv(t)
	env.skipSelfFilter = true // same token simulates remote; disable self-change filter
	env.writeLocal("file.txt", "original")
	env.sync()

	os.Remove(filepath.Join(env.localDir, "file.txt"))
	env.putRemote("file.txt", "remote edit")
	env.sync()

	// Remote version should be saved as conflict or pulled
	// (conflict because local deleted + remote changed)
	if _, found := env.localHasConflict("file.txt"); !found {
		// It's OK if the remote version was pulled directly too
		if !env.localExists("file.txt") {
			t.Error("remote version should be preserved somehow")
		}
	}
}

func TestS15_Incremental_LocalEditRemoteDelete(t *testing.T) {
	markScenario("S15")
	env := newTestEnv(t)
	env.skipSelfFilter = true // same token simulates remote; disable self-change filter
	env.writeLocal("file.txt", "original")
	env.sync()

	env.writeLocal("file.txt", "local edit")
	env.deleteRemote("file.txt")
	env.sync()

	// Local edit should survive and be pushed
	if got := env.readLocal("file.txt"); got != "local edit" {
		t.Errorf("local = %q, want 'local edit'", got)
	}
}

func TestS16_Incremental_RemoteMove(t *testing.T) {
	markScenario("S16")
	env := newTestEnv(t)
	env.skipSelfFilter = true // same token simulates remote; disable self-change filter
	env.writeLocal("old.txt", "content")
	env.sync()

	env.moveRemote("old.txt", "new.txt")
	env.sync()

	if env.localExists("old.txt") {
		t.Error("old.txt should not exist locally")
	}
	if !env.localExists("new.txt") {
		t.Error("new.txt should exist locally")
	}
}

func TestS17_Incremental_SelfPushSkipped(t *testing.T) {
	markScenario("S17")
	env := newTestEnv(t)
	env.writeLocal("file.txt", "v1")
	env.sync()

	env.writeLocal("file.txt", "v2")
	env.sync() // push v2

	// Sync again — should not re-pull own change
	result := env.sync()
	if result.Pulled > 0 {
		t.Errorf("pulled = %d, want 0 (self-change should be skipped)", result.Pulled)
	}
	if got := env.readLocal("file.txt"); got != "v2" {
		t.Errorf("local = %q, should still be 'v2'", got)
	}
}

func TestS18_Incremental_OtherDevicePush(t *testing.T) {
	markScenario("S18")
	env := newTestEnv(t)

	// Create a child token with the same base_path to simulate a second device.
	// (SELF-240: create returns raw_token directly)
	childResp, err := env.client.CreateToken("s18-device2", env.basePath, false, []types.AccessPath{{Path: "/", CanRead: true, CanWrite: true}})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	device2 := client.New(os.Getenv("S2_ENDPOINT"), auth.NewStaticSource(childResp.RawToken))

	env.writeLocal("file.txt", "v1")
	env.sync() // initial sync with device 1

	// Device 2 pushes an update (paths are relative to device2's base_path)
	if _, err := device2.Upload("file.txt", strings.NewReader("v2-from-device2"), "", -1); err != nil {
		t.Fatalf("device2 upload: %v", err)
	}

	result := env.sync() // incremental sync on device 1
	if result.Pulled < 1 {
		t.Errorf("pulled = %d, want >= 1 (remote change from device2 should be pulled)", result.Pulled)
	}
	if got := env.readLocal("file.txt"); got != "v2-from-device2" {
		t.Errorf("local = %q, want 'v2-from-device2'", got)
	}
}

// --- CAS / Error scenarios ---

func TestS19_CAS_PreconditionFailed(t *testing.T) {
	markScenario("S19")
	env := newTestEnv(t)
	env.skipSelfFilter = true // same token simulates remote; disable self-change filter
	env.writeLocal("file.txt", "v1")
	env.sync()

	// Remote edit advances content_version
	env.putRemote("file.txt", "remote v2")
	// Local edit
	env.writeLocal("file.txt", "local v2")

	result := env.sync()
	// Incremental sync detects both-changed → conflict resolution (local wins).
	// CAS 412 would only happen on a direct push race, which incremental sync
	// avoids by detecting the conflict via changelog first.
	if result.Conflicts < 1 {
		t.Errorf("conflicts = %d, want >= 1 (both sides edited)", result.Conflicts)
	}
	// Local version should win
	if got := env.readLocal("file.txt"); got != "local v2" {
		t.Errorf("local = %q, want 'local v2'", got)
	}
}

func TestS21_CursorGone_FullResync(t *testing.T) {
	markScenario("S21")
	t.Skip("TODO: requires cursor expiration on server (7 day wait or DB manipulation)")
}

func TestS22_ResyncRequired(t *testing.T) {
	markScenario("S22")
	t.Skip("TODO: requires scope ancestor move to trigger resync_required")
}

// --- Chunked upload ---

func TestS25_ChunkedUpload(t *testing.T) {
	markScenario("S25")
	env := newTestEnv(t)

	// Create 11MB file (above ChunkedUploadThreshold of 10MB)
	bigContent := strings.Repeat("x", 11*1024*1024)
	env.writeLocal("big.bin", bigContent)

	result := env.sync()
	if result.Pushed != 1 {
		t.Errorf("pushed = %d, want 1", result.Pushed)
	}
	if !env.remoteExists("big.bin") {
		t.Error("remote big.bin should exist")
	}
}

// --- Safety ---

func TestS31_MaxDeleteAbort(t *testing.T) {
	markScenario("S31")
	env := newTestEnv(t)
	env.skipSelfFilter = true // same token simulates remote; disable self-change filter

	// Create 10 files and sync
	for i := 0; i < 10; i++ {
		env.writeLocal(filepath.Join("file"+string(rune('0'+i))+".txt"), "content")
	}
	env.sync()

	// Delete 8 files on remote (80% > default 50% threshold)
	for i := 0; i < 8; i++ {
		env.deleteRemote("file" + string(rune('0'+i)) + ".txt")
	}

	// Incremental sync should detect mass deletion
	// Note: the max-delete check is in cmd/sync.go, not in executor.
	// This test verifies the changes feed reports the deletes correctly.
	state, err := LoadState(env.localDir, env.identity)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	defer state.Close()
	resp, err := env.client.PollChanges(state.Cursor)
	if err != nil {
		t.Fatalf("PollChanges: %v", err)
	}

	deleteCount := 0
	for _, ch := range resp.Changes {
		if ch.Action == "delete" {
			deleteCount++
		}
	}
	if deleteCount < 8 {
		t.Errorf("expected >= 8 delete events, got %d", deleteCount)
	}
}

func TestS32_PullDuringLocalEdit(t *testing.T) {
	markScenario("S32")
	env := newTestEnv(t)
	env.skipSelfFilter = true

	env.writeLocal("file.txt", "v1")
	env.sync() // initial sync

	// Remote pushes v2
	env.putRemote("file.txt", "v2-from-remote")

	// Incremental sync with hook: simulate a concurrent local write
	// between download and commit.
	state, err := LoadState(env.localDir, env.identity)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	defer state.Close()
	exclude := LoadExclude(env.localDir)
	walkRes, err := Walk(env.localDir, exclude)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	localFiles := walkRes.Files

	resp, err := env.client.PollChanges(state.Cursor)
	if err != nil {
		t.Fatalf("PollChanges: %v", err)
	}

	// Server returns base_path-relative client paths — no prefix stripping needed.
	var remoteChanges []types.ChangeEntry
	for _, ch := range resp.Changes {
		remoteChanges = append(remoteChanges, ch)
	}

	plans := CompareIncremental(localFiles, state.Files, remoteChanges)

	localEdited := false
	result, err := execute(plans, env.localDir, env.client, state, false, executeDeps{
		beforePullCommit: func(localPath string) {
			// Simulate concurrent local edit during pull
			os.WriteFile(localPath, []byte("local-edit-during-pull"), 0644)
			localEdited = true
		},
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if !localEdited {
		t.Fatal("hook was not called")
	}
	// Pull should have been aborted — local edit wins
	if result.Pulled > 0 {
		t.Errorf("pulled = %d, want 0 (local edit should block commit)", result.Pulled)
	}
	if result.Conflicts < 1 {
		t.Errorf("conflicts = %d, want >= 1 (concurrent edit should be counted as conflict)", result.Conflicts)
	}
	if got := env.readLocal("file.txt"); got != "local-edit-during-pull" {
		t.Errorf("local = %q, want 'local-edit-during-pull'", got)
	}
}

// --- Cross-token sync tests ---

// parentRelPath returns a path relative to the parent token's scope,
// so the parent can access this env's files.
func (e *testEnv) parentRelPath(parentBasePath, relPath string) string {
	suffix := strings.TrimPrefix(e.basePath, parentBasePath)
	return suffix + relPath
}

// parentClient creates a client using S2_TOKEN (the parent token).
// The parent's absolute base_path is taken from S2_BASE_PATH (defaulting
// to "/") because the API does not expose it.
func parentClient(t *testing.T) (*client.Client, string) {
	t.Helper()
	c := client.New(os.Getenv("S2_ENDPOINT"), auth.NewStaticSource(os.Getenv("S2_TOKEN")))
	return c, parentBasePath()
}

// TestScoped_CrossToken tests that changes uploaded via one token are visible
// to a parent token with broader scope (child → parent and parent → child).
func TestScoped_CrossToken(t *testing.T) {
	t.Run("ParentPushChildPulls", func(t *testing.T) {
		childEnv := newTestEnv(t)
		pc, parentBase := parentClient(t)

		// Parent token uploads to the child's path (relative to parent scope)
		if _, err := pc.Upload(childEnv.parentRelPath(parentBase, "shared.txt"), strings.NewReader("from parent"), "", -1); err != nil {
			t.Fatalf("parent upload: %v", err)
		}

		result := childEnv.sync()
		if result.Pulled < 1 {
			t.Errorf("pulled = %d, want >= 1", result.Pulled)
		}
		if got := childEnv.readLocal("shared.txt"); got != "from parent" {
			t.Errorf("local = %q, want 'from parent'", got)
		}
	})

	t.Run("ChildPushParentPulls", func(t *testing.T) {
		childEnv := newTestEnv(t)
		childEnv.skipSelfFilter = true

		childEnv.writeLocal("shared.txt", "from child")
		childEnv.sync()

		pc, parentBase := parentClient(t)
		dl, err := pc.Download(childEnv.parentRelPath(parentBase, "shared.txt"))
		if err != nil {
			t.Fatalf("parent download: %v", err)
		}
		defer dl.Body.Close()
		data, _ := io.ReadAll(dl.Body)
		if got := string(data); got != "from child" {
			t.Errorf("parent sees %q, want 'from child'", got)
		}
	})
}

// --- S22: ancestor move → transformed scope-wide delete (ADR 0038 decision 4) ---

// TestScoped_S22_AncestorMoveBecomesDelete: moving an ancestor directory
// of the token's scope used to set `resync_required` on the changes
// feed (ADR 0038 original). SELF-287 replaced that flag with an explicit
// event transform: ChangeLogService rewrites the is_dir move into a
// `delete /` entry targeting every affected access_path, so the client
// can handle it via the hybrid strategy (ADR 0040 §操作×スコープマトリクス
// case #9 "ancestor move out").
func TestScoped_S22_AncestorMoveBecomesDelete(t *testing.T) {
	pc, parentBase := parentClient(t)

	// Create nested token under the parent's scope:
	// base_path = "{parentBase}s22-outer-{ts}/inner/"
	outerDir := "s22-outer-" + time.Now().Format("150405")
	innerPath := outerDir + "/inner/"
	nestedBasePath := parentBase + innerPath
	childResp, err := pc.CreateToken("s22-nested", nestedBasePath, false, []types.AccessPath{
		{Path: "/", CanRead: true, CanWrite: true},
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	nestedClient := client.New(os.Getenv("S2_ENDPOINT"), auth.NewStaticSource(childResp.RawToken))

	// Seed a file so the directory exists on the server
	if _, err := pc.Upload(innerPath+"seed.txt", strings.NewReader("seed"), "", -1); err != nil {
		t.Fatalf("seed upload: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pc.Delete(outerDir + "-moved/inner/seed.txt")
		_, _ = pc.Delete(innerPath + "seed.txt")
	})

	// Get cursor for nested token (after seed)
	cursor, err := nestedClient.LatestCursor()
	if err != nil {
		t.Fatalf("LatestCursor: %v", err)
	}

	// Parent moves the outer directory → ancestor of nested token's scope
	movedDir := outerDir + "-moved"
	if _, err := pc.Move(outerDir+"/", movedDir+"/"); err != nil {
		t.Skipf("move not supported: %v", err)
	}

	// Poll changes with nested token — the ancestor move should surface
	// as a transformed `delete /` entry (is_dir=true).
	resp, err := nestedClient.PollChanges(cursor)
	if err != nil {
		t.Fatalf("PollChanges: %v", err)
	}
	foundScopeDelete := false
	for _, ch := range resp.Changes {
		if ch.IsDir && ch.Action == "delete" && (ch.PathBefore == "/" || ch.PathBefore == "") {
			foundScopeDelete = true
			break
		}
	}
	if !foundScopeDelete {
		t.Errorf("expected a transformed `delete /` entry after ancestor move; got %+v", resp.Changes)
	}
}

// --- case-sensitivity and Unicode normalization ---

// On macOS the literal "à.txt" lands on disk as NFD ("a" + U+0300).
// After sync, the server side must see the NFC form so Linux/Windows
// clients treat it as the same file.
func TestCase_NFDUploadedAsNFC(t *testing.T) {
	env := newTestEnv(t)
	// Raw NFD: "a" + U+0300 combining grave, then ".txt"
	nfd := "à.txt"
	nfc := "à.txt"
	env.writeLocal(nfd, "bonjour")

	result := env.sync()
	if result.Pushed < 1 {
		t.Errorf("pushed = %d, want >= 1", result.Pushed)
	}
	if !env.remoteExists(nfc) {
		t.Errorf("remote should have NFC %q, got none", nfc)
	}
	if got := env.readRemote(nfc); got != "bonjour" {
		t.Errorf("remote content = %q", got)
	}
}

// Case-only rename (Mac: file.txt → File.txt) must propagate via the
// server MOVE API as a single "move" changelog entry, not as
// delete+put. The atomicity is visible via revision_id: after a MOVE
// the destination's revision id matches the source's (history
// preserved), whereas delete+put creates a fresh revision.
func TestCase_CaseOnlyRename_UsesMOVE(t *testing.T) {
	env := newTestEnv(t)
	env.writeLocal("file.txt", "content")
	env.sync()
	origRev := env.remoteRevisionID("file.txt")
	if origRev == "" {
		t.Fatal("expected revision_id for file.txt after initial sync")
	}

	// Rename locally: Mac is case-insensitive but case-preserving,
	// so this creates a distinct walk-level path with the same inode.
	if err := os.Rename(
		filepath.Join(env.localDir, "file.txt"),
		filepath.Join(env.localDir, "File.txt"),
	); err != nil {
		t.Fatalf("rename: %v", err)
	}

	result := env.sync()
	if result.Moved != 1 {
		t.Errorf("moved = %d, want 1 (case-only rename should use MOVE)", result.Moved)
	}

	// Server should now have File.txt with the SAME revision id —
	// proving MOVE was used, not delete+put.
	if !env.remoteExists("File.txt") {
		t.Fatal("remote should have File.txt after rename")
	}
	newRev := env.remoteRevisionID("File.txt")
	if newRev != origRev {
		t.Errorf("revision_id changed: %s → %s (MOVE should preserve it)", origRev, newRev)
	}
	if env.remoteExists("file.txt") {
		// On case-sensitive server, old path should be gone after MOVE.
		t.Error("remote should no longer have old file.txt")
	}
}

// Server has File.txt and file.txt both. On case-insensitive local FS,
// only the canonical-sort-first (File.txt, because 'F' < 'f') is
// pulled; the other is surfaced as a warning and not written. Sync
// does not stop.
func TestCase_RemoteCollision_Skipped(t *testing.T) {
	if !IsCaseInsensitiveFS(os.TempDir()) {
		t.Skip("local FS is case-sensitive; cannot verify skip behavior")
	}
	env := newTestEnv(t)
	env.putRemote("File.txt", "upper")
	env.putRemote("file.txt", "lower")

	env.sync()

	// Lex-first "File.txt" should be present locally
	if got := env.readLocal("File.txt"); got != "upper" {
		t.Errorf("local File.txt = %q, want 'upper'", got)
	}
	// "file.txt" should NOT have been pulled (would have overwritten
	// the inode of File.txt on case-insensitive FS).
	localPath := filepath.Join(env.localDir, "file.txt")
	info, err := os.Stat(localPath)
	if err == nil {
		// Same inode is OK (they're the same file on case-insensitive FS)
		// but content must still be File.txt's content.
		if content, _ := os.ReadFile(localPath); string(content) != "upper" {
			t.Errorf("lower-case variant content = %q, want 'upper' (same-inode ok)", string(content))
		}
		_ = info
	}
}

// remoteRevisionID helper — looks up the current revision id of a
// remote path via snapshot. Returns "" if not found.
func (e *testEnv) remoteRevisionID(relPath string) string {
	e.t.Helper()
	resp, err := e.client.Snapshot("")
	if err != nil {
		e.t.Fatalf("Snapshot: %v", err)
	}
	full := strings.TrimSuffix(e.basePath, "/") + "/" + relPath
	for _, it := range resp.Items {
		if it.Path == full || it.Path == "/"+relPath {
			return it.RevisionID
		}
	}
	return ""
}
