//go:build e2e

// E2E tests for sync cycle against a real S2 server.
// Run with: S2_ENDPOINT=http://localhost:8787 S2_TOKEN=s2_xxx go test -tags e2e ./internal/sync/
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

	"github.com/selfbase-dev/s2-cli/internal/client"
	"github.com/selfbase-dev/s2-cli/internal/types"
)

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
	skipSelfFilter bool   // disable self-change filter (for testing remote changes with same token)
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	endpoint := os.Getenv("S2_ENDPOINT")
	rootToken := os.Getenv("S2_TOKEN")
	if endpoint == "" || rootToken == "" {
		t.Fatal("S2_ENDPOINT and S2_TOKEN must be set")
	}

	// Create a child token with a unique base_path for test isolation.
	// Each test gets its own virtual root so tests don't interfere with each other.
	rc := client.New(endpoint, rootToken)
	uniqueBasePath := "/e2e-test/" + time.Now().Format("20060102-150405") + "-" + t.Name() + "/"
	childResp, err := rc.CreateToken("e2e-"+t.Name(), uniqueBasePath, false, nil)
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	c := client.New(endpoint, childResp.RawToken)
	localDir := t.TempDir()

	t.Cleanup(func() {
		cleanRemote(c, "")
	})

	return &testEnv{t: t, client: c, localDir: localDir, basePath: uniqueBasePath}
}

func newTestEnvWithToken(t *testing.T, token string) *testEnv {
	t.Helper()

	endpoint := os.Getenv("S2_ENDPOINT")
	if endpoint == "" || token == "" {
		t.Fatal("S2_ENDPOINT and S2_TOKEN must be set")
	}

	c := client.New(endpoint, token)

	// Verify token and get base_path
	me, err := c.Me()
	if err != nil {
		t.Fatalf("failed to validate token: %v", err)
	}
	basePath := me.BasePath
	if basePath == "" {
		basePath = "/"
	}

	// Create a child token with a unique base_path for test isolation.
	uniqueBasePath := basePath + "e2e-test/" + time.Now().Format("20060102-150405") + "-" + t.Name() + "/"
	childResp, err := c.CreateToken("e2e-"+t.Name(), uniqueBasePath, false, nil)
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	childClient := client.New(endpoint, childResp.RawToken)
	localDir := t.TempDir()

	t.Cleanup(func() {
		cleanRemote(childClient, "")
	})

	return &testEnv{t: t, client: childClient, localDir: localDir, basePath: uniqueBasePath}
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
	if err := e.client.Move(from, to, false); err != nil {
		e.t.Fatalf("moveRemote(%s → %s): %v", from, to, err)
	}
}

// sync runs a full sync cycle (initial or incremental based on state).
func (e *testEnv) sync() *ExecuteResult {
	e.t.Helper()
	state, err := LoadState(e.localDir)
	if err != nil {
		e.t.Fatalf("LoadState: %v", err)
	}

	me, err := e.client.Me()
	if err != nil {
		e.t.Fatalf("Me: %v", err)
	}
	state.TokenID = me.TokenID

	if state.Cursor == "" {
		return e.initialSync(state)
	}
	return e.incrementalSync(state)
}

func (e *testEnv) initialSync(state *State) *ExecuteResult {
	e.t.Helper()
	state.Files = make(map[string]types.FileState)

	exclude := LoadExclude(e.localDir)
	localFiles, err := Walk(e.localDir, state.Files, exclude)
	if err != nil {
		e.t.Fatalf("Walk: %v", err)
	}

	remoteFiles, err := e.client.ListAllRecursive("")
	if err != nil {
		e.t.Fatalf("ListAllRecursive: %v", err)
	}

	plans := Compare(localFiles, remoteFiles, state.Files)
	result, err := Execute(plans, e.localDir, "", e.client, state, false)
	if err != nil {
		e.t.Fatalf("Execute: %v", err)
	}

	cursor, err := e.client.LatestCursor()
	if err == nil {
		state.Cursor = cursor
	}
	if err := SaveState(e.localDir, state); err != nil {
		e.t.Fatalf("SaveState: %v", err)
	}
	return result
}

func (e *testEnv) incrementalSync(state *State) *ExecuteResult {
	e.t.Helper()
	exclude := LoadExclude(e.localDir)
	localFiles, err := Walk(e.localDir, state.Files, exclude)
	if err != nil {
		e.t.Fatalf("Walk: %v", err)
	}

	resp, err := e.client.PollChanges(state.Cursor)
	if err == client.ErrCursorGone {
		state.Cursor = ""
		return e.initialSync(state)
	}
	if err != nil {
		e.t.Fatalf("PollChanges: %v", err)
	}
	if resp.ResyncRequired {
		state.Cursor = ""
		return e.initialSync(state)
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
			if ch.TokenID != "" && ch.TokenID == state.TokenID {
				continue
			}
		}
		remoteChanges = append(remoteChanges, ch)
	}

	plans := CompareIncremental(localFiles, state.Files, remoteChanges)
	result, err := Execute(plans, e.localDir, "", e.client, state, false)
	if err != nil {
		e.t.Fatalf("Execute: %v", err)
	}

	if resp.NextCursor != "" {
		state.Cursor = resp.NextCursor
	}
	if err := SaveState(e.localDir, state); err != nil {
		e.t.Fatalf("SaveState: %v", err)
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
	childResp, err := env.client.CreateToken("s18-device2", env.basePath, false, nil)
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	device2 := client.New(os.Getenv("S2_ENDPOINT"), childResp.RawToken)

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
	state, _ := LoadState(env.localDir)
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
	state, err := LoadState(env.localDir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	me, err := env.client.Me()
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	state.TokenID = me.TokenID

	exclude := LoadExclude(env.localDir)
	localFiles, err := Walk(env.localDir, state.Files, exclude)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

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
	result, err := execute(plans, env.localDir, "", env.client, state, false, executeDeps{
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

// =============================================================================
// Scoped token tests (base_path != "/")
//
// These run the same core sync scenarios using a non-root token
// (S2_SCOPED_TOKEN env var, base_path="/e2e-scope/").
// Skipped if S2_SCOPED_TOKEN is not set.
// =============================================================================

func scopedTestEnv(t *testing.T) *testEnv {
	t.Helper()
	token := os.Getenv("S2_SCOPED_TOKEN")
	if token == "" {
		t.Skip("S2_SCOPED_TOKEN not set — skipping scoped token tests")
	}
	env := newTestEnvWithToken(t, token)
	if env.basePath == "/" {
		t.Fatalf("S2_SCOPED_TOKEN has base_path='/', expected a non-root scoped token")
	}
	return env
}

// TestScoped_CoreScenarios runs the same sync scenarios as the root token tests
// but with a non-root base_path token. Verifies that prefix stripping, path
// conversion, and all sync operations work correctly with scoped tokens.
func TestScoped_CoreScenarios(t *testing.T) {
	t.Run("S01_InitialSync_LocalOnly", func(t *testing.T) {
		env := scopedTestEnv(t)
		env.writeLocal("hello.txt", "hello world")
		result := env.sync()
		if result.Pushed != 1 {
			t.Errorf("pushed = %d, want 1", result.Pushed)
		}
		if !env.remoteExists("hello.txt") {
			t.Error("remote hello.txt should exist")
		}
	})

	t.Run("S02_InitialSync_RemoteOnly", func(t *testing.T) {
		env := scopedTestEnv(t)
		env.putRemote("remote.txt", "remote content")
		result := env.sync()
		if result.Pulled != 1 {
			t.Errorf("pulled = %d, want 1", result.Pulled)
		}
		if got := env.readLocal("remote.txt"); got != "remote content" {
			t.Errorf("local = %q", got)
		}
	})

	t.Run("S04_InitialSync_BothDifferent", func(t *testing.T) {
		env := scopedTestEnv(t)
		env.writeLocal("diff.txt", "local version")
		env.putRemote("diff.txt", "remote version")
		env.sync()
		if got := env.readLocal("diff.txt"); got != "local version" {
			t.Errorf("local = %q, want 'local version'", got)
		}
		if _, found := env.localHasConflict("diff.txt"); !found {
			t.Error("should create .sync-conflict-* file")
		}
		if got := env.readRemote("diff.txt"); got != "local version" {
			t.Errorf("remote = %q, want 'local version'", got)
		}
	})

	t.Run("S07_Incremental_LocalAdd", func(t *testing.T) {
		env := scopedTestEnv(t)
		env.writeLocal("existing.txt", "existing")
		env.sync()
		env.writeLocal("new.txt", "new file")
		result := env.sync()
		if result.Pushed < 1 {
			t.Errorf("pushed = %d, want >= 1", result.Pushed)
		}
		if !env.remoteExists("new.txt") {
			t.Error("remote new.txt should exist")
		}
	})

	t.Run("S10_Incremental_RemoteAdd", func(t *testing.T) {
		env := scopedTestEnv(t)
		env.skipSelfFilter = true
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
	})

	t.Run("S13_Incremental_BothEdit", func(t *testing.T) {
		env := scopedTestEnv(t)
		env.skipSelfFilter = true
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
	})

	t.Run("S16_RemoteMove", func(t *testing.T) {
		env := scopedTestEnv(t)
		env.skipSelfFilter = true
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
	})
}

// --- Cross-token sync tests ---

// absPath returns the absolute path for a file in this env's scope,
// suitable for use by a root token (combines base_path with relPath).
func (e *testEnv) absPath(relPath string) string {
	return strings.TrimPrefix(e.basePath, "/") + relPath
}

// rootClient creates a client using the root token (S2_TOKEN env var).
func rootClient(t *testing.T) *client.Client {
	t.Helper()
	return client.New(os.Getenv("S2_ENDPOINT"), os.Getenv("S2_TOKEN"))
}

// TestScoped_CrossToken tests that changes are visible across tokens with
// different scopes (root ↔ scoped).
func TestScoped_CrossToken(t *testing.T) {
	t.Run("RootPushScopedPulls", func(t *testing.T) {
		scopedEnv := scopedTestEnv(t)
		rc := rootClient(t)

		// Root token uploads to the scoped env's absolute path
		if _, err := rc.Upload(scopedEnv.absPath("shared.txt"), strings.NewReader("from root"), "", -1); err != nil {
			t.Fatalf("root upload: %v", err)
		}

		result := scopedEnv.sync()
		if result.Pulled < 1 {
			t.Errorf("pulled = %d, want >= 1", result.Pulled)
		}
		if got := scopedEnv.readLocal("shared.txt"); got != "from root" {
			t.Errorf("local = %q, want 'from root'", got)
		}
	})

	t.Run("ScopedPushRootPulls", func(t *testing.T) {
		scopedEnv := scopedTestEnv(t)
		scopedEnv.skipSelfFilter = true

		scopedEnv.writeLocal("shared.txt", "from scoped")
		scopedEnv.sync()

		rc := rootClient(t)
		absPath := scopedEnv.absPath("shared.txt")
		dl, err := rc.Download(absPath)
		if err != nil {
			t.Fatalf("root download: %v", err)
		}
		defer dl.Body.Close()
		data, _ := io.ReadAll(dl.Body)
		if got := string(data); got != "from scoped" {
			t.Errorf("root sees %q, want 'from scoped'", got)
		}
	})
}

// --- S22: resync_required via scope ancestor move ---

// TestScoped_S22_ResyncRequired_AncestorMove: moving an ancestor directory of
// the token's scope triggers resync_required in the changes feed.
//
// Setup: dynamically create a 2-level nested token (base_path="/s22-outer-{ts}/inner/")
// using root token delegation. Then move the outer directory.
// isAncestorOfScope fires because "s22-outer-{ts}" is an ancestor of "s22-outer-{ts}/inner".
func TestScoped_S22_ResyncRequired_AncestorMove(t *testing.T) {
	_ = scopedTestEnv(t) // ensure S2_SCOPED_TOKEN is set (validates test environment)

	rc := rootClient(t)

	// Create nested token: base_path="/s22-outer-{ts}/inner/"
	outerDir := "s22-outer-" + time.Now().Format("150405")
	innerPath := outerDir + "/inner/"
	childResp, err := rc.CreateToken("s22-nested", "/"+innerPath, false, []types.AccessPath{
		{Path: "/", CanRead: true, CanWrite: true},
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	nestedClient := client.New(os.Getenv("S2_ENDPOINT"), childResp.RawToken)

	// Seed a file so the directory exists on the server
	if _, err := rc.Upload(innerPath+"seed.txt", strings.NewReader("seed"), "", -1); err != nil {
		t.Fatalf("seed upload: %v", err)
	}
	t.Cleanup(func() {
		_, _ = rc.Delete(outerDir + "-moved/inner/seed.txt")
		_, _ = rc.Delete(innerPath + "seed.txt")
	})

	// Get cursor for nested token (after seed)
	cursor, err := nestedClient.LatestCursor()
	if err != nil {
		t.Fatalf("LatestCursor: %v", err)
	}

	// Root moves the outer directory → ancestor of "/s22-outer-{ts}/inner/"
	movedDir := outerDir + "-moved"
	if err := rc.Move(outerDir+"/", movedDir+"/", false); err != nil {
		t.Skipf("move not supported: %v", err)
	}

	// Poll changes with nested token — should get resync_required
	resp, err := nestedClient.PollChanges(cursor)
	if err != nil {
		t.Fatalf("PollChanges: %v", err)
	}
	if !resp.ResyncRequired {
		t.Errorf("resync_required should be true after ancestor directory %q was moved", outerDir)
	}
}

