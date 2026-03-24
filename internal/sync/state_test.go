package sync

import (
	"os"
	"testing"

	"github.com/selfbase-dev/s2-cli/internal/types"
)

func TestLoadStateMissing(t *testing.T) {
	dir := t.TempDir()
	state, err := LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState failed: %v", err)
	}
	if state.Version != 1 {
		t.Errorf("expected version 1, got %d", state.Version)
	}
	if len(state.Files) != 0 {
		t.Errorf("expected empty files, got %d", len(state.Files))
	}
}

func TestLoadStateCorrupt(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(StateDir(dir), 0700)
	os.WriteFile(StatePath(dir), []byte("not json"), 0600)

	state, err := LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState failed: %v", err)
	}
	if state.Version != 1 {
		t.Errorf("expected version 1 for corrupt state, got %d", state.Version)
	}
}

func TestSaveAndLoadState(t *testing.T) {
	dir := t.TempDir()
	state := &State{
		Version:      1,
		RemotePrefix: "docs/",
		Files: map[string]types.FileState{
			"readme.md": {
				LocalHash:  "abc123",
				RemoteETag: "abc123",
				Size:       100,
			},
		},
	}

	if err := SaveState(dir, state); err != nil {
		t.Fatalf("SaveState failed: %v", err)
	}

	loaded, err := LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState failed: %v", err)
	}

	if loaded.RemotePrefix != "docs/" {
		t.Errorf("expected prefix docs/, got %s", loaded.RemotePrefix)
	}
	if loaded.SyncedAt == "" {
		t.Error("expected SyncedAt to be set")
	}
	f, ok := loaded.Files["readme.md"]
	if !ok {
		t.Fatal("expected readme.md in files")
	}
	if f.LocalHash != "abc123" {
		t.Errorf("expected hash abc123, got %s", f.LocalHash)
	}
}
