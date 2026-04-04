package sync

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/selfbase-dev/s2-cli/internal/client"
	"github.com/selfbase-dev/s2-cli/internal/types"
)

// testServer creates a mock S2 API for executor tests.
func testServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *client.Client) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, client.New(srv.URL, "s2_test")
}

func TestExecute_Push(t *testing.T) {
	var uploadedBody string
	var gotIfNoneMatch string
	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			gotIfNoneMatch = r.Header.Get("If-None-Match")
			b, _ := io.ReadAll(r.Body)
			uploadedBody = string(b)
			seq := int64(10)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{
				"id": "n1", "name": "file.txt", "size": 5,
				"hash": "abc", "etag": `"1"`, "seq": seq,
			})
		}
	})

	localDir := t.TempDir()
	os.WriteFile(filepath.Join(localDir, "file.txt"), []byte("hello"), 0644)

	state := &State{Files: map[string]types.FileState{}}
	plans := []types.SyncPlan{{Path: "file.txt", Action: types.Push}}

	result, err := Execute(plans, localDir, "prefix/", c, state, false)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Pushed != 1 {
		t.Errorf("pushed = %d, want 1", result.Pushed)
	}
	if uploadedBody != "hello" {
		t.Errorf("uploaded body = %q", uploadedBody)
	}
	// New file → If-None-Match: *
	if gotIfNoneMatch != "*" {
		t.Errorf("If-None-Match = %q, want *", gotIfNoneMatch)
	}
	// State updated
	fs, ok := state.Files["file.txt"]
	if !ok {
		t.Fatal("file.txt not in state after push")
	}
	if fs.ContentVersion != 1 {
		t.Errorf("content_version = %d, want 1", fs.ContentVersion)
	}
	// Seq recorded for self-change filter
	if !state.IsPushedSeq(10) {
		t.Error("seq 10 should be in pushed_seqs")
	}
}

func TestExecute_Push_CAS_Update(t *testing.T) {
	var gotIfMatch string
	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			gotIfMatch = r.Header.Get("If-Match")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{
				"id": "n1", "name": "file.txt", "size": 7,
				"hash": "def", "etag": `"2"`,
			})
		}
	})

	localDir := t.TempDir()
	os.WriteFile(filepath.Join(localDir, "file.txt"), []byte("updated"), 0644)

	state := &State{Files: map[string]types.FileState{
		"file.txt": {ContentVersion: 1, LocalHash: "old"},
	}}
	plans := []types.SyncPlan{{Path: "file.txt", Action: types.Push}}

	Execute(plans, localDir, "prefix/", c, state, false)

	// Existing file → If-Match: "1"
	if gotIfMatch != `"1"` {
		t.Errorf("If-Match = %q, want %q", gotIfMatch, `"1"`)
	}
	if state.Files["file.txt"].ContentVersion != 2 {
		t.Errorf("content_version = %d, want 2", state.Files["file.txt"].ContentVersion)
	}
}

func TestExecute_Pull(t *testing.T) {
	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Header().Set("ETag", `"5"`)
			w.Header().Set("Content-Length", "12")
			w.Write([]byte("remote stuff"))
		}
	})

	localDir := t.TempDir()
	state := &State{Files: map[string]types.FileState{}}
	plans := []types.SyncPlan{{Path: "doc.txt", Action: types.Pull}}

	result, err := Execute(plans, localDir, "prefix/", c, state, false)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Pulled != 1 {
		t.Errorf("pulled = %d, want 1", result.Pulled)
	}

	// File written locally
	data, err := os.ReadFile(filepath.Join(localDir, "doc.txt"))
	if err != nil {
		t.Fatalf("read local: %v", err)
	}
	if string(data) != "remote stuff" {
		t.Errorf("local content = %q", string(data))
	}

	// State updated
	fs := state.Files["doc.txt"]
	if fs.ContentVersion != 5 {
		t.Errorf("content_version = %d, want 5", fs.ContentVersion)
	}
	if fs.LocalHash == "" {
		t.Error("local_hash should be set")
	}
}

func TestExecute_DeleteLocal(t *testing.T) {
	localDir := t.TempDir()
	fpath := filepath.Join(localDir, "gone.txt")
	os.WriteFile(fpath, []byte("bye"), 0644)

	// No server needed for local delete
	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {})

	state := &State{Files: map[string]types.FileState{
		"gone.txt": {LocalHash: "x"},
	}}
	plans := []types.SyncPlan{{Path: "gone.txt", Action: types.DeleteLocal}}

	result, _ := Execute(plans, localDir, "prefix/", c, state, false)
	if result.Deleted != 1 {
		t.Errorf("deleted = %d, want 1", result.Deleted)
	}
	if _, err := os.Stat(fpath); !os.IsNotExist(err) {
		t.Error("file should be deleted")
	}
	if _, ok := state.Files["gone.txt"]; ok {
		t.Error("gone.txt should be removed from state")
	}
}

func TestExecute_DeleteRemote(t *testing.T) {
	var gotMethod string
	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		if r.Method == "DELETE" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"seq": 42})
		}
	})

	localDir := t.TempDir()
	state := &State{Files: map[string]types.FileState{
		"old.txt": {LocalHash: "x"},
	}}
	plans := []types.SyncPlan{{Path: "old.txt", Action: types.DeleteRemote}}

	result, _ := Execute(plans, localDir, "prefix/", c, state, false)
	if result.Deleted != 1 {
		t.Errorf("deleted = %d, want 1", result.Deleted)
	}
	if gotMethod != "DELETE" {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if _, ok := state.Files["old.txt"]; ok {
		t.Error("old.txt should be removed from state")
	}
	// Seq recorded
	if !state.IsPushedSeq(42) {
		t.Error("seq 42 should be in pushed_seqs")
	}
}

func TestExecute_Conflict_IdenticalContent(t *testing.T) {
	content := "same content"
	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Header().Set("ETag", `"3"`)
			w.Write([]byte(content))
		}
	})

	localDir := t.TempDir()
	os.WriteFile(filepath.Join(localDir, "file.txt"), []byte(content), 0644)

	state := &State{Files: map[string]types.FileState{}}
	plans := []types.SyncPlan{{Path: "file.txt", Action: types.Conflict}}

	result, _ := Execute(plans, localDir, "prefix/", c, state, false)
	if result.Conflicts != 1 {
		t.Errorf("conflicts = %d, want 1", result.Conflicts)
	}

	// No conflict file created (identical)
	entries, _ := os.ReadDir(localDir)
	for _, e := range entries {
		if e.Name() != "file.txt" {
			t.Errorf("unexpected file: %s (no conflict file for identical content)", e.Name())
		}
	}
	// State recorded
	if state.Files["file.txt"].ContentVersion != 3 {
		t.Errorf("content_version = %d, want 3", state.Files["file.txt"].ContentVersion)
	}
}

func TestExecute_Conflict_DifferentContent(t *testing.T) {
	uploadCount := 0
	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Header().Set("ETag", `"3"`)
			w.Write([]byte("remote version"))
		}
		if r.Method == "PUT" {
			uploadCount++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{
				"id": "n1", "name": "file.txt", "size": 13,
				"hash": "abc", "etag": `"4"`, "seq": 99,
			})
		}
	})

	localDir := t.TempDir()
	os.WriteFile(filepath.Join(localDir, "file.txt"), []byte("local version"), 0644)

	state := &State{Files: map[string]types.FileState{}}
	plans := []types.SyncPlan{{Path: "file.txt", Action: types.Conflict}}

	result, _ := Execute(plans, localDir, "prefix/", c, state, false)
	if result.Conflicts != 1 {
		t.Errorf("conflicts = %d, want 1", result.Conflicts)
	}

	// Conflict file created
	hasConflict := false
	entries, _ := os.ReadDir(localDir)
	for _, e := range entries {
		if e.Name() != "file.txt" {
			hasConflict = true
			// Remote content saved in conflict file
			data, _ := os.ReadFile(filepath.Join(localDir, e.Name()))
			if string(data) != "remote version" {
				t.Errorf("conflict file content = %q", string(data))
			}
		}
	}
	if !hasConflict {
		t.Error("expected .sync-conflict-* file")
	}

	// Local wins → pushed to remote
	if uploadCount != 1 {
		t.Errorf("upload count = %d, want 1 (local wins)", uploadCount)
	}

	// Seq recorded from conflict overwrite
	if !state.IsPushedSeq(99) {
		t.Error("seq 99 should be in pushed_seqs")
	}
}

func TestExecute_DryRun(t *testing.T) {
	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called in dry-run mode")
	})

	localDir := t.TempDir()
	os.WriteFile(filepath.Join(localDir, "file.txt"), []byte("x"), 0644)

	state := &State{Files: map[string]types.FileState{}}
	plans := []types.SyncPlan{
		{Path: "file.txt", Action: types.Push},
		{Path: "other.txt", Action: types.Pull},
	}

	result, _ := Execute(plans, localDir, "prefix/", c, state, true)
	if result.Pushed != 1 || result.Pulled != 1 {
		t.Errorf("dry-run: pushed=%d pulled=%d, want 1/1", result.Pushed, result.Pulled)
	}
}
