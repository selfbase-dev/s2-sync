package sync

import (
	"slices"
	"testing"

	"github.com/selfbase-dev/s2-sync/internal/types"
)

func archiveOf(paths ...string) map[string]types.FileState {
	m := make(map[string]types.FileState, len(paths))
	for _, p := range paths {
		m[p] = types.FileState{LocalHash: "h-" + p}
	}
	return m
}

func localOf(paths ...string) map[string]types.LocalFile {
	m := make(map[string]types.LocalFile, len(paths))
	for _, p := range paths {
		m[p] = types.LocalFile{Hash: "h-" + p}
	}
	return m
}

func deletePlans(paths ...string) []types.SyncPlan {
	plans := make([]types.SyncPlan, 0, len(paths))
	for _, p := range paths {
		plans = append(plans, types.SyncPlan{Path: p, Action: types.DeleteRemote})
	}
	return plans
}

func planSummary(plans []types.SyncPlan) []string {
	out := make([]string, 0, len(plans))
	for _, p := range plans {
		out = append(out, p.Action.String()+":"+p.Path)
	}
	return out
}

func TestMergeFolderDeletes_Empty(t *testing.T) {
	out := MergeFolderDeletes(nil, nil, nil)
	if len(out) != 0 {
		t.Errorf("expected empty, got %v", planSummary(out))
	}
}

func TestMergeFolderDeletes_NoDeletes(t *testing.T) {
	plans := []types.SyncPlan{
		{Path: "a.txt", Action: types.Push},
		{Path: "b.txt", Action: types.Pull},
	}
	out := MergeFolderDeletes(plans, localOf("a.txt"), archiveOf("a.txt", "b.txt"))
	want := []string{"push:a.txt", "pull:b.txt"}
	if !slices.Equal(planSummary(out), want) {
		t.Errorf("got %v, want %v", planSummary(out), want)
	}
}

func TestMergeFolderDeletes_FullFolderDelete(t *testing.T) {
	plans := deletePlans("foo/a.txt", "foo/b.txt", "foo/c.txt")
	archive := archiveOf("foo/a.txt", "foo/b.txt", "foo/c.txt")
	local := localOf()

	out := MergeFolderDeletes(plans, local, archive)
	want := []string{"delete-remote-dir:foo"}
	if !slices.Equal(planSummary(out), want) {
		t.Errorf("got %v, want %v", planSummary(out), want)
	}
}

func TestMergeFolderDeletes_PartialDeleteNoMerge(t *testing.T) {
	plans := deletePlans("foo/a.txt", "foo/b.txt")
	archive := archiveOf("foo/a.txt", "foo/b.txt", "foo/c.txt")
	local := localOf("foo/c.txt")

	out := MergeFolderDeletes(plans, local, archive)
	want := []string{"delete-remote:foo/a.txt", "delete-remote:foo/b.txt"}
	if !slices.Equal(planSummary(out), want) {
		t.Errorf("got %v, want %v", planSummary(out), want)
	}
}

func TestMergeFolderDeletes_LocalRemainBlocksMerge(t *testing.T) {
	// All archive entries under foo/ are being deleted, but a local file
	// remains under foo/ (e.g., user-created not-yet-pushed file). Must
	// NOT cascade-delete because that would wipe the live local sibling
	// after push reaches the server.
	plans := deletePlans("foo/a.txt", "foo/b.txt")
	archive := archiveOf("foo/a.txt", "foo/b.txt")
	local := localOf("foo/new.txt")

	out := MergeFolderDeletes(plans, local, archive)
	want := []string{"delete-remote:foo/a.txt", "delete-remote:foo/b.txt"}
	if !slices.Equal(planSummary(out), want) {
		t.Errorf("got %v, want %v", planSummary(out), want)
	}
}

func TestMergeFolderDeletes_NestedShallowestWins(t *testing.T) {
	plans := deletePlans("foo/a.txt", "foo/bar/b.txt", "foo/bar/baz/c.txt")
	archive := archiveOf("foo/a.txt", "foo/bar/b.txt", "foo/bar/baz/c.txt")
	local := localOf()

	out := MergeFolderDeletes(plans, local, archive)
	want := []string{"delete-remote-dir:foo"}
	if !slices.Equal(planSummary(out), want) {
		t.Errorf("got %v, want %v", planSummary(out), want)
	}
}

func TestMergeFolderDeletes_SiblingDirs(t *testing.T) {
	plans := deletePlans("foo/a.txt", "bar/b.txt")
	archive := archiveOf("foo/a.txt", "bar/b.txt")
	local := localOf()

	out := MergeFolderDeletes(plans, local, archive)
	want := []string{"delete-remote-dir:bar", "delete-remote-dir:foo"}
	if !slices.Equal(planSummary(out), want) {
		t.Errorf("got %v, want %v", planSummary(out), want)
	}
}

func TestMergeFolderDeletes_PreservesNonDeletePlans(t *testing.T) {
	plans := []types.SyncPlan{
		{Path: "foo/a.txt", Action: types.DeleteRemote},
		{Path: "foo/b.txt", Action: types.DeleteRemote},
		{Path: "outside.txt", Action: types.Push},
	}
	archive := archiveOf("foo/a.txt", "foo/b.txt")
	local := localOf("outside.txt")

	out := MergeFolderDeletes(plans, local, archive)
	want := []string{"delete-remote-dir:foo", "push:outside.txt"}
	if !slices.Equal(planSummary(out), want) {
		t.Errorf("got %v, want %v", planSummary(out), want)
	}
}

func TestMergeFolderDeletes_PrefixCollisionWithSiblingFile(t *testing.T) {
	// Archive has foo.txt (sibling file, same prefix as foo/) — must not
	// be confused for a foo/ entry. Only foo/* gets cascaded.
	plans := deletePlans("foo/a.txt", "foo/b.txt")
	archive := archiveOf("foo/a.txt", "foo/b.txt", "foo.txt")
	local := localOf("foo.txt")

	out := MergeFolderDeletes(plans, local, archive)
	want := []string{"delete-remote-dir:foo"}
	if !slices.Equal(planSummary(out), want) {
		t.Errorf("got %v, want %v", planSummary(out), want)
	}
}

func TestMergeFolderDeletes_RootLevelFileIsNotDir(t *testing.T) {
	// A root-level file deletion has no parent dir prefix. Don't try to
	// emit a DeleteRemoteDir for "/" / scope-root.
	plans := deletePlans("a.txt")
	archive := archiveOf("a.txt")
	local := localOf()

	out := MergeFolderDeletes(plans, local, archive)
	want := []string{"delete-remote:a.txt"}
	if !slices.Equal(planSummary(out), want) {
		t.Errorf("got %v, want %v", planSummary(out), want)
	}
}
