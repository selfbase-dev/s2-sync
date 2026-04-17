package sync

import (
	"testing"

	"github.com/selfbase-dev/s2-sync/internal/types"
)

func TestApplyChanges_FilePut(t *testing.T) {
	remote := map[string]types.RemoteFile{}
	size := int64(100)
	cv := int64(1)
	changes := []types.ChangeEntry{
		{Action: "put", PathAfter: "/hello.txt", Hash: "abc", RevisionID: "R1", Size: &size, ContentVersion: &cv},
	}
	queue := applyChanges(changes, remote)
	if len(queue) != 0 {
		t.Errorf("expected no dirty queue, got %d", len(queue))
	}
	if f, ok := remote["hello.txt"]; !ok || f.Hash != "abc" {
		t.Errorf("expected hello.txt with hash abc, got %+v", remote)
	}
	if remote["hello.txt"].Name != "hello.txt" {
		t.Errorf("expected name hello.txt, got %s", remote["hello.txt"].Name)
	}
	if remote["hello.txt"].Size != 100 {
		t.Errorf("expected size 100, got %d", remote["hello.txt"].Size)
	}
	if remote["hello.txt"].ContentVersion != 1 {
		t.Errorf("expected content version 1, got %d", remote["hello.txt"].ContentVersion)
	}
}

func TestApplyChanges_FilePutNested(t *testing.T) {
	remote := map[string]types.RemoteFile{}
	size := int64(50)
	cv := int64(2)
	changes := []types.ChangeEntry{
		{Action: "put", PathAfter: "/docs/notes/a.md", Hash: "xyz", RevisionID: "R2", Size: &size, ContentVersion: &cv},
	}
	applyChanges(changes, remote)
	f, ok := remote["docs/notes/a.md"]
	if !ok {
		t.Fatal("expected docs/notes/a.md")
	}
	if f.Name != "a.md" {
		t.Errorf("expected name a.md, got %s", f.Name)
	}
	if f.Hash != "xyz" {
		t.Errorf("expected hash xyz, got %s", f.Hash)
	}
}

func TestApplyChanges_FileDelete(t *testing.T) {
	remote := map[string]types.RemoteFile{
		"old.txt": {Hash: "abc"},
	}
	changes := []types.ChangeEntry{
		{Action: "delete", PathBefore: "/old.txt"},
	}
	applyChanges(changes, remote)
	if _, ok := remote["old.txt"]; ok {
		t.Error("expected old.txt to be deleted")
	}
}

func TestApplyChanges_FileMove(t *testing.T) {
	remote := map[string]types.RemoteFile{
		"a.jpg": {Hash: "abc"},
	}
	cv := int64(1)
	changes := []types.ChangeEntry{
		{Action: "move", PathBefore: "/a.jpg", PathAfter: "/photos/a.jpg", Hash: "abc", RevisionID: "R1", ContentVersion: &cv},
	}
	applyChanges(changes, remote)
	if _, ok := remote["a.jpg"]; ok {
		t.Error("expected a.jpg to be removed")
	}
	if f, ok := remote["photos/a.jpg"]; !ok || f.Hash != "abc" {
		t.Errorf("expected photos/a.jpg with hash abc, got %+v", remote)
	}
	if remote["photos/a.jpg"].Name != "a.jpg" {
		t.Errorf("expected name a.jpg, got %s", remote["photos/a.jpg"].Name)
	}
}

func TestApplyChanges_DirPut(t *testing.T) {
	remote := map[string]types.RemoteFile{}
	changes := []types.ChangeEntry{
		{Action: "put", PathAfter: "/restored", IsDir: true},
	}
	queue := applyChanges(changes, remote)
	if len(queue) != 1 || queue[0] != "restored" {
		t.Errorf("expected dirty queue [restored], got %v", queue)
	}
}

func TestApplyChanges_DirDelete(t *testing.T) {
	remote := map[string]types.RemoteFile{
		"trash/a.txt": {Hash: "abc"},
		"trash/b.txt": {Hash: "def"},
		"keep.txt":    {Hash: "ghi"},
	}
	changes := []types.ChangeEntry{
		{Action: "delete", PathBefore: "/trash", IsDir: true},
	}
	applyChanges(changes, remote)
	if _, ok := remote["trash/a.txt"]; ok {
		t.Error("expected trash/a.txt to be deleted")
	}
	if _, ok := remote["trash/b.txt"]; ok {
		t.Error("expected trash/b.txt to be deleted")
	}
	if _, ok := remote["keep.txt"]; !ok {
		t.Error("expected keep.txt to remain")
	}
}

func TestApplyChanges_DirDeleteRoot(t *testing.T) {
	remote := map[string]types.RemoteFile{
		"a.txt": {Hash: "abc"},
		"b.txt": {Hash: "def"},
	}
	changes := []types.ChangeEntry{
		{Action: "delete", PathBefore: "/", IsDir: true},
	}
	applyChanges(changes, remote)
	if len(remote) != 0 {
		t.Errorf("expected empty map after root delete, got %d entries", len(remote))
	}
}

func TestApplyChanges_DirMove(t *testing.T) {
	remote := map[string]types.RemoteFile{
		"old/a.txt":     {Hash: "abc"},
		"old/sub/b.txt": {Hash: "def"},
		"other.txt":     {Hash: "ghi"},
	}
	changes := []types.ChangeEntry{
		{Action: "move", PathBefore: "/old", PathAfter: "/new", IsDir: true},
	}
	applyChanges(changes, remote)
	if _, ok := remote["old/a.txt"]; ok {
		t.Error("expected old/a.txt to be removed")
	}
	if f, ok := remote["new/a.txt"]; !ok || f.Hash != "abc" {
		t.Errorf("expected new/a.txt with hash abc")
	}
	if f, ok := remote["new/sub/b.txt"]; !ok || f.Hash != "def" {
		t.Errorf("expected new/sub/b.txt with hash def")
	}
	if _, ok := remote["other.txt"]; !ok {
		t.Error("expected other.txt to remain")
	}
}

func TestApplyChanges_DirMkdir(t *testing.T) {
	remote := map[string]types.RemoteFile{}
	changes := []types.ChangeEntry{
		{Action: "mkdir", PathAfter: "/newdir", IsDir: true},
	}
	queue := applyChanges(changes, remote)
	if len(queue) != 0 {
		t.Errorf("expected no dirty queue for mkdir, got %v", queue)
	}
	if len(remote) != 0 {
		t.Errorf("expected no files added for mkdir, got %d", len(remote))
	}
}

func TestApplyChanges_UnsafePath(t *testing.T) {
	remote := map[string]types.RemoteFile{}
	size := int64(10)
	changes := []types.ChangeEntry{
		{Action: "put", PathAfter: "/../etc/passwd", Size: &size},
	}
	applyChanges(changes, remote)
	if len(remote) != 0 {
		t.Errorf("expected unsafe path to be skipped, got %d entries", len(remote))
	}
}

func TestApplyChanges_MultipleChanges(t *testing.T) {
	remote := map[string]types.RemoteFile{
		"existing.txt": {Hash: "old"},
	}
	size := int64(10)
	cv := int64(3)
	changes := []types.ChangeEntry{
		{Action: "put", PathAfter: "/new.txt", Hash: "new-h", RevisionID: "R1", Size: &size, ContentVersion: &cv},
		{Action: "delete", PathBefore: "/existing.txt"},
	}
	applyChanges(changes, remote)
	if _, ok := remote["existing.txt"]; ok {
		t.Error("expected existing.txt to be deleted")
	}
	if _, ok := remote["new.txt"]; !ok {
		t.Error("expected new.txt to be added")
	}
}

func TestCoalesceDirDelete(t *testing.T) {
	queue := []string{"restored", "restored/sub", "other"}
	result := coalesceDirDelete(queue, "restored")
	if len(result) != 1 || result[0] != "other" {
		t.Errorf("expected [other], got %v", result)
	}
}

func TestCoalesceDirDelete_NoMatch(t *testing.T) {
	queue := []string{"foo", "bar"}
	result := coalesceDirDelete(queue, "baz")
	if len(result) != 2 {
		t.Errorf("expected 2 entries, got %d", len(result))
	}
}

func TestCoalesceDirDelete_EmptyQueue(t *testing.T) {
	var queue []string
	result := coalesceDirDelete(queue, "foo")
	if len(result) != 0 {
		t.Errorf("expected empty, got %v", result)
	}
}

func TestCoalesceDirMove(t *testing.T) {
	queue := []string{"old", "old/sub", "other"}
	result := coalesceDirMove(queue, "old", "new")
	expected := []string{"new", "new/sub", "other"}
	if len(result) != len(expected) {
		t.Fatalf("expected %d entries, got %d", len(expected), len(result))
	}
	for i, v := range result {
		if v != expected[i] {
			t.Errorf("index %d: expected %s, got %s", i, expected[i], v)
		}
	}
}

func TestCoalesceDirMove_NoMatch(t *testing.T) {
	queue := []string{"foo", "bar"}
	result := coalesceDirMove(queue, "baz", "new")
	if result[0] != "foo" || result[1] != "bar" {
		t.Errorf("expected unchanged queue, got %v", result)
	}
}

func TestPathOrRoot(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", "/"},
		{"/", "/"},
		{"/docs", "/docs"},
		{"docs", "docs"},
	}
	for _, tt := range tests {
		got := pathOrRoot(tt.input)
		if got != tt.want {
			t.Errorf("pathOrRoot(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
