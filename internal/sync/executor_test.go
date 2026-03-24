package sync

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/selfbase-dev/s2-cli/internal/client"
	"github.com/selfbase-dev/s2-cli/internal/types"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// fakeServer creates a minimal REST API server for executor tests.
// files map: key → content (simulates remote storage).
func fakeServer(t *testing.T, files map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/api/files/")

		switch r.Method {
		case "GET":
			if strings.HasSuffix(r.URL.Path, "/") {
				// List
				var items []map[string]any
				for k, v := range files {
					items = append(items, map[string]any{
						"key": k, "size": len(v), "uploaded": "2026-01-01T00:00:00Z", "hash": fmt.Sprintf("hash_%s", k),
					})
				}
				json.NewEncoder(w).Encode(map[string]any{"items": items})
				return
			}
			// Download
			content, ok := files[key]
			if !ok {
				http.Error(w, "Not Found", 404)
				return
			}
			w.Header().Set("ETag", fmt.Sprintf(`"hash_%s"`, key))
			fmt.Fprint(w, content)

		case "PUT":
			body, _ := io.ReadAll(r.Body)
			// Check If-Match
			if ifMatch := r.Header.Get("If-Match"); ifMatch != "" {
				expected := strings.Trim(ifMatch, "\"")
				currentHash := fmt.Sprintf("hash_%s", key)
				if _, exists := files[key]; !exists || currentHash != expected {
					w.WriteHeader(412)
					return
				}
			}
			files[key] = string(body)
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{
				"size": len(body),
				"hash": fmt.Sprintf("sha256_%s", key),
				"etag": fmt.Sprintf("new_hash_%s", key),
			})

		case "DELETE":
			if _, ok := files[key]; !ok {
				http.Error(w, "Not Found", 404)
				return
			}
			delete(files, key)
			w.WriteHeader(204)

		default:
			http.Error(w, "Method Not Allowed", 405)
		}
	}))
}

func writeLocalFile(t *testing.T, dir, relPath, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func readLocalFile(t *testing.T, dir, relPath string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(relPath)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func fileExists(dir, relPath string) bool {
	_, err := os.Stat(filepath.Join(dir, filepath.FromSlash(relPath)))
	return err == nil
}

// ---------------------------------------------------------------------------
// conflictFileName tests
// ---------------------------------------------------------------------------

func TestConflictFileName(t *testing.T) {
	tests := []struct {
		input       string
		wantPrefix  string
		wantContain string
		wantSuffix  string
	}{
		{
			input:       "/tmp/report.txt",
			wantPrefix:  "/tmp/report.sync-conflict-",
			wantContain: ".sync-conflict-",
			wantSuffix:  ".txt",
		},
		{
			input:       "/tmp/Makefile",
			wantPrefix:  "/tmp/Makefile.sync-conflict-",
			wantContain: ".sync-conflict-",
			wantSuffix:  "",
		},
		{
			input:       "/tmp/archive.tar.gz",
			wantPrefix:  "/tmp/archive.tar.sync-conflict-",
			wantContain: ".sync-conflict-",
			wantSuffix:  ".gz",
		},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := conflictFileName(tt.input)

			if tt.wantPrefix != "" && !strings.HasPrefix(got, tt.wantPrefix) {
				t.Errorf("expected prefix %q, got %q", tt.wantPrefix, got)
			}

			if !strings.Contains(got, tt.wantContain) {
				t.Errorf("expected to contain %q, got %q", tt.wantContain, got)
			}

			if tt.wantSuffix != "" {
				if !strings.HasSuffix(got, tt.wantSuffix) {
					t.Errorf("expected suffix %q, got %q", tt.wantSuffix, got)
				}
			} else {
				if got[len(got)-1] == '.' {
					t.Errorf("should not end with dot: %s", got)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Execute: Push
// ---------------------------------------------------------------------------

func TestExecute_Push(t *testing.T) {
	remoteFiles := map[string]string{}
	srv := fakeServer(t, remoteFiles)
	defer srv.Close()

	localDir := t.TempDir()
	writeLocalFile(t, localDir, "hello.txt", "hello world")

	c := client.New(srv.URL, "s2_test")
	state := &State{Files: make(map[string]types.FileState)}

	plans := []types.SyncPlan{
		{Path: "hello.txt", Action: types.Push},
	}

	result, err := Execute(plans, localDir, "docs/", c, state, false)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result.Pushed != 1 {
		t.Errorf("expected 1 push, got %d", result.Pushed)
	}
	if len(result.Errors) > 0 {
		t.Errorf("unexpected errors: %v", result.Errors)
	}

	// Verify remote received the file
	if remoteFiles["docs/hello.txt"] != "hello world" {
		t.Errorf("remote file content: got %q", remoteFiles["docs/hello.txt"])
	}

	// Verify state was updated
	fs, ok := state.Files["hello.txt"]
	if !ok {
		t.Fatal("expected state entry for hello.txt")
	}
	if fs.LocalHash == "" {
		t.Error("expected non-empty local hash")
	}
	if fs.RemoteETag == "" {
		t.Error("expected non-empty remote etag")
	}
}

// ---------------------------------------------------------------------------
// Execute: Pull
// ---------------------------------------------------------------------------

func TestExecute_Pull(t *testing.T) {
	remoteFiles := map[string]string{
		"docs/readme.md": "# Hello from remote",
	}
	srv := fakeServer(t, remoteFiles)
	defer srv.Close()

	localDir := t.TempDir()
	c := client.New(srv.URL, "s2_test")
	state := &State{Files: make(map[string]types.FileState)}

	plans := []types.SyncPlan{
		{Path: "readme.md", Action: types.Pull},
	}

	result, err := Execute(plans, localDir, "docs/", c, state, false)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result.Pulled != 1 {
		t.Errorf("expected 1 pull, got %d", result.Pulled)
	}

	// Verify local file was created
	content := readLocalFile(t, localDir, "readme.md")
	if content != "# Hello from remote" {
		t.Errorf("local file content: got %q", content)
	}

	// Verify state was updated
	fs := state.Files["readme.md"]
	if fs.RemoteETag == "" {
		t.Error("expected non-empty remote etag in state")
	}
}

func TestExecute_Pull_CreatesNestedDirs(t *testing.T) {
	remoteFiles := map[string]string{
		"project/src/components/Button.tsx": "export default Button;",
	}
	srv := fakeServer(t, remoteFiles)
	defer srv.Close()

	localDir := t.TempDir()
	c := client.New(srv.URL, "s2_test")
	state := &State{Files: make(map[string]types.FileState)}

	plans := []types.SyncPlan{
		{Path: "src/components/Button.tsx", Action: types.Pull},
	}

	result, err := Execute(plans, localDir, "project/", c, state, false)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result.Pulled != 1 {
		t.Errorf("expected 1 pull, got %d", result.Pulled)
	}

	content := readLocalFile(t, localDir, "src/components/Button.tsx")
	if content != "export default Button;" {
		t.Errorf("unexpected content: %q", content)
	}
}

// ---------------------------------------------------------------------------
// Execute: Delete Local
// ---------------------------------------------------------------------------

func TestExecute_DeleteLocal(t *testing.T) {
	localDir := t.TempDir()
	writeLocalFile(t, localDir, "old.txt", "old content")

	remoteFiles := map[string]string{}
	srv := fakeServer(t, remoteFiles)
	defer srv.Close()

	c := client.New(srv.URL, "s2_test")
	state := &State{Files: map[string]types.FileState{
		"old.txt": {LocalHash: "xxx", RemoteETag: "yyy"},
	}}

	plans := []types.SyncPlan{
		{Path: "old.txt", Action: types.DeleteLocal},
	}

	result, err := Execute(plans, localDir, "docs/", c, state, false)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result.Deleted != 1 {
		t.Errorf("expected 1 delete, got %d", result.Deleted)
	}
	if fileExists(localDir, "old.txt") {
		t.Error("expected old.txt to be deleted")
	}
	if _, ok := state.Files["old.txt"]; ok {
		t.Error("expected state entry to be removed")
	}
}

// ---------------------------------------------------------------------------
// Execute: Delete Remote
// ---------------------------------------------------------------------------

func TestExecute_DeleteRemote(t *testing.T) {
	remoteFiles := map[string]string{
		"docs/stale.txt": "stale content",
	}
	srv := fakeServer(t, remoteFiles)
	defer srv.Close()

	localDir := t.TempDir()
	c := client.New(srv.URL, "s2_test")
	state := &State{Files: map[string]types.FileState{
		"stale.txt": {LocalHash: "xxx", RemoteETag: "yyy"},
	}}

	plans := []types.SyncPlan{
		{Path: "stale.txt", Action: types.DeleteRemote},
	}

	result, err := Execute(plans, localDir, "docs/", c, state, false)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result.Deleted != 1 {
		t.Errorf("expected 1 delete, got %d", result.Deleted)
	}
	if _, ok := remoteFiles["docs/stale.txt"]; ok {
		t.Error("expected remote file to be deleted")
	}
}

// ---------------------------------------------------------------------------
// Execute: Conflict (local wins)
// ---------------------------------------------------------------------------

func TestExecute_Conflict(t *testing.T) {
	remoteFiles := map[string]string{
		"docs/shared.txt": "remote version",
	}
	srv := fakeServer(t, remoteFiles)
	defer srv.Close()

	localDir := t.TempDir()
	writeLocalFile(t, localDir, "shared.txt", "local version")

	c := client.New(srv.URL, "s2_test")
	state := &State{Files: make(map[string]types.FileState)}

	plans := []types.SyncPlan{
		{Path: "shared.txt", Action: types.Conflict},
	}

	result, err := Execute(plans, localDir, "docs/", c, state, false)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result.Conflicts != 1 {
		t.Errorf("expected 1 conflict, got %d", result.Conflicts)
	}

	// Local file should still have local content
	content := readLocalFile(t, localDir, "shared.txt")
	if content != "local version" {
		t.Errorf("local should be unchanged, got %q", content)
	}

	// Remote should now have local version (local wins)
	if remoteFiles["docs/shared.txt"] != "local version" {
		t.Errorf("remote should have local version, got %q", remoteFiles["docs/shared.txt"])
	}

	// A conflict file should exist locally
	found := false
	entries, _ := os.ReadDir(localDir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".sync-conflict-") {
			found = true
			data, _ := os.ReadFile(filepath.Join(localDir, e.Name()))
			if string(data) != "remote version" {
				t.Errorf("conflict file should contain remote version, got %q", string(data))
			}
		}
	}
	if !found {
		t.Error("expected a .sync-conflict- file to be created")
	}
}

// ---------------------------------------------------------------------------
// Execute: Dry Run
// ---------------------------------------------------------------------------

func TestExecute_DryRun(t *testing.T) {
	remoteFiles := map[string]string{}
	srv := fakeServer(t, remoteFiles)
	defer srv.Close()

	localDir := t.TempDir()
	writeLocalFile(t, localDir, "new.txt", "new content")
	writeLocalFile(t, localDir, "delete-me.txt", "old")

	c := client.New(srv.URL, "s2_test")
	state := &State{Files: make(map[string]types.FileState)}

	plans := []types.SyncPlan{
		{Path: "new.txt", Action: types.Push},
		{Path: "pull.txt", Action: types.Pull},
		{Path: "delete-me.txt", Action: types.DeleteLocal},
		{Path: "remote-del.txt", Action: types.DeleteRemote},
		{Path: "conflict.txt", Action: types.Conflict},
	}

	result, err := Execute(plans, localDir, "docs/", c, state, true)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	// Counts should be incremented
	if result.Pushed != 1 || result.Pulled != 1 || result.Deleted != 2 || result.Conflicts != 1 {
		t.Errorf("unexpected counts: push=%d pull=%d delete=%d conflict=%d",
			result.Pushed, result.Pulled, result.Deleted, result.Conflicts)
	}

	// But no actual changes should have happened
	if len(remoteFiles) > 0 {
		t.Error("dry-run should not modify remote")
	}
	if !fileExists(localDir, "delete-me.txt") {
		t.Error("dry-run should not delete local files")
	}
}

// ---------------------------------------------------------------------------
// Execute: Many files (stress test)
// ---------------------------------------------------------------------------

func TestExecute_ManyFiles_Push(t *testing.T) {
	const fileCount = 100
	remoteFiles := map[string]string{}
	srv := fakeServer(t, remoteFiles)
	defer srv.Close()

	localDir := t.TempDir()
	var plans []types.SyncPlan
	for i := 0; i < fileCount; i++ {
		name := fmt.Sprintf("src/file_%04d.ts", i)
		writeLocalFile(t, localDir, name, fmt.Sprintf("content %d", i))
		plans = append(plans, types.SyncPlan{Path: name, Action: types.Push})
	}

	c := client.New(srv.URL, "s2_test")
	state := &State{Files: make(map[string]types.FileState)}

	result, err := Execute(plans, localDir, "project/", c, state, false)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result.Pushed != fileCount {
		t.Errorf("expected %d pushes, got %d", fileCount, result.Pushed)
	}
	if len(result.Errors) > 0 {
		t.Errorf("unexpected errors: %v", result.Errors)
	}
	if len(remoteFiles) != fileCount {
		t.Errorf("expected %d remote files, got %d", fileCount, len(remoteFiles))
	}
	if len(state.Files) != fileCount {
		t.Errorf("expected %d state entries, got %d", fileCount, len(state.Files))
	}
}

func TestExecute_ManyFiles_Pull(t *testing.T) {
	const fileCount = 100
	remoteFiles := make(map[string]string)
	var plans []types.SyncPlan
	for i := 0; i < fileCount; i++ {
		name := fmt.Sprintf("src/file_%04d.ts", i)
		remoteFiles["repo/"+name] = fmt.Sprintf("remote content %d", i)
		plans = append(plans, types.SyncPlan{Path: name, Action: types.Pull})
	}
	srv := fakeServer(t, remoteFiles)
	defer srv.Close()

	localDir := t.TempDir()
	c := client.New(srv.URL, "s2_test")
	state := &State{Files: make(map[string]types.FileState)}

	result, err := Execute(plans, localDir, "repo/", c, state, false)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result.Pulled != fileCount {
		t.Errorf("expected %d pulls, got %d", fileCount, result.Pulled)
	}
	// Verify a few files
	for _, i := range []int{0, 50, 99} {
		name := fmt.Sprintf("src/file_%04d.ts", i)
		content := readLocalFile(t, localDir, name)
		expected := fmt.Sprintf("remote content %d", i)
		if content != expected {
			t.Errorf("file %s: expected %q, got %q", name, expected, content)
		}
	}
}

// ---------------------------------------------------------------------------
// Execute: Mixed operations
// ---------------------------------------------------------------------------

func TestExecute_MixedOperations(t *testing.T) {
	remoteFiles := map[string]string{
		"docs/pull-me.txt":    "from remote",
		"docs/conflict-me.md": "remote conflict",
		"docs/delete-remote.txt": "will be deleted",
	}
	srv := fakeServer(t, remoteFiles)
	defer srv.Close()

	localDir := t.TempDir()
	writeLocalFile(t, localDir, "push-me.txt", "local new file")
	writeLocalFile(t, localDir, "conflict-me.md", "local conflict")
	writeLocalFile(t, localDir, "delete-local.txt", "will be removed")

	c := client.New(srv.URL, "s2_test")
	state := &State{Files: map[string]types.FileState{
		"delete-local.txt":   {LocalHash: "a", RemoteETag: "b"},
		"delete-remote.txt":  {LocalHash: "c", RemoteETag: "d"},
	}}

	plans := []types.SyncPlan{
		{Path: "push-me.txt", Action: types.Push},
		{Path: "pull-me.txt", Action: types.Pull},
		{Path: "conflict-me.md", Action: types.Conflict},
		{Path: "delete-local.txt", Action: types.DeleteLocal},
		{Path: "delete-remote.txt", Action: types.DeleteRemote},
	}

	result, err := Execute(plans, localDir, "docs/", c, state, false)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result.Pushed != 1 {
		t.Errorf("pushed: got %d", result.Pushed)
	}
	if result.Pulled != 1 {
		t.Errorf("pulled: got %d", result.Pulled)
	}
	if result.Conflicts != 1 {
		t.Errorf("conflicts: got %d", result.Conflicts)
	}
	if result.Deleted != 2 {
		t.Errorf("deleted: got %d", result.Deleted)
	}
	if len(result.Errors) > 0 {
		t.Errorf("unexpected errors: %v", result.Errors)
	}

	// Verify side effects
	if !fileExists(localDir, "pull-me.txt") {
		t.Error("pull-me.txt should exist locally")
	}
	if fileExists(localDir, "delete-local.txt") {
		t.Error("delete-local.txt should be deleted")
	}
	if _, ok := remoteFiles["docs/delete-remote.txt"]; ok {
		t.Error("delete-remote.txt should be deleted from remote")
	}
	if remoteFiles["docs/push-me.txt"] != "local new file" {
		t.Errorf("push-me.txt remote: got %q", remoteFiles["docs/push-me.txt"])
	}
}

// ---------------------------------------------------------------------------
// Execute: Push with optimistic locking (If-Match)
// ---------------------------------------------------------------------------

func TestExecute_Push_OptimisticLocking(t *testing.T) {
	remoteFiles := map[string]string{
		"docs/locked.txt": "original",
	}
	srv := fakeServer(t, remoteFiles)
	defer srv.Close()

	localDir := t.TempDir()
	writeLocalFile(t, localDir, "locked.txt", "updated local")

	c := client.New(srv.URL, "s2_test")
	// State has a previous etag that matches remote
	state := &State{Files: map[string]types.FileState{
		"locked.txt": {LocalHash: "old", RemoteETag: "hash_docs/locked.txt"},
	}}

	plans := []types.SyncPlan{
		{Path: "locked.txt", Action: types.Push},
	}

	result, err := Execute(plans, localDir, "docs/", c, state, false)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result.Pushed != 1 {
		t.Errorf("expected 1 push, got %d", result.Pushed)
	}
	if remoteFiles["docs/locked.txt"] != "updated local" {
		t.Errorf("remote should be updated, got %q", remoteFiles["docs/locked.txt"])
	}
}

func TestExecute_Push_OptimisticLocking_Rejected(t *testing.T) {
	remoteFiles := map[string]string{
		"docs/locked.txt": "someone else modified this",
	}
	srv := fakeServer(t, remoteFiles)
	defer srv.Close()

	localDir := t.TempDir()
	writeLocalFile(t, localDir, "locked.txt", "my update")

	c := client.New(srv.URL, "s2_test")
	// State has a stale etag that doesn't match remote
	state := &State{Files: map[string]types.FileState{
		"locked.txt": {LocalHash: "old", RemoteETag: "stale_etag"},
	}}

	plans := []types.SyncPlan{
		{Path: "locked.txt", Action: types.Push},
	}

	result, err := Execute(plans, localDir, "docs/", c, state, false)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	// Push should succeed (conflict handled gracefully)
	if result.Pushed != 1 {
		t.Errorf("expected 1 push (conflict handled), got %d", result.Pushed)
	}
	if len(result.Errors) > 0 {
		t.Errorf("unexpected errors: %v", result.Errors)
	}
}

// ---------------------------------------------------------------------------
// Edge case: file with no extension
// ---------------------------------------------------------------------------

func TestExecute_Push_FileNoExtension(t *testing.T) {
	remoteFiles := map[string]string{}
	srv := fakeServer(t, remoteFiles)
	defer srv.Close()

	localDir := t.TempDir()
	writeLocalFile(t, localDir, "Makefile", "all: build")
	writeLocalFile(t, localDir, "Dockerfile", "FROM alpine")
	writeLocalFile(t, localDir, ".gitignore", "node_modules/")

	c := client.New(srv.URL, "s2_test")
	state := &State{Files: make(map[string]types.FileState)}

	plans := []types.SyncPlan{
		{Path: "Makefile", Action: types.Push},
		{Path: "Dockerfile", Action: types.Push},
		{Path: ".gitignore", Action: types.Push},
	}

	result, err := Execute(plans, localDir, "repo/", c, state, false)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result.Pushed != 3 {
		t.Errorf("expected 3 pushes, got %d", result.Pushed)
	}
}

// ---------------------------------------------------------------------------
// Edge case: error continues processing remaining plans
// ---------------------------------------------------------------------------

func TestExecute_ErrorContinuesProcessing(t *testing.T) {
	remoteFiles := map[string]string{
		"docs/exists.txt": "exists",
	}
	srv := fakeServer(t, remoteFiles)
	defer srv.Close()

	localDir := t.TempDir()
	// Don't create "missing.txt" locally — push will fail
	writeLocalFile(t, localDir, "good.txt", "good content")

	c := client.New(srv.URL, "s2_test")
	state := &State{Files: make(map[string]types.FileState)}

	plans := []types.SyncPlan{
		{Path: "missing.txt", Action: types.Push},  // will fail (file doesn't exist)
		{Path: "good.txt", Action: types.Push},      // should still succeed
		{Path: "exists.txt", Action: types.Pull},    // should succeed
	}

	result, err := Execute(plans, localDir, "docs/", c, state, false)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Errorf("expected 1 error, got %d: %v", len(result.Errors), result.Errors)
	}
	if result.Pushed != 1 {
		t.Errorf("expected 1 successful push, got %d", result.Pushed)
	}
	if result.Pulled != 1 {
		t.Errorf("expected 1 pull, got %d", result.Pulled)
	}
}

// ---------------------------------------------------------------------------
// Stress: Server error partial failure
// ---------------------------------------------------------------------------

func TestExecute_ServerError_PartialFailure(t *testing.T) {
	// Use 422 (Unprocessable Entity) instead of 500 to avoid retryablehttp retries
	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/api/files/")

		switch r.Method {
		case "PUT":
			requestCount++
			if requestCount == 5 {
				w.WriteHeader(422)
				fmt.Fprint(w, `{"error":"Unprocessable Entity"}`)
				return
			}
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{
				"size": len(body),
				"hash": fmt.Sprintf("sha_%s", key),
				"etag": fmt.Sprintf("etag_%s", key),
			})
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	localDir := t.TempDir()
	var plans []types.SyncPlan
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("file_%02d.txt", i)
		writeLocalFile(t, localDir, name, fmt.Sprintf("content %d", i))
		plans = append(plans, types.SyncPlan{Path: name, Action: types.Push})
	}

	c := client.New(srv.URL, "s2_test")
	state := &State{Files: make(map[string]types.FileState)}

	result, err := Execute(plans, localDir, "docs/", c, state, false)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	// 5th request fails → 1 error, 9 successes
	if len(result.Errors) != 1 {
		t.Errorf("expected 1 error, got %d: %v", len(result.Errors), result.Errors)
	}
	if result.Pushed != 9 {
		t.Errorf("expected 9 successful pushes, got %d", result.Pushed)
	}
}

// ---------------------------------------------------------------------------
// Stress: 412 during multi-file push
// ---------------------------------------------------------------------------

func TestExecute_412_DuringMultiFilePush(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/api/files/")

		switch r.Method {
		case "PUT":
			// Return 412 for one specific file
			if strings.HasSuffix(key, "file_03.txt") {
				w.WriteHeader(412)
				fmt.Fprint(w, `{"error":"Precondition Failed"}`)
				return
			}
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{
				"size": len(body),
				"hash": fmt.Sprintf("sha_%s", key),
				"etag": fmt.Sprintf("etag_%s", key),
			})
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	localDir := t.TempDir()
	var plans []types.SyncPlan
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("file_%02d.txt", i)
		writeLocalFile(t, localDir, name, fmt.Sprintf("content %d", i))
		plans = append(plans, types.SyncPlan{Path: name, Action: types.Push})
	}

	c := client.New(srv.URL, "s2_test")
	state := &State{Files: map[string]types.FileState{
		"file_03.txt": {LocalHash: "old", RemoteETag: "stale"},
	}}

	result, err := Execute(plans, localDir, "docs/", c, state, false)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	// 412 is handled gracefully (prints "conflict (push rejected)") → counts as pushed
	if result.Pushed != 5 {
		t.Errorf("expected 5 pushes (412 handled gracefully), got %d", result.Pushed)
	}
	if len(result.Errors) > 0 {
		t.Errorf("expected no errors (412 is graceful), got: %v", result.Errors)
	}
}

// ---------------------------------------------------------------------------
// Stress: Unicode filenames full round-trip
// ---------------------------------------------------------------------------

func TestExecute_UnicodeFilenames_FullRoundTrip(t *testing.T) {
	remoteFiles := map[string]string{
		"proj/リモート.txt":     "remote japanese",
		"proj/spaced file.md": "remote spaced",
	}
	srv := fakeServer(t, remoteFiles)
	defer srv.Close()

	localDir := t.TempDir()
	writeLocalFile(t, localDir, "日本語ファイル.txt", "local japanese")
	writeLocalFile(t, localDir, "café.txt", "local french")

	c := client.New(srv.URL, "s2_test")
	state := &State{Files: make(map[string]types.FileState)}

	plans := []types.SyncPlan{
		{Path: "日本語ファイル.txt", Action: types.Push},
		{Path: "café.txt", Action: types.Push},
		{Path: "リモート.txt", Action: types.Pull},
		{Path: "spaced file.md", Action: types.Pull},
	}

	result, err := Execute(plans, localDir, "proj/", c, state, false)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result.Pushed != 2 {
		t.Errorf("expected 2 pushed, got %d", result.Pushed)
	}
	if result.Pulled != 2 {
		t.Errorf("expected 2 pulled, got %d", result.Pulled)
	}

	// Verify pulled files
	content := readLocalFile(t, localDir, "リモート.txt")
	if content != "remote japanese" {
		t.Errorf("japanese file: got %q", content)
	}
	content = readLocalFile(t, localDir, "spaced file.md")
	if content != "remote spaced" {
		t.Errorf("spaced file: got %q", content)
	}

	// Verify state entries
	for _, name := range []string{"日本語ファイル.txt", "café.txt", "リモート.txt", "spaced file.md"} {
		if _, ok := state.Files[name]; !ok {
			t.Errorf("expected state entry for %s", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Stress: 1000 mixed operations
// ---------------------------------------------------------------------------

func TestExecute_ManyFiles_MixedOperations_1000(t *testing.T) {
	remoteFiles := make(map[string]string)
	for i := 0; i < 200; i++ {
		remoteFiles[fmt.Sprintf("proj/pull_%04d.txt", i)] = fmt.Sprintf("pull content %d", i)
	}
	for i := 0; i < 50; i++ {
		remoteFiles[fmt.Sprintf("proj/conflict_%04d.txt", i)] = fmt.Sprintf("remote conflict %d", i)
	}
	for i := 0; i < 100; i++ {
		remoteFiles[fmt.Sprintf("proj/del_remote_%04d.txt", i)] = fmt.Sprintf("to delete %d", i)
	}

	srv := fakeServer(t, remoteFiles)
	defer srv.Close()

	localDir := t.TempDir()

	var plans []types.SyncPlan

	// 200 push
	for i := 0; i < 200; i++ {
		name := fmt.Sprintf("push_%04d.txt", i)
		writeLocalFile(t, localDir, name, fmt.Sprintf("push content %d", i))
		plans = append(plans, types.SyncPlan{Path: name, Action: types.Push})
	}

	// 200 pull
	for i := 0; i < 200; i++ {
		plans = append(plans, types.SyncPlan{Path: fmt.Sprintf("pull_%04d.txt", i), Action: types.Pull})
	}

	// 100 delete-local
	for i := 0; i < 100; i++ {
		name := fmt.Sprintf("del_local_%04d.txt", i)
		writeLocalFile(t, localDir, name, fmt.Sprintf("to delete local %d", i))
		plans = append(plans, types.SyncPlan{Path: name, Action: types.DeleteLocal})
	}

	// 100 delete-remote
	for i := 0; i < 100; i++ {
		plans = append(plans, types.SyncPlan{Path: fmt.Sprintf("del_remote_%04d.txt", i), Action: types.DeleteRemote})
	}

	// 50 conflict
	for i := 0; i < 50; i++ {
		name := fmt.Sprintf("conflict_%04d.txt", i)
		writeLocalFile(t, localDir, name, fmt.Sprintf("local conflict %d", i))
		plans = append(plans, types.SyncPlan{Path: name, Action: types.Conflict})
	}

	c := client.New(srv.URL, "s2_test")
	state := &State{Files: make(map[string]types.FileState)}

	// Pre-populate state for delete operations
	for i := 0; i < 100; i++ {
		state.Files[fmt.Sprintf("del_local_%04d.txt", i)] = types.FileState{LocalHash: "x", RemoteETag: "y"}
		state.Files[fmt.Sprintf("del_remote_%04d.txt", i)] = types.FileState{LocalHash: "x", RemoteETag: "y"}
	}

	result, err := Execute(plans, localDir, "proj/", c, state, false)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if result.Pushed != 200 {
		t.Errorf("expected 200 pushed, got %d", result.Pushed)
	}
	if result.Pulled != 200 {
		t.Errorf("expected 200 pulled, got %d", result.Pulled)
	}
	if result.Deleted != 200 {
		t.Errorf("expected 200 deleted, got %d", result.Deleted)
	}
	if result.Conflicts != 50 {
		t.Errorf("expected 50 conflicts, got %d", result.Conflicts)
	}
	if len(result.Errors) > 0 {
		t.Errorf("expected no errors, got %d: %v", len(result.Errors), result.Errors)
	}

	// Verify delete-local files are gone
	for i := 0; i < 100; i++ {
		if fileExists(localDir, fmt.Sprintf("del_local_%04d.txt", i)) {
			t.Errorf("del_local_%04d.txt should be deleted", i)
			break
		}
	}
}
