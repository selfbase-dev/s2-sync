package sync

import (
	"testing"

	"github.com/selfbase-dev/s2-sync/internal/types"
)

func TestCompare_InitialSync(t *testing.T) {
	tests := []struct {
		name   string
		local  map[string]types.LocalFile
		remote map[string]types.RemoteFile
		want   map[string]types.SyncAction
	}{
		{
			name:   "local only → push",
			local:  map[string]types.LocalFile{"a.txt": {Hash: "h1", Size: 10}},
			remote: map[string]types.RemoteFile{},
			want:   map[string]types.SyncAction{"a.txt": types.Push},
		},
		{
			name:   "remote only → pull",
			local:  map[string]types.LocalFile{},
			remote: map[string]types.RemoteFile{"b.txt": {Size: 20}},
			want:   map[string]types.SyncAction{"b.txt": types.Pull},
		},
		{
			name:   "both exist no archive → conflict (need hash compare)",
			local:  map[string]types.LocalFile{"c.txt": {Hash: "h1", Size: 10}},
			remote: map[string]types.RemoteFile{"c.txt": {Size: 10}},
			want:   map[string]types.SyncAction{"c.txt": types.Conflict},
		},
		{
			name:   "empty both sides → no plans",
			local:  map[string]types.LocalFile{},
			remote: map[string]types.RemoteFile{},
			want:   map[string]types.SyncAction{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plans := Compare(tt.local, tt.remote, nil)
			got := planMap(plans)
			for path, wantAction := range tt.want {
				if gotAction, ok := got[path]; !ok {
					t.Errorf("missing plan for %q", path)
				} else if gotAction != wantAction {
					t.Errorf("plan[%q] = %v, want %v", path, gotAction, wantAction)
				}
			}
			if len(got) != len(tt.want) {
				t.Errorf("got %d plans, want %d: %v", len(got), len(tt.want), plans)
			}
		})
	}
}

func TestCompare_WithArchive(t *testing.T) {
	tests := []struct {
		name    string
		local   map[string]types.LocalFile
		remote  map[string]types.RemoteFile
		archive map[string]types.FileState
		want    map[string]types.SyncAction
	}{
		{
			name:    "nothing changed → no-op",
			local:   map[string]types.LocalFile{"a.txt": {Hash: "h1", Size: 10}},
			remote:  map[string]types.RemoteFile{"a.txt": {Size: 10}},
			archive: map[string]types.FileState{"a.txt": {LocalHash: "h1", ContentVersion: 1}},
			want:    map[string]types.SyncAction{},
		},
		{
			name:    "local changed → push",
			local:   map[string]types.LocalFile{"a.txt": {Hash: "h2", Size: 15}},
			remote:  map[string]types.RemoteFile{"a.txt": {Size: 10}},
			archive: map[string]types.FileState{"a.txt": {LocalHash: "h1", ContentVersion: 1}},
			want:    map[string]types.SyncAction{"a.txt": types.Push},
		},
		{
			name:    "local deleted, remote unchanged → delete remote",
			local:   map[string]types.LocalFile{},
			remote:  map[string]types.RemoteFile{"a.txt": {Size: 10}},
			archive: map[string]types.FileState{"a.txt": {LocalHash: "h1", ContentVersion: 1}},
			want:    map[string]types.SyncAction{"a.txt": types.DeleteRemote},
		},
		{
			name:    "remote deleted, local unchanged → delete local",
			local:   map[string]types.LocalFile{"a.txt": {Hash: "h1", Size: 10}},
			remote:  map[string]types.RemoteFile{},
			archive: map[string]types.FileState{"a.txt": {LocalHash: "h1", ContentVersion: 1}},
			want:    map[string]types.SyncAction{"a.txt": types.DeleteLocal},
		},
		{
			name:    "local changed + remote deleted → conflict",
			local:   map[string]types.LocalFile{"a.txt": {Hash: "h2", Size: 15}},
			remote:  map[string]types.RemoteFile{},
			archive: map[string]types.FileState{"a.txt": {LocalHash: "h1", ContentVersion: 1}},
			want:    map[string]types.SyncAction{"a.txt": types.Conflict},
		},
		{
			// Bug fix: both deleted must clean archive (DeleteLocal) so that
			// the next full sync doesn't try to DeleteRemote a file already gone.
			name:    "both deleted → clean archive (DeleteLocal, not no-op)",
			local:   map[string]types.LocalFile{},
			remote:  map[string]types.RemoteFile{},
			archive: map[string]types.FileState{"a.txt": {LocalHash: "h1", ContentVersion: 1}},
			want:    map[string]types.SyncAction{"a.txt": types.DeleteLocal},
		},
		{
			name: "multiple files mixed",
			local: map[string]types.LocalFile{
				"unchanged.txt": {Hash: "h1", Size: 10},
				"edited.txt":    {Hash: "h_new", Size: 20},
				"new.txt":       {Hash: "h3", Size: 30},
			},
			remote: map[string]types.RemoteFile{
				"unchanged.txt":  {Size: 10},
				"edited.txt":     {Size: 10},
				"remote_new.txt": {Size: 40},
			},
			archive: map[string]types.FileState{
				"unchanged.txt": {LocalHash: "h1", ContentVersion: 1},
				"edited.txt":    {LocalHash: "h_old", ContentVersion: 1},
				"deleted.txt":   {LocalHash: "h_del", ContentVersion: 1},
			},
			want: map[string]types.SyncAction{
				"edited.txt":     types.Push,
				"new.txt":        types.Push,
				"remote_new.txt": types.Pull,
				// Both sides deleted: clean archive to prevent DeleteRemote re-firing
				"deleted.txt": types.DeleteLocal,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plans := Compare(tt.local, tt.remote, tt.archive)
			got := planMap(plans)
			for path, wantAction := range tt.want {
				if gotAction, ok := got[path]; !ok {
					t.Errorf("missing plan for %q", path)
				} else if gotAction != wantAction {
					t.Errorf("plan[%q] = %v, want %v", path, gotAction, wantAction)
				}
			}
			if len(got) != len(tt.want) {
				t.Errorf("got %d plans, want %d: %v", len(got), len(tt.want), got)
			}
		})
	}
}

func TestCompareIncremental(t *testing.T) {
	tests := []struct {
		name    string
		local   map[string]types.LocalFile
		archive map[string]types.FileState
		changes []types.ChangeEntry
		want    map[string]types.SyncAction
	}{
		{
			name:    "remote put, local unchanged → pull",
			local:   map[string]types.LocalFile{"a.txt": {Hash: "h1"}},
			archive: map[string]types.FileState{"a.txt": {LocalHash: "h1"}},
			changes: []types.ChangeEntry{{Action: "put", PathAfter: "a.txt"}},
			want:    map[string]types.SyncAction{"a.txt": types.Pull},
		},
		{
			name:    "remote delete, local unchanged → delete local",
			local:   map[string]types.LocalFile{"a.txt": {Hash: "h1"}},
			archive: map[string]types.FileState{"a.txt": {LocalHash: "h1"}},
			changes: []types.ChangeEntry{{Action: "delete", PathBefore: "a.txt"}},
			want:    map[string]types.SyncAction{"a.txt": types.DeleteLocal},
		},
		{
			name:    "local changed, no remote changes → push",
			local:   map[string]types.LocalFile{"a.txt": {Hash: "h2"}},
			archive: map[string]types.FileState{"a.txt": {LocalHash: "h1"}},
			changes: []types.ChangeEntry{},
			want:    map[string]types.SyncAction{"a.txt": types.Push},
		},
		{
			name:    "local new, no remote changes → push",
			local:   map[string]types.LocalFile{"new.txt": {Hash: "h1"}},
			archive: map[string]types.FileState{},
			changes: []types.ChangeEntry{},
			want:    map[string]types.SyncAction{"new.txt": types.Push},
		},
		{
			name:    "local deleted, no remote changes → delete remote",
			local:   map[string]types.LocalFile{},
			archive: map[string]types.FileState{"a.txt": {LocalHash: "h1"}},
			changes: []types.ChangeEntry{},
			want:    map[string]types.SyncAction{"a.txt": types.DeleteRemote},
		},
		{
			name:    "both changed → conflict",
			local:   map[string]types.LocalFile{"a.txt": {Hash: "h2"}},
			archive: map[string]types.FileState{"a.txt": {LocalHash: "h1"}},
			changes: []types.ChangeEntry{{Action: "put", PathAfter: "a.txt"}},
			want:    map[string]types.SyncAction{"a.txt": types.Conflict},
		},
		{
			name:    "local deleted + remote changed → conflict",
			local:   map[string]types.LocalFile{},
			archive: map[string]types.FileState{"a.txt": {LocalHash: "h1"}},
			changes: []types.ChangeEntry{{Action: "put", PathAfter: "a.txt"}},
			want:    map[string]types.SyncAction{"a.txt": types.Conflict},
		},
		{
			name:    "local changed + remote deleted → conflict",
			local:   map[string]types.LocalFile{"a.txt": {Hash: "h2"}},
			archive: map[string]types.FileState{"a.txt": {LocalHash: "h1"}},
			changes: []types.ChangeEntry{{Action: "delete", PathBefore: "a.txt"}},
			want:    map[string]types.SyncAction{"a.txt": types.Conflict},
		},
		{
			// file moves are preserved as a single MoveApply
			// plan (executed via os.Rename). Decomposing to delete+put
			// would corrupt case-only renames on case-insensitive FS.
			name:    "remote move → move-apply at new (preserves inode)",
			local:   map[string]types.LocalFile{"old.txt": {Hash: "h1"}},
			archive: map[string]types.FileState{"old.txt": {LocalHash: "h1"}},
			changes: []types.ChangeEntry{{Action: "move", PathBefore: "old.txt", PathAfter: "new.txt"}},
			want: map[string]types.SyncAction{
				"new.txt": types.MoveApply,
			},
		},
		{
			name:    "no changes at all → empty",
			local:   map[string]types.LocalFile{"a.txt": {Hash: "h1"}},
			archive: map[string]types.FileState{"a.txt": {LocalHash: "h1"}},
			changes: []types.ChangeEntry{},
			want:    map[string]types.SyncAction{},
		},
		{
			name:    "local new + remote put (same path) → conflict",
			local:   map[string]types.LocalFile{"foo.txt": {Hash: "local_h"}},
			archive: map[string]types.FileState{},
			changes: []types.ChangeEntry{{Action: "put", PathAfter: "foo.txt"}},
			want:    map[string]types.SyncAction{"foo.txt": types.Conflict},
		},
		{
			name:    "mkdir event is ignored (directories not synced as files)",
			local:   map[string]types.LocalFile{},
			archive: map[string]types.FileState{},
			changes: []types.ChangeEntry{{Action: "put", PathAfter: "newdir", IsDir: true}},
			want:    map[string]types.SyncAction{},
		},
		// Bug fix: both deleted must emit DeleteLocal to clean archive entry;
		// previously NoOp left archive intact causing DeleteRemote on next sync.
		{
			name:    "both deleted → clean archive (DeleteLocal, not NoOp)",
			local:   map[string]types.LocalFile{},
			archive: map[string]types.FileState{"gone.txt": {LocalHash: "h1"}},
			changes: []types.ChangeEntry{{Action: "delete", PathBefore: "gone.txt"}},
			want:    map[string]types.SyncAction{"gone.txt": types.DeleteLocal},
		},
		// Server paths may include leading "/" — normalization must strip it
		// so paths match local/archive keys (which never have leading "/").
		{
			name:    "leading slash on remote put path is normalized",
			local:   map[string]types.LocalFile{"a.txt": {Hash: "h1"}},
			archive: map[string]types.FileState{"a.txt": {LocalHash: "h1"}},
			changes: []types.ChangeEntry{{Action: "put", PathAfter: "/a.txt"}},
			want:    map[string]types.SyncAction{"a.txt": types.Pull},
		},
		{
			name:    "leading slash on remote delete path is normalized",
			local:   map[string]types.LocalFile{"a.txt": {Hash: "h2"}},
			archive: map[string]types.FileState{"a.txt": {LocalHash: "h1"}},
			changes: []types.ChangeEntry{{Action: "delete", PathBefore: "/a.txt"}},
			want:    map[string]types.SyncAction{"a.txt": types.Conflict},
		},
		{
			name:    "leading slash on remote move paths is normalized",
			local:   map[string]types.LocalFile{"old.txt": {Hash: "h1"}},
			archive: map[string]types.FileState{"old.txt": {LocalHash: "h1"}},
			changes: []types.ChangeEntry{{Action: "move", PathBefore: "/old.txt", PathAfter: "/new.txt"}},
			want: map[string]types.SyncAction{
				"new.txt": types.MoveApply,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plans := CompareIncremental(tt.local, tt.archive, tt.changes)
			got := planMap(plans)
			for path, wantAction := range tt.want {
				if gotAction, ok := got[path]; !ok {
					t.Errorf("missing plan for %q", path)
				} else if gotAction != wantAction {
					t.Errorf("plan[%q] = %v, want %v", path, gotAction, wantAction)
				}
			}
			if len(got) != len(tt.want) {
				t.Errorf("got %d plans, want %d: %v", len(got), len(tt.want), got)
			}
		})
	}
}

func TestCompare_PlansSortedByPath(t *testing.T) {
	local := map[string]types.LocalFile{
		"z.txt": {Hash: "h1"}, "a.txt": {Hash: "h2"}, "m.txt": {Hash: "h3"},
	}
	plans := Compare(local, nil, nil)
	for i := 1; i < len(plans); i++ {
		if plans[i].Path < plans[i-1].Path {
			t.Errorf("plans not sorted: %v before %v", plans[i-1].Path, plans[i].Path)
		}
	}
}

func TestCompare_ThreadsHashToSyncPlan(t *testing.T) {
	remote := map[string]types.RemoteFile{
		"a.txt": {Size: 10, RevisionID: "rev1", Hash: "h1", ContentVersion: 7},
	}
	plans := Compare(nil, remote, nil)
	if len(plans) != 1 {
		t.Fatalf("got %d plans, want 1", len(plans))
	}
	p := plans[0]
	if p.Hash != "h1" {
		t.Errorf("Hash = %q, want %q", p.Hash, "h1")
	}
	if p.RevisionID != "rev1" {
		t.Errorf("RevisionID = %q, want %q", p.RevisionID, "rev1")
	}
}

func TestCompareIncremental_ThreadsHashToSyncPlan(t *testing.T) {
	cv := int64(3)
	changes := []types.ChangeEntry{
		{Seq: 1, Action: "put", PathAfter: "/a.txt", RevisionID: "rev2", Hash: "h2", ContentVersion: &cv},
	}
	plans := CompareIncremental(nil, nil, changes)
	if len(plans) != 1 {
		t.Fatalf("got %d plans, want 1", len(plans))
	}
	p := plans[0]
	if p.Hash != "h2" {
		t.Errorf("Hash = %q, want %q", p.Hash, "h2")
	}
	if p.RevisionID != "rev2" {
		t.Errorf("RevisionID = %q, want %q", p.RevisionID, "rev2")
	}
}

// planMap converts a slice of SyncPlan to a map for easier assertion.
func planMap(plans []types.SyncPlan) map[string]types.SyncAction {
	m := make(map[string]types.SyncAction)
	for _, p := range plans {
		m[p.Path] = p.Action
	}
	return m
}

func TestHasLocalChanges(t *testing.T) {
	tests := []struct {
		name    string
		local   map[string]types.LocalFile
		archive map[string]types.FileState
		want    bool
	}{
		{
			name:    "identical",
			local:   map[string]types.LocalFile{"a.txt": {Hash: "h1"}},
			archive: map[string]types.FileState{"a.txt": {LocalHash: "h1"}},
			want:    false,
		},
		{
			name:    "both empty",
			local:   map[string]types.LocalFile{},
			archive: map[string]types.FileState{},
			want:    false,
		},
		{
			name:    "local modified",
			local:   map[string]types.LocalFile{"a.txt": {Hash: "h2"}},
			archive: map[string]types.FileState{"a.txt": {LocalHash: "h1"}},
			want:    true,
		},
		{
			name:    "local new file",
			local:   map[string]types.LocalFile{"a.txt": {Hash: "h1"}, "b.txt": {Hash: "h2"}},
			archive: map[string]types.FileState{"a.txt": {LocalHash: "h1"}},
			want:    true,
		},
		{
			name:    "local deleted",
			local:   map[string]types.LocalFile{},
			archive: map[string]types.FileState{"a.txt": {LocalHash: "h1"}},
			want:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HasLocalChanges(tt.local, tt.archive)
			if got != tt.want {
				t.Errorf("HasLocalChanges() = %v, want %v", got, tt.want)
			}
		})
	}
}
