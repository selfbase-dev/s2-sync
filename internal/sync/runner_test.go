package sync

import (
	"testing"

	"github.com/selfbase-dev/s2-sync/internal/types"
)

func TestCountPlanDeletes_FilesAndDir(t *testing.T) {
	archive := map[string]types.FileState{
		"foo/a.txt":     {LocalHash: "h1"},
		"foo/b.txt":     {LocalHash: "h2"},
		"foo/bar/c.txt": {LocalHash: "h3"},
		"other.txt":     {LocalHash: "h4"},
	}
	plans := []types.SyncPlan{
		{Path: "foo", Action: types.DeleteRemoteDir},
		{Path: "other.txt", Action: types.DeleteRemote},
		{Path: "lonely.txt", Action: types.Push},
	}

	got := countPlanDeletes(plans, archive)
	if got != 4 {
		t.Errorf("countPlanDeletes = %d, want 4 (3 from foo/ + 1 file)", got)
	}
}

func TestCountPlanDeletes_DirWithNoArchiveEntries(t *testing.T) {
	// Defensive: a DeleteRemoteDir whose prefix doesn't match any
	// archive entry should count zero, not crash.
	archive := map[string]types.FileState{
		"keep.txt": {LocalHash: "h"},
	}
	plans := []types.SyncPlan{
		{Path: "ghost", Action: types.DeleteRemoteDir},
	}
	got := countPlanDeletes(plans, archive)
	if got != 0 {
		t.Errorf("countPlanDeletes = %d, want 0", got)
	}
}
