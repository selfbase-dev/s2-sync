package sync

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/selfbase-dev/s2-cli/internal/types"
)

func TestLoadState_NoFile(t *testing.T) {
	dir := t.TempDir()
	state, err := LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState() error: %v", err)
	}
	if state.Version != currentStateVersion {
		t.Errorf("Version = %d, want %d", state.Version, currentStateVersion)
	}
	if state.Cursor != "" {
		t.Errorf("Cursor = %q, want empty", state.Cursor)
	}
	if state.Files == nil {
		t.Error("Files should be initialized")
	}
}

func TestLoadState_CorruptJSON(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".s2"), 0700)
	os.WriteFile(filepath.Join(dir, ".s2", "state.json"), []byte("{bad"), 0600)

	state, err := LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState() error: %v", err)
	}
	if state.Version != currentStateVersion {
		t.Errorf("Version = %d", state.Version)
	}
	if len(state.Files) != 0 {
		t.Errorf("Files len = %d", len(state.Files))
	}
}

func TestLoadState_OldVersionMigration(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".s2"), 0700)
	v1 := map[string]any{"version": 1, "cursor": 42, "files": map[string]any{
		"a.txt": map[string]any{"local_hash": "abc", "remote_etag": "def", "size": 100},
	}}
	data, _ := json.Marshal(v1)
	os.WriteFile(filepath.Join(dir, ".s2", "state.json"), data, 0600)

	state, err := LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState() error: %v", err)
	}
	if state.Cursor != "" {
		t.Errorf("Cursor should be empty after migration, got %q", state.Cursor)
	}
	if len(state.Files) != 0 {
		t.Errorf("Files should be empty after migration, got %d", len(state.Files))
	}
}

func TestSaveState_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	state := &State{
		Cursor: "opaque_cursor",
		TokenID:      "tok_123",
		PushedSeqs:   []int64{10, 20},
		Files: map[string]types.FileState{
			"readme.md": {LocalHash: "sha256hash", ContentVersion: 42, Size: 1024},
		},
	}

	if err := SaveState(dir, state); err != nil {
		t.Fatalf("SaveState() error: %v", err)
	}

	loaded, err := LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState() error: %v", err)
	}
	if loaded.Cursor != "opaque_cursor" {
		t.Errorf("Cursor = %q", loaded.Cursor)
	}
	if loaded.TokenID != "tok_123" {
		t.Errorf("TokenID = %q", loaded.TokenID)
	}
	if len(loaded.PushedSeqs) != 2 {
		t.Errorf("PushedSeqs = %v", loaded.PushedSeqs)
	}
	if fs := loaded.Files["readme.md"]; fs.ContentVersion != 42 {
		t.Errorf("ContentVersion = %d", fs.ContentVersion)
	}
	// SyncedAt should be set automatically
	if loaded.SyncedAt == "" {
		t.Error("SyncedAt should be set")
	}
}

func TestSaveState_NoTmpFileRemains(t *testing.T) {
	dir := t.TempDir()
	SaveState(dir, &State{Files: map[string]types.FileState{}})
	if _, err := os.Stat(StatePath(dir) + ".tmp"); !os.IsNotExist(err) {
		t.Error("tmp file should not remain")
	}
}

func TestPushedSeqs_AddAndCheck(t *testing.T) {
	s := newEmptyState()
	s.AddPushedSeq(10)
	s.AddPushedSeq(20)

	if !s.IsPushedSeq(10) {
		t.Error("10 should be pushed")
	}
	if s.IsPushedSeq(15) {
		t.Error("15 should not be pushed")
	}
}

func TestPushedSeqs_Prune(t *testing.T) {
	s := newEmptyState()
	s.AddPushedSeq(10)
	s.AddPushedSeq(20)
	s.AddPushedSeq(30)

	s.PrunePushedSeqs(20)

	if s.IsPushedSeq(10) {
		t.Error("10 should be pruned")
	}
	if !s.IsPushedSeq(20) {
		t.Error("20 should remain")
	}
	if !s.IsPushedSeq(30) {
		t.Error("30 should remain")
	}
}
