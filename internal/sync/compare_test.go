package sync

import (
	"testing"

	"github.com/selfbase-dev/s2-cli/internal/types"
)

func TestCompare(t *testing.T) {
	hashA := "aaaa"
	hashB := "bbbb"
	hashC := "cccc"

	tests := []struct {
		name    string
		local   map[string]types.LocalFile
		remote  map[string]types.RemoteFile
		archive map[string]types.FileState
		want    []types.SyncPlan
	}{
		{
			name:    "no changes",
			local:   map[string]types.LocalFile{"f.txt": {Hash: hashA}},
			remote:  map[string]types.RemoteFile{"f.txt": {ETag: hashA}},
			archive: map[string]types.FileState{"f.txt": {LocalHash: hashA, RemoteETag: hashA}},
			want:    nil,
		},
		{
			name:    "local changed, remote unchanged → push",
			local:   map[string]types.LocalFile{"f.txt": {Hash: hashB}},
			remote:  map[string]types.RemoteFile{"f.txt": {ETag: hashA}},
			archive: map[string]types.FileState{"f.txt": {LocalHash: hashA, RemoteETag: hashA}},
			want:    []types.SyncPlan{{Path: "f.txt", Action: types.Push}},
		},
		{
			name:    "remote changed, local unchanged → pull",
			local:   map[string]types.LocalFile{"f.txt": {Hash: hashA}},
			remote:  map[string]types.RemoteFile{"f.txt": {ETag: hashB}},
			archive: map[string]types.FileState{"f.txt": {LocalHash: hashA, RemoteETag: hashA}},
			want:    []types.SyncPlan{{Path: "f.txt", Action: types.Pull}},
		},
		{
			name:    "both changed → conflict",
			local:   map[string]types.LocalFile{"f.txt": {Hash: hashB}},
			remote:  map[string]types.RemoteFile{"f.txt": {ETag: hashC}},
			archive: map[string]types.FileState{"f.txt": {LocalHash: hashA, RemoteETag: hashA}},
			want:    []types.SyncPlan{{Path: "f.txt", Action: types.Conflict}},
		},
		{
			name:    "new local file (no archive) → push",
			local:   map[string]types.LocalFile{"new.txt": {Hash: hashA}},
			remote:  map[string]types.RemoteFile{},
			archive: nil,
			want:    []types.SyncPlan{{Path: "new.txt", Action: types.Push}},
		},
		{
			name:    "new remote file (no archive) → pull",
			local:   map[string]types.LocalFile{},
			remote:  map[string]types.RemoteFile{"new.txt": {ETag: hashA}},
			archive: nil,
			want:    []types.SyncPlan{{Path: "new.txt", Action: types.Pull}},
		},
		{
			name:    "local deleted, remote unchanged → delete remote",
			local:   map[string]types.LocalFile{},
			remote:  map[string]types.RemoteFile{"f.txt": {ETag: hashA}},
			archive: map[string]types.FileState{"f.txt": {LocalHash: hashA, RemoteETag: hashA}},
			want:    []types.SyncPlan{{Path: "f.txt", Action: types.DeleteRemote}},
		},
		{
			name:    "remote deleted, local unchanged → delete local",
			local:   map[string]types.LocalFile{"f.txt": {Hash: hashA}},
			remote:  map[string]types.RemoteFile{},
			archive: map[string]types.FileState{"f.txt": {LocalHash: hashA, RemoteETag: hashA}},
			want:    []types.SyncPlan{{Path: "f.txt", Action: types.DeleteLocal}},
		},
		{
			name:    "local deleted, remote changed → conflict",
			local:   map[string]types.LocalFile{},
			remote:  map[string]types.RemoteFile{"f.txt": {ETag: hashB}},
			archive: map[string]types.FileState{"f.txt": {LocalHash: hashA, RemoteETag: hashA}},
			want:    []types.SyncPlan{{Path: "f.txt", Action: types.Conflict}},
		},
		{
			name:    "remote deleted, local changed → conflict",
			local:   map[string]types.LocalFile{"f.txt": {Hash: hashB}},
			remote:  map[string]types.RemoteFile{},
			archive: map[string]types.FileState{"f.txt": {LocalHash: hashA, RemoteETag: hashA}},
			want:    []types.SyncPlan{{Path: "f.txt", Action: types.Conflict}},
		},
		{
			name:    "both deleted → no-op",
			local:   map[string]types.LocalFile{},
			remote:  map[string]types.RemoteFile{},
			archive: map[string]types.FileState{"f.txt": {LocalHash: hashA, RemoteETag: hashA}},
			want:    nil,
		},
		{
			name:    "initial sync, same content → no-op",
			local:   map[string]types.LocalFile{"f.txt": {Hash: hashA}},
			remote:  map[string]types.RemoteFile{"f.txt": {ETag: hashA}},
			archive: nil,
			want:    nil,
		},
		{
			name:    "initial sync, different content → conflict (local wins)",
			local:   map[string]types.LocalFile{"f.txt": {Hash: hashA}},
			remote:  map[string]types.RemoteFile{"f.txt": {ETag: hashB}},
			archive: nil,
			want:    []types.SyncPlan{{Path: "f.txt", Action: types.Conflict}},
		},
		{
			name: "multiple files sorted",
			local: map[string]types.LocalFile{
				"b.txt": {Hash: hashB},
				"a.txt": {Hash: hashA},
			},
			remote:  map[string]types.RemoteFile{},
			archive: nil,
			want: []types.SyncPlan{
				{Path: "a.txt", Action: types.Push},
				{Path: "b.txt", Action: types.Push},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Compare(tt.local, tt.remote, tt.archive)

			if len(got) != len(tt.want) {
				t.Fatalf("got %d plans, want %d\ngot: %v", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i].Path != tt.want[i].Path || got[i].Action != tt.want[i].Action {
					t.Errorf("plan[%d]: got {%s, %s}, want {%s, %s}",
						i, got[i].Path, got[i].Action, tt.want[i].Path, tt.want[i].Action)
				}
			}
		})
	}
}
