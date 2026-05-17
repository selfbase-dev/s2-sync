package sync

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/selfbase-dev/s2-sync/internal/types"
)

// TestExecute_PreserveLocalRename_RenamesAndDropsArchive covers the
// PreserveLocalRename action path. Planner emits this when the local
// file has drifted from its archive hash and the remote announces a
// move/rename to a new path — the safe move is to keep the local edits
// as a .sync-conflict-* copy and clear the archive entry so the next
// sync can re-discover them as new.
func TestExecute_PreserveLocalRename_RenamesAndDropsArchive(t *testing.T) {
	localDir := t.TempDir()
	original := filepath.Join(localDir, "doc.txt")
	if err := os.WriteFile(original, []byte("local edits"), 0644); err != nil {
		t.Fatal(err)
	}

	state := testStateFromArchive(map[string]types.FileState{
		"doc.txt": {LocalHash: "stale-hash", ContentVersion: 4},
	})
	plans := []types.SyncPlan{{Path: "doc.txt", Action: types.PreserveLocalRename}}

	// No client calls expected — preserve is purely local.
	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected server call: %s %s", r.Method, r.URL.Path)
	})

	result, err := execute(plans, localDir, c, state, false, executeDeps{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.Conflicts != 1 {
		t.Errorf("conflicts = %d, want 1", result.Conflicts)
	}
	if _, ok := state.Files["doc.txt"]; ok {
		t.Error("archive entry should be removed for preserved file")
	}
	if _, err := os.Stat(original); !os.IsNotExist(err) {
		t.Errorf("original path should no longer exist (it was renamed): err=%v", err)
	}
	// .sync-conflict-* sibling should exist with the local content.
	entries, _ := os.ReadDir(localDir)
	found := false
	for _, e := range entries {
		if strings.Contains(e.Name(), ".sync-conflict-") {
			found = true
			data, _ := os.ReadFile(filepath.Join(localDir, e.Name()))
			if string(data) != "local edits" {
				t.Errorf("conflict copy content = %q, want \"local edits\"", string(data))
			}
		}
	}
	if !found {
		t.Errorf("no .sync-conflict-* file found in %v", entries)
	}
}

// TestExecute_PreserveLocalRename_AlreadyGone is the idempotent path:
// the file disappeared between plan and execute (user manually deleted
// it). Drop the archive entry without erroring.
func TestExecute_PreserveLocalRename_AlreadyGone(t *testing.T) {
	localDir := t.TempDir()
	state := testStateFromArchive(map[string]types.FileState{
		"gone.txt": {LocalHash: "h", ContentVersion: 1},
	})
	plans := []types.SyncPlan{{Path: "gone.txt", Action: types.PreserveLocalRename}}

	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected server call: %s %s", r.Method, r.URL.Path)
	})

	result, err := execute(plans, localDir, c, state, false, executeDeps{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Errorf("Errors = %v, want empty", result.Errors)
	}
	if _, ok := state.Files["gone.txt"]; ok {
		t.Error("archive entry should be cleared even when local is already gone")
	}
}

// TestExecute_PreserveLocalRename_DryRun touches neither disk nor
// archive — only the counter advances.
func TestExecute_PreserveLocalRename_DryRun(t *testing.T) {
	localDir := t.TempDir()
	original := filepath.Join(localDir, "doc.txt")
	if err := os.WriteFile(original, []byte("local"), 0644); err != nil {
		t.Fatal(err)
	}

	state := testStateFromArchive(map[string]types.FileState{
		"doc.txt": {LocalHash: "h", ContentVersion: 1},
	})
	plans := []types.SyncPlan{{Path: "doc.txt", Action: types.PreserveLocalRename}}

	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected server call in dry-run")
	})

	result, err := execute(plans, localDir, c, state, true, executeDeps{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.Conflicts != 1 {
		t.Errorf("conflicts = %d, want 1", result.Conflicts)
	}
	if _, ok := state.Files["doc.txt"]; !ok {
		t.Error("dry-run must not mutate archive")
	}
	if _, err := os.Stat(original); err != nil {
		t.Errorf("dry-run must not rename file: %v", err)
	}
}

// TestExecute_Conflict_LocalGoneBeforePush covers the 3-way merge
// edge case where the user deleted the local file between the planner
// and the executor: conflictPushLocal must observe ENOENT, drop the
// archive entry, and return cleanly (no error, no spurious upload).
func TestExecute_Conflict_LocalGoneBeforePush(t *testing.T) {
	var uploadHits int

	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			// Remote also doesn't exist → conflictPushLocal path
			// triggers, which immediately observes the missing
			// local file.
			w.WriteHeader(404)
			return
		}
		if r.Method == "PUT" {
			uploadHits++
		}
	})

	localDir := t.TempDir()
	// Deliberately no file written → local is "gone".
	state := testStateFromArchive(map[string]types.FileState{
		"ghost.txt": {LocalHash: "h", ContentVersion: 1},
	})
	plans := []types.SyncPlan{{Path: "ghost.txt", Action: types.Conflict}}

	result, err := execute(plans, localDir, c, state, false, executeDeps{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Errorf("Errors = %v, want empty (local-gone + remote-gone is benign)", result.Errors)
	}
	if uploadHits != 0 {
		t.Errorf("uploads = %d, want 0 (must not push a missing file)", uploadHits)
	}
	if _, ok := state.Files["ghost.txt"]; ok {
		t.Error("archive entry should be cleared when both sides are gone")
	}
}

// TestExecute_Conflict_RemoteIdenticalContentNoUpload guards a sneaky
// optimization: when remote and local hash match (race: both ends
// already converged), the executor must NOT push redundantly. Without
// this check, every sync would loop-amplify a converged-but-divergent
// state into bogus seq churn.
func TestExecute_Conflict_RemoteIdenticalContentNoUpload(t *testing.T) {
	var uploadHits int
	content := "shared content"
	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Header().Set("ETag", `"9"`)
			_, _ = w.Write([]byte(content))
			return
		}
		if r.Method == "PUT" {
			uploadHits++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "n1", "name": "f.txt", "size": 14,
				"hash": "h", "content_version": int64(10),
			})
		}
	})

	localDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(localDir, "f.txt"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	state := testStateFromArchive(nil)
	plans := []types.SyncPlan{{Path: "f.txt", Action: types.Conflict}}

	result, err := execute(plans, localDir, c, state, false, executeDeps{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if uploadHits != 0 {
		t.Errorf("uploads = %d, want 0 (identical content → no PUT)", uploadHits)
	}
	if result.Conflicts != 1 {
		t.Errorf("conflicts = %d, want 1", result.Conflicts)
	}
	row, ok := state.Files["f.txt"]
	if !ok {
		t.Fatal("archive entry should be recorded after identical-content reconciliation")
	}
	if row.ContentVersion != 9 {
		t.Errorf("content_version = %d, want 9 (taken from remote ETag)", row.ContentVersion)
	}
}

// TestExecute_PartialBatch_OneFailsOthersContinue is the resilience
// invariant for any multi-plan execute(): a single bad plan must not
// abort siblings, otherwise one corrupt remote entry could hold up
// the entire change feed indefinitely.
func TestExecute_PartialBatch_OneFailsOthersContinue(t *testing.T) {
	var pushCount, deleteCount int
	_, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			pushCount++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "n1", "name": "ok.txt", "size": 1,
				"hash": "h", "content_version": int64(1),
			})
			return
		}
		if r.Method == "DELETE" {
			deleteCount++
			w.WriteHeader(500)
			_, _ = w.Write([]byte("simulated server error"))
			return
		}
	})

	localDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(localDir, "ok.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	state := testStateFromArchive(map[string]types.FileState{
		"will_fail_to_delete.txt": {LocalHash: "h", ContentVersion: 1},
	})
	plans := []types.SyncPlan{
		{Path: "will_fail_to_delete.txt", Action: types.DeleteRemote},
		{Path: "ok.txt", Action: types.Push},
	}

	result, err := execute(plans, localDir, c, state, false, executeDeps{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if pushCount != 1 {
		t.Errorf("pushCount = %d, want 1 (push must run despite earlier delete failure)", pushCount)
	}
	if deleteCount < 1 {
		t.Errorf("deleteCount = %d, want >= 1", deleteCount)
	}
	if result.Pushed != 1 {
		t.Errorf("result.Pushed = %d, want 1", result.Pushed)
	}
	if len(result.Errors) != 1 {
		t.Errorf("Errors = %d, want 1 (the failed delete)", len(result.Errors))
	}
}
