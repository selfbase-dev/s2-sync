package sync

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/selfbase-dev/s2-sync/internal/auth"
	"github.com/selfbase-dev/s2-sync/internal/client"
	"github.com/selfbase-dev/s2-sync/internal/types"
)

// testServer creates a mock S2 API for executor tests.
func testServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *client.Client) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, client.New(srv.URL, auth.NewStaticSource("s2_test"))
}

// Push targets /api/v1/files/<plan.Path> directly — the server resolves
// paths relative to the token's base_path, which s2-sync never sees.
func TestExecute_Push_RemoteKey(t *testing.T) {
	cases := []struct {
		name     string
		planPath string
		wantURL  string
	}{
		{"top-level file", "README.md", "/api/v1/files/README.md"},
		{"nested file", "agents/MEMORY.md", "/api/v1/files/agents/MEMORY.md"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotURL string
			_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "PUT" {
					gotURL = r.URL.Path
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(201)
					json.NewEncoder(w).Encode(map[string]any{
						"id": "n1", "name": "f", "size": 1,
						"hash": "h", "content_version": int64(1),
					})
				}
			})

			localDir := t.TempDir()
			localPath := filepath.Join(localDir, tc.planPath)
			os.MkdirAll(filepath.Dir(localPath), 0755)
			os.WriteFile(localPath, []byte("x"), 0644)

			state := testStateFromArchive(nil)
			plans := []types.SyncPlan{{Path: tc.planPath, Action: types.Push}}
			Execute(plans, localDir, c, state, false)

			if gotURL != tc.wantURL {
				t.Errorf("URL.Path = %q, want %q", gotURL, tc.wantURL)
			}
		})
	}
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
				"hash": "abc", "content_version": int64(1), "seq": seq,
			})
		}
	})

	localDir := t.TempDir()
	os.WriteFile(filepath.Join(localDir, "file.txt"), []byte("hello"), 0644)

	state := testStateFromArchive(nil)
	plans := []types.SyncPlan{{Path: "file.txt", Action: types.Push}}

	result, err := Execute(plans, localDir, c, state, false)
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
				"hash": "def", "content_version": int64(2),
			})
		}
	})

	localDir := t.TempDir()
	os.WriteFile(filepath.Join(localDir, "file.txt"), []byte("updated"), 0644)

	state := testStateFromArchive(map[string]types.FileState{
		"file.txt": {ContentVersion: 1, LocalHash: "old"},
	})
	plans := []types.SyncPlan{{Path: "file.txt", Action: types.Push}}

	Execute(plans, localDir, c, state, false)

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
	state := testStateFromArchive(nil)
	plans := []types.SyncPlan{{Path: "doc.txt", Action: types.Pull}}

	result, err := Execute(plans, localDir, c, state, false)
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

	state := testStateFromArchive(map[string]types.FileState{
		"gone.txt": {LocalHash: "x"},
	})
	plans := []types.SyncPlan{{Path: "gone.txt", Action: types.DeleteLocal}}

	result, _ := Execute(plans, localDir, c, state, false)
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
	state := testStateFromArchive(map[string]types.FileState{
		"old.txt": {LocalHash: "x"},
	})
	plans := []types.SyncPlan{{Path: "old.txt", Action: types.DeleteRemote}}

	result, _ := Execute(plans, localDir, c, state, false)
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

func TestExecute_DeleteRemoteDir(t *testing.T) {
	var gotMethod, gotPath string
	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		if r.Method == "DELETE" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"seq": 99})
		}
	})

	localDir := t.TempDir()
	state := testStateFromArchive(map[string]types.FileState{
		"foo/a.txt":     {LocalHash: "h1"},
		"foo/b.txt":     {LocalHash: "h2"},
		"foo/bar/c.txt": {LocalHash: "h3"},
		"sibling.txt":   {LocalHash: "hX"},
	})
	plans := []types.SyncPlan{{Path: "foo", Action: types.DeleteRemoteDir}}

	result, _ := Execute(plans, localDir, c, state, false)
	if result.Deleted != 3 {
		t.Errorf("deleted = %d, want 3", result.Deleted)
	}
	if gotMethod != "DELETE" {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if gotPath != "/api/v1/files/foo" {
		t.Errorf("path = %q, want /api/v1/files/foo", gotPath)
	}
	for _, p := range []string{"foo/a.txt", "foo/b.txt", "foo/bar/c.txt"} {
		if _, ok := state.Files[p]; ok {
			t.Errorf("%s should be removed from archive", p)
		}
	}
	if _, ok := state.Files["sibling.txt"]; !ok {
		t.Error("sibling.txt must remain")
	}
	if !state.IsPushedSeq(99) {
		t.Error("seq 99 should be in pushed_seqs")
	}
}

func TestExecute_DeleteRemoteDir_DryRun(t *testing.T) {
	called := false
	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	localDir := t.TempDir()
	state := testStateFromArchive(map[string]types.FileState{
		"foo/a.txt": {LocalHash: "h1"},
	})
	plans := []types.SyncPlan{{Path: "foo", Action: types.DeleteRemoteDir}}

	result, _ := Execute(plans, localDir, c, state, true)
	if called {
		t.Error("dry-run must not call server")
	}
	if result.Deleted != 1 {
		t.Errorf("deleted = %d, want 1 (dry-run still counts archive entries)", result.Deleted)
	}
	if _, ok := state.Files["foo/a.txt"]; !ok {
		t.Error("dry-run must not mutate archive")
	}
}

func TestExecute_Move_Success(t *testing.T) {
	var gotURL, gotFrom, gotTo string
	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/api/v1/files-move" {
			gotURL = r.URL.Path
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			gotFrom, _ = body["from"].(string)
			gotTo, _ = body["to"].(string)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{
				"id":              "n1",
				"seq":             int64(77),
				"content_version": int64(5),
			})
		}
	})

	localDir := t.TempDir()
	state := testStateFromArchive(map[string]types.FileState{
		"file.txt": {LocalHash: "h1", ContentVersion: 4, RevisionID: "r1"},
	})
	plans := []types.SyncPlan{{
		Path:   "File.txt",
		From:   "file.txt",
		Action: types.Move,
		Hash:   "h1",
	}}

	result, _ := Execute(plans, localDir, c, state, false)
	if result.Moved != 1 {
		t.Errorf("moved = %d, want 1", result.Moved)
	}
	if gotURL != "/api/v1/files-move" {
		t.Errorf("URL = %q, want /api/v1/files-move", gotURL)
	}
	if gotFrom != "file.txt" {
		t.Errorf("from = %q, want file.txt", gotFrom)
	}
	if gotTo != "File.txt" {
		t.Errorf("to = %q, want File.txt", gotTo)
	}
	if _, ok := state.Files["file.txt"]; ok {
		t.Error("archive should no longer have old path file.txt")
	}
	newRow, ok := state.Files["File.txt"]
	if !ok {
		t.Fatalf("archive should have new path File.txt, got: %+v", state.Files)
	}
	if newRow.ContentVersion != 5 {
		t.Errorf("content_version = %d, want 5", newRow.ContentVersion)
	}
	if !state.IsPushedSeq(77) {
		t.Error("seq 77 should be in pushed_seqs")
	}
}

func TestExecute_Move_409_FallsBackToSkip(t *testing.T) {
	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{"error": "destination exists"})
		}
	})

	localDir := t.TempDir()
	state := testStateFromArchive(map[string]types.FileState{
		"file.txt": {LocalHash: "h1"},
	})
	plans := []types.SyncPlan{{
		Path:   "File.txt",
		From:   "file.txt",
		Action: types.Move,
		Hash:   "h1",
	}}

	result, _ := Execute(plans, localDir, c, state, false)
	if result.Moved != 0 {
		t.Errorf("moved = %d, want 0 on 409", result.Moved)
	}
	if result.Skipped != 1 {
		t.Errorf("skipped = %d, want 1 on 409", result.Skipped)
	}
	// Archive MUST retain original — no delete+push fallback
	if _, ok := state.Files["file.txt"]; !ok {
		t.Error("archive should keep original file.txt after 409 (no destructive fallback)")
	}
}

func TestExecute_MoveApply_PreservesInode(t *testing.T) {
	// pull-side file move must apply os.Rename, not
	// decompose into delete+download — essential for case-only
	// renames on case-insensitive FS.
	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("MoveApply must not hit the network, got %s %s", r.Method, r.URL.Path)
	})

	localDir := t.TempDir()
	oldPath := filepath.Join(localDir, "old.txt")
	if err := os.WriteFile(oldPath, []byte("content"), 0644); err != nil {
		t.Fatalf("write old: %v", err)
	}
	infoBefore, err := os.Stat(oldPath)
	if err != nil {
		t.Fatalf("stat old: %v", err)
	}

	state := testStateFromArchive(map[string]types.FileState{
		"old.txt": {LocalHash: "h", ContentVersion: 1, RevisionID: "r"},
	})
	plans := []types.SyncPlan{{
		Path:   "new.txt",
		From:   "old.txt",
		Action: types.MoveApply,
	}}

	result, _ := Execute(plans, localDir, c, state, false)
	if result.Moved != 1 {
		t.Errorf("moved = %d, want 1", result.Moved)
	}
	// New path exists, old does not
	if _, err := os.Stat(filepath.Join(localDir, "old.txt")); !os.IsNotExist(err) {
		t.Errorf("old.txt should be gone after move-apply, got: %v", err)
	}
	infoAfter, err := os.Stat(filepath.Join(localDir, "new.txt"))
	if err != nil {
		t.Fatalf("new.txt should exist: %v", err)
	}
	// Same inode — this is the critical property for case-only renames
	if !os.SameFile(infoBefore, infoAfter) {
		t.Error("os.Rename must preserve inode (expected SameFile)")
	}
	// Archive updated
	if _, ok := state.Files["old.txt"]; ok {
		t.Error("archive should drop old.txt")
	}
	if _, ok := state.Files["new.txt"]; !ok {
		t.Error("archive should add new.txt")
	}
}

func TestExecute_SkipCaseConflict_NoOp(t *testing.T) {
	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no HTTP call expected for SkipCaseConflict, got %s %s", r.Method, r.URL.Path)
	})

	localDir := t.TempDir()
	state := testStateFromArchive(map[string]types.FileState{
		"file.txt": {LocalHash: "h1"},
	})
	plans := []types.SyncPlan{{Path: "File.txt", Action: types.SkipCaseConflict}}

	result, _ := Execute(plans, localDir, c, state, false)
	if result.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", result.Skipped)
	}
	// Archive unchanged — terminal state
	if _, ok := state.Files["file.txt"]; !ok {
		t.Error("archive should be untouched by SkipCaseConflict")
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

	state := testStateFromArchive(nil)
	plans := []types.SyncPlan{{Path: "file.txt", Action: types.Conflict}}

	result, _ := Execute(plans, localDir, c, state, false)
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
				"hash": "abc", "content_version": int64(4), "seq": 99,
			})
		}
	})

	localDir := t.TempDir()
	os.WriteFile(filepath.Join(localDir, "file.txt"), []byte("local version"), 0644)

	state := testStateFromArchive(nil)
	plans := []types.SyncPlan{{Path: "file.txt", Action: types.Conflict}}

	result, _ := Execute(plans, localDir, c, state, false)
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

// Bug fix: remote 404 during conflict must push local, not silently return nil.
func TestExecute_Conflict_Remote404_PushesLocal(t *testing.T) {
	uploadCount := 0
	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(404)
			return
		}
		if r.Method == "PUT" {
			uploadCount++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{
				"id": "n1", "name": "file.txt", "size": 13,
				"hash": "abc", "content_version": int64(1), "seq": int64(77),
			})
		}
	})

	localDir := t.TempDir()
	os.WriteFile(filepath.Join(localDir, "file.txt"), []byte("local content"), 0644)

	state := testStateFromArchive(nil)
	plans := []types.SyncPlan{{Path: "file.txt", Action: types.Conflict}}

	result, err := Execute(plans, localDir, c, state, false)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Conflicts != 1 {
		t.Errorf("conflicts = %d, want 1", result.Conflicts)
	}
	if uploadCount != 1 {
		t.Errorf("upload count = %d, want 1 (local must be pushed to remote)", uploadCount)
	}
	fs, ok := state.Files["file.txt"]
	if !ok {
		t.Fatal("file.txt not in state after conflict push")
	}
	if fs.ContentVersion != 1 {
		t.Errorf("content_version = %d, want 1", fs.ContentVersion)
	}
	if !state.IsPushedSeq(77) {
		t.Error("seq 77 should be in pushed_seqs")
	}
}

// --- SELF-315: 404 fallback tests ---

func TestExecute_Pull_RevisionPruned_FallsBackToPath(t *testing.T) {
	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/api/v1/revisions/rev-pruned" {
			w.WriteHeader(404)
			return
		}
		if r.Method == "GET" {
			// path-based download succeeds
			w.Header().Set("ETag", `"8"`)
			w.Write([]byte("latest content"))
		}
	})

	localDir := t.TempDir()
	state := testStateFromArchive(nil)
	plans := []types.SyncPlan{{Path: "doc.txt", Action: types.Pull, RevisionID: "rev-pruned"}}

	result, err := Execute(plans, localDir, c, state, false)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Pulled != 1 {
		t.Errorf("pulled = %d, want 1", result.Pulled)
	}
	data, _ := os.ReadFile(filepath.Join(localDir, "doc.txt"))
	if string(data) != "latest content" {
		t.Errorf("content = %q, want %q", string(data), "latest content")
	}
	fs := state.Files["doc.txt"]
	if fs.ContentVersion != 8 {
		t.Errorf("content_version = %d, want 8", fs.ContentVersion)
	}
	// Fallback → RevisionID not recorded
	if fs.RevisionID != "" {
		t.Errorf("revision_id = %q, want empty (fallback)", fs.RevisionID)
	}
}

func TestExecute_Pull_RevisionPruned_FileAlsoDeleted(t *testing.T) {
	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(404) // both revision and path return 404
		}
	})

	localDir := t.TempDir()
	state := testStateFromArchive(nil)
	plans := []types.SyncPlan{{Path: "gone.txt", Action: types.Pull, RevisionID: "rev-gone"}}

	result, _ := Execute(plans, localDir, c, state, false)
	if len(result.Errors) == 0 {
		t.Error("expected error when both revision and path return 404")
	}
}

func TestExecute_Pull_RevisionPinned_RecordsRevisionID(t *testing.T) {
	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Header().Set("ETag", `"5"`)
			w.Write([]byte("pinned content"))
		}
	})

	localDir := t.TempDir()
	state := testStateFromArchive(nil)
	plans := []types.SyncPlan{{Path: "doc.txt", Action: types.Pull, RevisionID: "rev-abc"}}

	Execute(plans, localDir, c, state, false)

	fs := state.Files["doc.txt"]
	if fs.RevisionID != "rev-abc" {
		t.Errorf("revision_id = %q, want %q", fs.RevisionID, "rev-abc")
	}
}

func TestExecute_Conflict_RevisionPruned_FallsBack(t *testing.T) {
	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/api/v1/revisions/rev-old" {
			w.WriteHeader(404)
			return
		}
		if r.Method == "GET" {
			w.Header().Set("ETag", `"6"`)
			w.Write([]byte("remote version"))
		}
		if r.Method == "PUT" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{
				"id": "n1", "name": "file.txt", "size": 13,
				"hash": "abc", "content_version": int64(7),
			})
		}
	})

	localDir := t.TempDir()
	os.WriteFile(filepath.Join(localDir, "file.txt"), []byte("local version"), 0644)

	state := testStateFromArchive(nil)
	plans := []types.SyncPlan{{Path: "file.txt", Action: types.Conflict, RevisionID: "rev-old"}}

	result, err := Execute(plans, localDir, c, state, false)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Conflicts != 1 {
		t.Errorf("conflicts = %d, want 1", result.Conflicts)
	}
}

// --- SELF-315: idempotent apply tests ---

func TestExecute_Pull_IdempotentSkip_RevisionID(t *testing.T) {
	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called when idempotent skip fires")
	})

	localDir := t.TempDir()
	os.WriteFile(filepath.Join(localDir, "file.txt"), []byte("content"), 0644)

	state := testStateFromArchive(map[string]types.FileState{
		"file.txt": {LocalHash: "h1", ContentVersion: 5, RevisionID: "rev-same"},
	})
	plans := []types.SyncPlan{{Path: "file.txt", Action: types.Pull, RevisionID: "rev-same"}}

	result, err := Execute(plans, localDir, c, state, false)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Pulled != 0 {
		t.Errorf("pulled = %d, want 0 (should be skipped)", result.Pulled)
	}
}

func TestExecute_Pull_NoSkip_HashOnlyNotSufficient(t *testing.T) {
	// Hash match alone must NOT skip — it would leave ContentVersion stale
	// and bypass the local-change safety check in executePull.
	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Header().Set("ETag", `"4"`)
			w.Write([]byte("new content"))
		}
	})

	localDir := t.TempDir()
	// No pre-existing local file — archive entry with matching hash but
	// no RevisionID should still result in a download.
	state := testStateFromArchive(nil)
	plans := []types.SyncPlan{{Path: "file.txt", Action: types.Pull, Hash: "some-hash"}}

	result, err := Execute(plans, localDir, c, state, false)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Pulled != 1 {
		t.Errorf("pulled = %d, want 1 (hash-only must not skip)", result.Pulled)
	}
}

func TestExecute_Pull_NoSkip_DeleteRecreate(t *testing.T) {
	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Header().Set("ETag", `"1"`)
			w.Write([]byte("new file content"))
		}
	})

	localDir := t.TempDir()
	state := testStateFromArchive(map[string]types.FileState{
		"file.txt": {LocalHash: "old-hash", ContentVersion: 5, RevisionID: "rev-old-node"},
	})
	// Different RevisionID = different node (delete→recreate same path)
	plans := []types.SyncPlan{{Path: "file.txt", Action: types.Pull, RevisionID: "rev-new-node", Hash: "new-hash"}}

	result, _ := Execute(plans, localDir, c, state, false)
	if result.Pulled != 1 {
		t.Errorf("pulled = %d, want 1 (different revision must download)", result.Pulled)
	}
}

func TestExecute_Pull_NoSkip_NoArchive(t *testing.T) {
	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Header().Set("ETag", `"1"`)
			w.Write([]byte("content"))
		}
	})

	localDir := t.TempDir()
	state := testStateFromArchive(nil)
	plans := []types.SyncPlan{{Path: "new.txt", Action: types.Pull, RevisionID: "rev-1"}}

	result, _ := Execute(plans, localDir, c, state, false)
	if result.Pulled != 1 {
		t.Errorf("pulled = %d, want 1 (no archive entry must download)", result.Pulled)
	}
}

func TestExecute_DryRun(t *testing.T) {
	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called in dry-run mode")
	})

	localDir := t.TempDir()
	os.WriteFile(filepath.Join(localDir, "file.txt"), []byte("x"), 0644)

	state := testStateFromArchive(nil)
	plans := []types.SyncPlan{
		{Path: "file.txt", Action: types.Push},
		{Path: "other.txt", Action: types.Pull},
	}

	result, _ := Execute(plans, localDir, c, state, true)
	if result.Pushed != 1 || result.Pulled != 1 {
		t.Errorf("dry-run: pushed=%d pulled=%d, want 1/1", result.Pushed, result.Pulled)
	}
}
