package sync

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/selfbase-dev/s2-cli/internal/types"
)

// State represents the .s2/state.json file.
type State struct {
	Version      int                        `json:"version"`
	RemotePrefix string                     `json:"remote_prefix"`
	SyncedAt     string                     `json:"synced_at"`
	Cursor       int64                      `json:"cursor,omitempty"`
	Files        map[string]types.FileState `json:"files"`
}

// StateDir returns the .s2 directory path within the sync root.
func StateDir(syncRoot string) string {
	return filepath.Join(syncRoot, ".s2")
}

// StatePath returns the state.json file path.
func StatePath(syncRoot string) string {
	return filepath.Join(StateDir(syncRoot), "state.json")
}

// LoadState reads state.json from the sync root.
// Returns an empty state if the file doesn't exist or is corrupt.
func LoadState(syncRoot string) (*State, error) {
	path := StatePath(syncRoot)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &State{
				Version: 1,
				Files:   make(map[string]types.FileState),
			}, nil
		}
		return nil, err
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		// Corrupt state: treat as first sync
		return &State{
			Version: 1,
			Files:   make(map[string]types.FileState),
		}, nil
	}

	if state.Files == nil {
		state.Files = make(map[string]types.FileState)
	}
	return &state, nil
}

// SaveState writes state.json atomically (write to tmp, then rename).
func SaveState(syncRoot string, state *State) error {
	dir := StateDir(syncRoot)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	state.SyncedAt = time.Now().UTC().Format(time.RFC3339)

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	tmpPath := StatePath(syncRoot) + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return err
	}

	return os.Rename(tmpPath, StatePath(syncRoot))
}
