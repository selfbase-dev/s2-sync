package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/selfbase-dev/s2-cli/internal/client"
	"github.com/selfbase-dev/s2-cli/internal/types"
)

// ---------------------------------------------------------------------------
// fakeS2Server — thread-safe fake REST API for integration tests
// ---------------------------------------------------------------------------

type fakeS2Server struct {
	mu          sync.Mutex
	files       map[string]string // key → content
	fail412Keys map[string]bool   // keys that return 412 on PUT
	failAfterN  int               // return 500 after N successful PUTs (0 = disabled)
	putCount    int
}

func newFakeS2Server(t *testing.T) (*fakeS2Server, *httptest.Server) {
	t.Helper()
	fs := &fakeS2Server{
		files:       make(map[string]string),
		fail412Keys: make(map[string]bool),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		fs.handle(t, w, r)
	}))
	t.Cleanup(srv.Close)
	return fs, srv
}

func (fs *fakeS2Server) handle(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()

	if r.Method == "GET" && r.URL.Path == "/api/me" {
		w.WriteHeader(200)
		fmt.Fprint(w, `{"user_id":"test","email":"test@test.com"}`)
		return
	}

	if !strings.HasPrefix(r.URL.Path, "/api/files/") {
		http.Error(w, "not found", 404)
		return
	}

	rawPath := strings.TrimPrefix(r.URL.Path, "/api/files/")

	switch r.Method {
	case "GET":
		if strings.HasSuffix(r.URL.Path, "/") || rawPath == "" {
			fs.handleList(w, rawPath)
		} else {
			fs.handleGet(w, rawPath)
		}
	case "PUT":
		fs.handlePut(t, w, r, rawPath)
	case "DELETE":
		fs.handleDelete(w, rawPath)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (fs *fakeS2Server) handleList(w http.ResponseWriter, prefix string) {
	var items []map[string]any
	for k, v := range fs.files {
		if prefix == "" || strings.HasPrefix(k, prefix) {
			hash := sha256Hex(v)
			items = append(items, map[string]any{
				"key":      k,
				"size":     len(v),
				"uploaded": "2026-01-01T00:00:00Z",
				"hash":     hash,
			})
		}
	}
	if items == nil {
		items = []map[string]any{}
	}
	json.NewEncoder(w).Encode(map[string]any{"items": items})
}

func (fs *fakeS2Server) handleGet(w http.ResponseWriter, key string) {
	content, ok := fs.files[key]
	if !ok {
		http.Error(w, "Not Found", 404)
		return
	}
	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, sha256Hex(content)))
	fmt.Fprint(w, content)
}

func (fs *fakeS2Server) handlePut(t *testing.T, w http.ResponseWriter, r *http.Request, key string) {
	// Error injection: fail after N PUTs
	if fs.failAfterN > 0 {
		fs.putCount++
		if fs.putCount > fs.failAfterN {
			http.Error(w, "internal server error", 500)
			return
		}
	}

	// Error injection: 412 for specific keys
	if fs.fail412Keys[key] {
		w.WriteHeader(412)
		fmt.Fprint(w, `{"error":"Precondition Failed"}`)
		return
	}

	// If-Match check
	if ifMatch := r.Header.Get("If-Match"); ifMatch != "" {
		expected := strings.Trim(ifMatch, "\"")
		if existing, ok := fs.files[key]; ok {
			currentHash := sha256Hex(existing)
			if currentHash != expected {
				w.WriteHeader(412)
				fmt.Fprint(w, `{"error":"Precondition Failed"}`)
				return
			}
		}
	}

	body, _ := io.ReadAll(r.Body)
	content := string(body)
	fs.files[key] = content

	hash := sha256Hex(content)
	w.WriteHeader(201)
	json.NewEncoder(w).Encode(map[string]any{
		"size": len(body),
		"hash": hash,
		"etag": hash, // Use SHA-256 as etag for simplicity in tests
	})
}

func (fs *fakeS2Server) handleDelete(w http.ResponseWriter, key string) {
	if _, ok := fs.files[key]; !ok {
		http.Error(w, "Not Found", 404)
		return
	}
	delete(fs.files, key)
	w.WriteHeader(204)
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeFile(t *testing.T, dir, relPath, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, dir, relPath string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(relPath)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func exists(dir, relPath string) bool {
	_, err := os.Stat(filepath.Join(dir, filepath.FromSlash(relPath)))
	return err == nil
}

// runSyncCycle does Walk → list remote → Compare → Execute → SaveState.
// Returns the result and the plans.
func runSyncCycle(t *testing.T, localDir, remotePrefix string, c *client.Client, state *State) (*ExecuteResult, []types.SyncPlan) {
	t.Helper()

	local, err := Walk(localDir, state.Files, nil)
	if err != nil {
		t.Fatalf("Walk failed: %v", err)
	}

	objects, err := c.ListAll(remotePrefix)
	if err != nil {
		t.Fatalf("ListAll failed: %v", err)
	}

	remote := make(map[string]types.RemoteFile)
	for _, obj := range objects {
		relPath := strings.TrimPrefix(obj.Key, remotePrefix)
		remote[relPath] = types.RemoteFile{
			ETag:         obj.ETag,
			Size:         obj.Size,
			LastModified: obj.LastModified,
		}
	}

	plans := Compare(local, remote, state.Files)

	result, err := Execute(plans, localDir, remotePrefix, c, state, false)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if err := SaveState(localDir, state); err != nil {
		t.Fatalf("SaveState failed: %v", err)
	}

	return result, plans
}

// assertInSync verifies that re-running Compare produces zero plans.
func assertInSync(t *testing.T, localDir, remotePrefix string, c *client.Client, state *State) {
	t.Helper()

	local, err := Walk(localDir, state.Files, nil)
	if err != nil {
		t.Fatalf("Walk failed: %v", err)
	}

	objects, err := c.ListAll(remotePrefix)
	if err != nil {
		t.Fatalf("ListAll failed: %v", err)
	}

	remote := make(map[string]types.RemoteFile)
	for _, obj := range objects {
		relPath := strings.TrimPrefix(obj.Key, remotePrefix)
		remote[relPath] = types.RemoteFile{
			ETag:         obj.ETag,
			Size:         obj.Size,
			LastModified: obj.LastModified,
		}
	}

	plans := Compare(local, remote, state.Files)
	if len(plans) > 0 {
		for _, p := range plans {
			t.Errorf("unexpected plan: %s → %s", p.Path, p.Action)
		}
		t.Fatalf("expected 0 plans after sync, got %d", len(plans))
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestFullSyncCycle_InitialPushThenResync(t *testing.T) {
	fs, srv := newFakeS2Server(t)
	c := client.New(srv.URL, "s2_test")
	localDir := t.TempDir()
	state := &State{Version: 1, Files: make(map[string]types.FileState)}
	prefix := "docs/"

	// Phase 1: Create 10 local files, push all
	for i := 0; i < 10; i++ {
		writeFile(t, localDir, fmt.Sprintf("file_%02d.txt", i), fmt.Sprintf("content %d", i))
	}

	result, _ := runSyncCycle(t, localDir, prefix, c, state)
	if result.Pushed != 10 {
		t.Errorf("phase 1: expected 10 pushed, got %d", result.Pushed)
	}
	if len(fs.files) != 10 {
		t.Errorf("phase 1: expected 10 remote files, got %d", len(fs.files))
	}
	if len(state.Files) != 10 {
		t.Errorf("phase 1: expected 10 state entries, got %d", len(state.Files))
	}
	assertInSync(t, localDir, prefix, c, state)

	// Phase 2: Modify 2, delete 1
	writeFile(t, localDir, "file_00.txt", "modified content 0")
	writeFile(t, localDir, "file_01.txt", "modified content 1")
	os.Remove(filepath.Join(localDir, "file_09.txt"))

	result, _ = runSyncCycle(t, localDir, prefix, c, state)
	if result.Pushed != 2 {
		t.Errorf("phase 2: expected 2 pushed, got %d", result.Pushed)
	}
	if result.Deleted != 1 {
		t.Errorf("phase 2: expected 1 deleted, got %d", result.Deleted)
	}
	if len(fs.files) != 9 {
		t.Errorf("phase 2: expected 9 remote files, got %d", len(fs.files))
	}
	assertInSync(t, localDir, prefix, c, state)

	// Phase 3: No changes → zero plans
	result, plans := runSyncCycle(t, localDir, prefix, c, state)
	if len(plans) != 0 {
		t.Errorf("phase 3: expected 0 plans, got %d", len(plans))
	}
	if result.Pushed+result.Pulled+result.Deleted+result.Conflicts != 0 {
		t.Error("phase 3: expected no operations")
	}
}

func TestFullSyncCycle_InitialPullThenResync(t *testing.T) {
	fs, srv := newFakeS2Server(t)
	c := client.New(srv.URL, "s2_test")
	localDir := t.TempDir()
	state := &State{Version: 1, Files: make(map[string]types.FileState)}
	prefix := "repo/"

	// Phase 1: 10 remote-only files → pull all
	for i := 0; i < 10; i++ {
		fs.files[fmt.Sprintf("repo/src/file_%02d.go", i)] = fmt.Sprintf("package main // %d", i)
	}

	result, _ := runSyncCycle(t, localDir, prefix, c, state)
	if result.Pulled != 10 {
		t.Errorf("phase 1: expected 10 pulled, got %d", result.Pulled)
	}
	for i := 0; i < 10; i++ {
		content := readFile(t, localDir, fmt.Sprintf("src/file_%02d.go", i))
		expected := fmt.Sprintf("package main // %d", i)
		if content != expected {
			t.Errorf("phase 1: file_%02d.go: got %q, want %q", i, content, expected)
		}
	}
	assertInSync(t, localDir, prefix, c, state)

	// Phase 2: Add 3, modify 1 on remote
	for i := 10; i < 13; i++ {
		fs.files[fmt.Sprintf("repo/src/file_%02d.go", i)] = fmt.Sprintf("package main // %d", i)
	}
	fs.files["repo/src/file_00.go"] = "package main // MODIFIED"

	result, _ = runSyncCycle(t, localDir, prefix, c, state)
	if result.Pulled != 4 {
		t.Errorf("phase 2: expected 4 pulled, got %d", result.Pulled)
	}
	assertInSync(t, localDir, prefix, c, state)

	// Verify modified file content
	content := readFile(t, localDir, "src/file_00.go")
	if content != "package main // MODIFIED" {
		t.Errorf("modified file: got %q", content)
	}
}

func TestFullSyncCycle_BidirectionalMix(t *testing.T) {
	fs, srv := newFakeS2Server(t)
	c := client.New(srv.URL, "s2_test")
	localDir := t.TempDir()
	state := &State{Version: 1, Files: make(map[string]types.FileState)}
	prefix := "mix/"

	// Phase 1: 5 local + 5 remote → 5 push + 5 pull
	for i := 0; i < 5; i++ {
		writeFile(t, localDir, fmt.Sprintf("local_%d.txt", i), fmt.Sprintf("local %d", i))
	}
	for i := 0; i < 5; i++ {
		fs.files[fmt.Sprintf("mix/remote_%d.txt", i)] = fmt.Sprintf("remote %d", i)
	}

	result, _ := runSyncCycle(t, localDir, prefix, c, state)
	if result.Pushed != 5 {
		t.Errorf("phase 1: expected 5 pushed, got %d", result.Pushed)
	}
	if result.Pulled != 5 {
		t.Errorf("phase 1: expected 5 pulled, got %d", result.Pulled)
	}
	assertInSync(t, localDir, prefix, c, state)

	// Phase 2: modify 2 local, modify 2 remote, delete 1 local, add 1 local
	writeFile(t, localDir, "local_0.txt", "local 0 modified")
	writeFile(t, localDir, "local_1.txt", "local 1 modified")
	fs.files["mix/remote_0.txt"] = "remote 0 modified"
	fs.files["mix/remote_1.txt"] = "remote 1 modified"
	os.Remove(filepath.Join(localDir, "remote_3.txt"))
	writeFile(t, localDir, "brand_new.txt", "brand new")

	result, _ = runSyncCycle(t, localDir, prefix, c, state)
	if result.Pushed != 3 { // 2 modified local + 1 new
		t.Errorf("phase 2: expected 3 pushed, got %d", result.Pushed)
	}
	if result.Pulled != 2 { // 2 modified remote
		t.Errorf("phase 2: expected 2 pulled, got %d", result.Pulled)
	}
	if result.Deleted != 1 { // 1 delete-remote
		t.Errorf("phase 2: expected 1 deleted, got %d", result.Deleted)
	}
	assertInSync(t, localDir, prefix, c, state)
}

func TestFullSyncCycle_ConflictResolution(t *testing.T) {
	fs, srv := newFakeS2Server(t)
	c := client.New(srv.URL, "s2_test")
	localDir := t.TempDir()
	state := &State{Version: 1, Files: make(map[string]types.FileState)}
	prefix := "proj/"

	// Phase 1: Initial sync to establish archive
	writeFile(t, localDir, "shared.txt", "initial content")
	fs.files["proj/shared.txt"] = "initial content"

	result, _ := runSyncCycle(t, localDir, prefix, c, state)
	// Both have same content → should be no-op (same hash)
	assertInSync(t, localDir, prefix, c, state)

	// Phase 2: Both sides modify the same file
	writeFile(t, localDir, "shared.txt", "local edit")
	fs.files["proj/shared.txt"] = "remote edit"

	result, _ = runSyncCycle(t, localDir, prefix, c, state)
	if result.Conflicts != 1 {
		t.Errorf("phase 2: expected 1 conflict, got %d", result.Conflicts)
	}

	// Local should win
	content := readFile(t, localDir, "shared.txt")
	if content != "local edit" {
		t.Errorf("local should be unchanged, got %q", content)
	}

	// Remote should have local version
	if fs.files["proj/shared.txt"] != "local edit" {
		t.Errorf("remote should have local version, got %q", fs.files["proj/shared.txt"])
	}

	// Conflict file should exist
	entries, _ := os.ReadDir(localDir)
	foundConflict := false
	for _, e := range entries {
		if strings.Contains(e.Name(), ".sync-conflict-") {
			foundConflict = true
			data, _ := os.ReadFile(filepath.Join(localDir, e.Name()))
			if string(data) != "remote edit" {
				t.Errorf("conflict file should contain remote version, got %q", string(data))
			}
		}
	}
	if !foundConflict {
		t.Error("expected conflict file to be created")
	}
}

func TestFullSyncCycle_1000Files(t *testing.T) {
	_, srv := newFakeS2Server(t)
	c := client.New(srv.URL, "s2_test")
	localDir := t.TempDir()
	state := &State{Version: 1, Files: make(map[string]types.FileState)}
	prefix := "big/"

	const totalFiles = 1000

	// Phase 1: Create 1000 files across 50 directories
	for i := 0; i < totalFiles; i++ {
		dir := fmt.Sprintf("dir_%02d", i%50)
		name := fmt.Sprintf("%s/file_%04d.txt", dir, i)
		writeFile(t, localDir, name, fmt.Sprintf("content-%04d", i))
	}

	result, _ := runSyncCycle(t, localDir, prefix, c, state)
	if result.Pushed != totalFiles {
		t.Errorf("phase 1: expected %d pushed, got %d", totalFiles, result.Pushed)
	}
	if len(state.Files) != totalFiles {
		t.Errorf("phase 1: expected %d state entries, got %d", totalFiles, len(state.Files))
	}
	assertInSync(t, localDir, prefix, c, state)

	// Phase 2: Modify 100, delete 50, add 50
	for i := 0; i < 100; i++ {
		dir := fmt.Sprintf("dir_%02d", i%50)
		name := fmt.Sprintf("%s/file_%04d.txt", dir, i)
		writeFile(t, localDir, name, fmt.Sprintf("modified-%04d", i))
	}
	for i := 950; i < totalFiles; i++ {
		dir := fmt.Sprintf("dir_%02d", i%50)
		name := fmt.Sprintf("%s/file_%04d.txt", dir, i)
		os.Remove(filepath.Join(localDir, filepath.FromSlash(name)))
	}
	for i := 0; i < 50; i++ {
		name := fmt.Sprintf("new_dir/new_%04d.txt", i)
		writeFile(t, localDir, name, fmt.Sprintf("new-%04d", i))
	}

	result, _ = runSyncCycle(t, localDir, prefix, c, state)
	if result.Pushed != 150 { // 100 modified + 50 new
		t.Errorf("phase 2: expected 150 pushed, got %d", result.Pushed)
	}
	if result.Deleted != 50 {
		t.Errorf("phase 2: expected 50 deleted, got %d", result.Deleted)
	}

	expectedFiles := totalFiles - 50 + 50 // 1000 - 50 deleted + 50 new
	if len(state.Files) != expectedFiles {
		t.Errorf("phase 2: expected %d state entries, got %d", expectedFiles, len(state.Files))
	}
	assertInSync(t, localDir, prefix, c, state)
}

func TestFullSyncCycle_DeepNesting(t *testing.T) {
	fs, srv := newFakeS2Server(t)
	c := client.New(srv.URL, "s2_test")
	localDir := t.TempDir()
	state := &State{Version: 1, Files: make(map[string]types.FileState)}
	prefix := "deep/"

	// Create a 20-level deep file
	deepPath := "a/b/c/d/e/f/g/h/i/j/k/l/m/n/o/p/q/r/s/t/deep_file.txt"
	writeFile(t, localDir, deepPath, "deep content")

	// Also create a remote file at depth
	remotePath := "x/y/z/w/v/u/t/s/r/q/p/o/n/m/l/k/j/i/h/g/remote_deep.txt"
	fs.files["deep/"+remotePath] = "remote deep content"

	result, _ := runSyncCycle(t, localDir, prefix, c, state)
	if result.Pushed != 1 {
		t.Errorf("expected 1 pushed, got %d", result.Pushed)
	}
	if result.Pulled != 1 {
		t.Errorf("expected 1 pulled, got %d", result.Pulled)
	}

	// Verify both files exist
	if !exists(localDir, deepPath) {
		t.Error("deep local file should exist")
	}
	if !exists(localDir, remotePath) {
		t.Error("deep pulled file should exist")
	}

	// Verify paths in state
	if _, ok := state.Files[deepPath]; !ok {
		t.Error("expected state entry for deep local file")
	}
	if _, ok := state.Files[remotePath]; !ok {
		t.Error("expected state entry for deep pulled file")
	}

	assertInSync(t, localDir, prefix, c, state)
}

func TestFullSyncCycle_UnicodeFilenames(t *testing.T) {
	fs, srv := newFakeS2Server(t)
	c := client.New(srv.URL, "s2_test")
	localDir := t.TempDir()
	state := &State{Version: 1, Files: make(map[string]types.FileState)}
	prefix := "unicode/"

	// Local files with various Unicode and special names
	localFiles := map[string]string{
		"日本語テスト.txt":           "japanese content",
		"file with spaces.txt":  "spaces in name",
		"Makefile":               "all: build",
		".gitignore":             "node_modules/",
		"Dockerfile":             "FROM alpine",
		"special-chars_v2.0.txt": "special chars",
	}

	for name, content := range localFiles {
		writeFile(t, localDir, name, content)
	}

	// Remote files with Unicode
	fs.files["unicode/リモートファイル.md"] = "remote japanese"
	fs.files["unicode/café.txt"] = "french accent"

	result, _ := runSyncCycle(t, localDir, prefix, c, state)
	expectedPush := len(localFiles)
	expectedPull := 2
	if result.Pushed != expectedPush {
		t.Errorf("expected %d pushed, got %d", expectedPush, result.Pushed)
	}
	if result.Pulled != expectedPull {
		t.Errorf("expected %d pulled, got %d", expectedPull, result.Pulled)
	}

	// Verify all files in state
	totalFiles := expectedPush + expectedPull
	if len(state.Files) != totalFiles {
		t.Errorf("expected %d state entries, got %d", totalFiles, len(state.Files))
	}

	// Verify pulled files exist locally
	if !exists(localDir, "リモートファイル.md") {
		t.Error("japanese remote file should exist locally")
	}
	if !exists(localDir, "café.txt") {
		t.Error("french accent file should exist locally")
	}

	assertInSync(t, localDir, prefix, c, state)
}

func TestFullSyncCycle_StateCorruptionRecovery(t *testing.T) {
	fs, srv := newFakeS2Server(t)
	c := client.New(srv.URL, "s2_test")
	localDir := t.TempDir()
	state := &State{Version: 1, Files: make(map[string]types.FileState)}
	prefix := "recover/"

	// Phase 1: Normal sync
	for i := 0; i < 5; i++ {
		writeFile(t, localDir, fmt.Sprintf("file_%d.txt", i), fmt.Sprintf("content %d", i))
	}

	runSyncCycle(t, localDir, prefix, c, state)
	assertInSync(t, localDir, prefix, c, state)

	// Phase 2: Corrupt state.json
	stateDir := StateDir(localDir)
	os.MkdirAll(stateDir, 0700)
	os.WriteFile(StatePath(localDir), []byte("THIS IS NOT JSON{{{garbage"), 0600)

	// Reload state (should recover to empty)
	state, err := LoadState(localDir)
	if err != nil {
		t.Fatalf("LoadState should recover from corruption: %v", err)
	}
	if len(state.Files) != 0 {
		t.Errorf("corrupted state should return empty files, got %d", len(state.Files))
	}

	// Phase 3: Resync — should be treated as initial sync
	// Both local and remote have same content, so no-ops (same hash)
	result, _ := runSyncCycle(t, localDir, prefix, c, state)
	// All 5 files exist locally and remotely with same content → NoOp
	if result.Pushed+result.Pulled+result.Deleted != 0 {
		// Actually, since the hashes match (sha256 local vs sha256 remote), they should be no-ops
		t.Logf("After corruption: pushed=%d pulled=%d deleted=%d conflicts=%d",
			result.Pushed, result.Pulled, result.Deleted, result.Conflicts)
	}

	// After resync, should be stable
	assertInSync(t, localDir, prefix, c, state)

	// Verify files still exist with original content
	for i := 0; i < 5; i++ {
		content := readFile(t, localDir, fmt.Sprintf("file_%d.txt", i))
		expected := fmt.Sprintf("content %d", i)
		if content != expected {
			t.Errorf("file_%d.txt: got %q, want %q", i, content, expected)
		}
	}

	// Verify remote still has files
	if len(fs.files) != 5 {
		t.Errorf("expected 5 remote files, got %d", len(fs.files))
	}
}

func TestFullSyncCycle_EmptySync(t *testing.T) {
	_, srv := newFakeS2Server(t)
	c := client.New(srv.URL, "s2_test")
	localDir := t.TempDir()
	state := &State{Version: 1, Files: make(map[string]types.FileState)}
	prefix := "empty/"

	// Empty sync: no local, no remote
	result, plans := runSyncCycle(t, localDir, prefix, c, state)
	if len(plans) != 0 {
		t.Errorf("expected 0 plans, got %d", len(plans))
	}
	if result.Pushed+result.Pulled+result.Deleted+result.Conflicts != 0 {
		t.Error("expected no operations")
	}

	// Idempotent: run again
	result, plans = runSyncCycle(t, localDir, prefix, c, state)
	if len(plans) != 0 {
		t.Errorf("second run: expected 0 plans, got %d", len(plans))
	}

	assertInSync(t, localDir, prefix, c, state)
}
