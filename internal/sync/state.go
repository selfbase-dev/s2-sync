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
	Cursor       string                     `json:"cursor,omitempty"`
	TokenID      string                     `json:"token_id,omitempty"`
	PushedSeqs   []int64                    `json:"pushed_seqs,omitempty"`
	Files        map[string]types.FileState `json:"files"`
}

const currentStateVersion = 2

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
// If the state version is old (v1), resets to empty (cursor incompatible).
func LoadState(syncRoot string) (*State, error) {
	path := StatePath(syncRoot)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return newEmptyState(), nil
		}
		return nil, err
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		// Corrupt state: treat as first sync
		return newEmptyState(), nil
	}

	// v1 → v2 migration: cursor format changed (int64 → opaque string),
	// ETag changed (hash → content_version). Reset state.
	if state.Version < currentStateVersion {
		return newEmptyState(), nil
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

	state.Version = currentStateVersion
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

// AddPushedSeq records a seq from a push operation for self-change filtering.
func (s *State) AddPushedSeq(seq int64) {
	s.PushedSeqs = append(s.PushedSeqs, seq)
}

// IsPushedSeq returns true if the seq was recorded as a push from this instance.
func (s *State) IsPushedSeq(seq int64) bool {
	for _, ps := range s.PushedSeqs {
		if ps == seq {
			return true
		}
	}
	return false
}

// PrunePushedSeqs removes seqs that are older than the given cursor's minimum seq.
// Called after polling to clean up stale entries.
func (s *State) PrunePushedSeqs(minSeq int64) {
	kept := s.PushedSeqs[:0]
	for _, seq := range s.PushedSeqs {
		if seq >= minSeq {
			kept = append(kept, seq)
		}
	}
	s.PushedSeqs = kept
}

func newEmptyState() *State {
	return &State{
		Version: currentStateVersion,
		Files:   make(map[string]types.FileState),
	}
}
