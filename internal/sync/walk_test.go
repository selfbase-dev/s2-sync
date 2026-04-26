package sync

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0755)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writeFile(%q): %v", path, err)
	}
}

func TestWalk_BasicFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "hello")
	writeFile(t, filepath.Join(dir, "sub", "b.txt"), "world")

	res, err := Walk(dir, nil)
	if err != nil {
		t.Fatalf("Walk() error: %v", err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("got %d files, want 2", len(res.Files))
	}
	if _, ok := res.Files["a.txt"]; !ok {
		t.Error("missing a.txt")
	}
	if _, ok := res.Files["sub/b.txt"]; !ok {
		t.Error("missing sub/b.txt")
	}
	if len(res.Collisions) != 0 {
		t.Errorf("unexpected collisions: %v", res.Collisions)
	}
}

func TestWalk_SkipsS2Dir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "hello")
	writeFile(t, filepath.Join(dir, ".s2", "state.json"), "{}")

	res, err := Walk(dir, nil)
	if err != nil {
		t.Fatalf("Walk() error: %v", err)
	}
	if _, ok := res.Files[".s2/state.json"]; ok {
		t.Error(".s2/state.json should be skipped")
	}
	if len(res.Files) != 1 {
		t.Errorf("got %d files, want 1", len(res.Files))
	}
}

func TestWalk_ExcludeFunction(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "hello")
	writeFile(t, filepath.Join(dir, ".DS_Store"), "mac")
	writeFile(t, filepath.Join(dir, "._hidden"), "resource fork")
	writeFile(t, filepath.Join(dir, ".git", "config"), "git")

	exclude := DefaultExclude()
	res, err := Walk(dir, exclude)
	if err != nil {
		t.Fatalf("Walk() error: %v", err)
	}
	if _, ok := res.Files["._hidden"]; ok {
		t.Error("._hidden should be excluded (matches ._*)")
	}
	if _, ok := res.Files[".git/config"]; !ok {
		t.Error(".git/config should be included (not excluded by default)")
	}
	if _, ok := res.Files[".DS_Store"]; !ok {
		t.Error(".DS_Store should be included (only excluded via .s2ignore)")
	}
}

func TestWalk_HashConsistency(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "consistent content")

	res1, _ := Walk(dir, nil)
	res2, _ := Walk(dir, nil)

	if res1.Files["a.txt"].Hash != res2.Files["a.txt"].Hash {
		t.Error("hash should be consistent across walks")
	}
}

func TestWalk_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	res, err := Walk(dir, nil)
	if err != nil {
		t.Fatalf("Walk() error: %v", err)
	}
	if len(res.Files) != 0 {
		t.Errorf("got %d files, want 0", len(res.Files))
	}
}

func TestWalk_ForwardSlashPaths(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a", "b", "c.txt"), "deep")

	res, _ := Walk(dir, nil)
	if _, ok := res.Files["a/b/c.txt"]; !ok {
		t.Error("paths should use forward slashes")
	}
}

// local walk must NFC-normalize paths so macOS NFD and
// other-OS NFC converge to the same archive key.
func TestWalk_NFCNormalizes(t *testing.T) {
	dir := t.TempDir()
	// On macOS, this literal will land on disk as NFD; on Linux
	// it stays NFC. Either way, walk should return NFC-canonical.
	writeFile(t, filepath.Join(dir, "à.txt"), "hello")

	res, err := Walk(dir, nil)
	if err != nil {
		t.Fatalf("Walk() error: %v", err)
	}
	// NFC form of "à" is U+00E0 (single code point)
	nfc := "à.txt"
	if _, ok := res.Files[nfc]; !ok {
		keys := make([]string, 0, len(res.Files))
		for k := range res.Files {
			keys = append(keys, k)
		}
		t.Errorf("expected NFC key %q (U+00E0), got keys: %v", nfc, keys)
	}
}

// local collision (e.g. case-sensitive FS with File.txt +
// file.txt) must be detected and reported, not silently drop one.
func TestWalk_LocalCollision(t *testing.T) {
	dir := t.TempDir()
	// Skip this test on case-insensitive filesystems — these files
	// cannot coexist there so we can't set up the scenario.
	writeFile(t, filepath.Join(dir, "file.txt"), "lowercase")
	if err := os.WriteFile(filepath.Join(dir, "File.txt"), []byte("upper"), 0644); err != nil {
		t.Skipf("cannot create File.txt alongside file.txt (case-insensitive FS): %v", err)
	}
	// Same content → not a collision if they're on the same inode (Mac/Win).
	// On case-sensitive FS they're distinct.
	info1, _ := os.Stat(filepath.Join(dir, "file.txt"))
	info2, _ := os.Stat(filepath.Join(dir, "File.txt"))
	if info1 != nil && info2 != nil && os.SameFile(info1, info2) {
		t.Skip("filesystem is case-insensitive (same inode); skipping")
	}

	res, err := Walk(dir, nil)
	if err != nil {
		t.Fatalf("Walk() error: %v", err)
	}
	if len(res.Collisions) != 1 {
		t.Fatalf("expected 1 collision group, got %d: %+v", len(res.Collisions), res.Collisions)
	}
	group := res.Collisions[0]
	if len(group.Paths) != 2 {
		t.Errorf("expected 2 paths in group, got %d: %v", len(group.Paths), group.Paths)
	}
	// Lexicographic first wins — "File.txt" < "file.txt" in UTF-8 bytewise
	// (uppercase ASCII < lowercase ASCII).
	winner := group.Paths[0]
	if _, ok := res.Files[winner]; !ok {
		t.Errorf("winner %q should be in Files", winner)
	}
}

