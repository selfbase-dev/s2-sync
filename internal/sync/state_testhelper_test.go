package sync

import "github.com/selfbase-dev/s2-sync/internal/types"

// testStateFromArchive builds a minimal *State around an in-memory
// archive map for tests that exercise pure sync logic (Walk, Compare,
// dir_events, executor) without opening SQLite. State.Save on this
// instance returns an error because db is nil; tests that only examine
// plan output or in-memory mutation need not call Save.
func testStateFromArchive(archive map[string]types.FileState) *State {
	if archive == nil {
		archive = make(map[string]types.FileState)
	}
	return &State{
		Files:      archive,
		dirty:      make(map[string]struct{}),
		pushedSeqs: make(map[int64]struct{}),
	}
}
