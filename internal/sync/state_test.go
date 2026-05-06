package sync

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

var testIdentity = Identity{
	Endpoint: "https://s2.example",
	UserID:   "user_test",
	TokenID:  "tok_test",
}

func TestLoadState_NoDB(t *testing.T) {
	dir := t.TempDir()
	state, err := LoadState(dir, testIdentity)
	if err != nil {
		t.Fatalf("LoadState() error: %v", err)
	}
	defer state.Close()

	if state.Cursor != "" {
		t.Errorf("Cursor = %q, want empty", state.Cursor)
	}
	if state.Files == nil {
		t.Error("Files should be initialized")
	}
	if len(state.Files) != 0 {
		t.Errorf("Files len = %d, want 0", len(state.Files))
	}
}

func TestLoadState_CorruptDB(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".s2"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DBPath(dir), []byte("not a sqlite file"), 0600); err != nil {
		t.Fatal(err)
	}

	state, err := LoadState(dir, testIdentity)
	if err != nil {
		t.Fatalf("LoadState() error: %v", err)
	}
	defer state.Close()

	if len(state.Files) != 0 {
		t.Errorf("Files len = %d after corrupt load, want 0", len(state.Files))
	}

	// Old DB should have been quarantined.
	entries, _ := filepath.Glob(DBPath(dir) + ".corrupt.*")
	if len(entries) == 0 {
		t.Error("expected quarantined .corrupt directory to exist")
	}
}

func TestSaveAndReload_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	state, err := LoadState(dir, testIdentity)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}

	state.Cursor = "opaque_cursor"
	state.RecordFile("readme.md", "sha256hash", 42, "rev_1")
	state.AddPushedSeq(10)
	state.AddPushedSeq(20)

	if err := state.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := state.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	loaded, err := LoadState(dir, testIdentity)
	if err != nil {
		t.Fatalf("LoadState (reload): %v", err)
	}
	defer loaded.Close()

	if loaded.Cursor != "opaque_cursor" {
		t.Errorf("Cursor = %q", loaded.Cursor)
	}
	if fs, ok := loaded.Files["readme.md"]; !ok || fs.ContentVersion != 42 || fs.RevisionID != "rev_1" {
		t.Errorf("Files[readme.md] = %+v ok=%v", fs, ok)
	}
	if !loaded.IsPushedSeq(10) || !loaded.IsPushedSeq(20) {
		t.Error("pushed seqs were not persisted")
	}
}

func TestLoadState_IdentityMismatchResets(t *testing.T) {
	dir := t.TempDir()
	state, err := LoadState(dir, testIdentity)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	state.Cursor = "stale-cursor"
	state.RecordFile("keep-me.txt", "h", 1, "")
	if err := state.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	state.Close()

	// Reopen with a different identity → archive must be dropped.
	otherIdentity := Identity{Endpoint: "https://other.example", UserID: "user_other", TokenID: "tok_other"}
	reloaded, err := LoadState(dir, otherIdentity)
	if err != nil {
		t.Fatalf("LoadState (new identity): %v", err)
	}
	defer reloaded.Close()

	if reloaded.Cursor != "" {
		t.Errorf("Cursor = %q after identity mismatch, want empty", reloaded.Cursor)
	}
	if len(reloaded.Files) != 0 {
		t.Errorf("Files len = %d after identity mismatch, want 0", len(reloaded.Files))
	}
	if reloaded.IsPushedSeq(0) {
		t.Error("pushed seqs should be cleared on identity mismatch")
	}
}

// Same user with a different token = different scope. The archive must
// reset, otherwise the previous token's cursor / files / pushed_seqs
// leak into the new scope and cause spurious deletes / pulls.
func TestLoadState_TokenSwitchResets(t *testing.T) {
	dir := t.TempDir()
	state, err := LoadState(dir, testIdentity)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	state.Cursor = "scope-A-cursor"
	state.RecordFile("a.txt", "h", 1, "")
	if err := state.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	state.Close()

	// Same endpoint + user, different token → archive must be dropped.
	swapped := testIdentity
	swapped.TokenID = "tok_other"
	reloaded, err := LoadState(dir, swapped)
	if err != nil {
		t.Fatalf("LoadState (token switch): %v", err)
	}
	defer reloaded.Close()

	if reloaded.Cursor != "" {
		t.Errorf("Cursor = %q after token switch, want empty", reloaded.Cursor)
	}
	if len(reloaded.Files) != 0 {
		t.Errorf("Files len = %d after token switch, want 0", len(reloaded.Files))
	}
}

// Opening a v2 (or any non-current) state.db must quarantine the old
// file and start fresh. Verifies the schema-mismatch path that this PR
// relies on for v2 → v3 migration.
func TestLoadState_SchemaVersionMismatch_Quarantines(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".s2"), 0700); err != nil {
		t.Fatal(err)
	}

	// Hand-build a "v2" state.db: schemaSQL minus token_id, with
	// PRAGMA user_version = 2.
	dbPath := DBPath(dir)
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	v2SQL := `
		CREATE TABLE state_meta (
		    id INTEGER PRIMARY KEY CHECK (id = 1),
		    cursor TEXT NOT NULL DEFAULT '',
		    endpoint TEXT NOT NULL DEFAULT '',
		    user_id TEXT NOT NULL DEFAULT '',
		    base_path TEXT NOT NULL DEFAULT '',
		    collision_keys TEXT NOT NULL DEFAULT ''
		);
		INSERT INTO state_meta (id, cursor, base_path) VALUES (1, 'v2-cursor', '/old-scope/');
		CREATE TABLE files (
		    path TEXT PRIMARY KEY,
		    local_hash TEXT NOT NULL,
		    content_version INTEGER NOT NULL,
		    revision_id TEXT NOT NULL DEFAULT ''
		) WITHOUT ROWID;
		CREATE TABLE pushed_seqs (seq INTEGER PRIMARY KEY);
		PRAGMA user_version = 2;
	`
	if _, err := db.Exec(v2SQL); err != nil {
		t.Fatalf("seed v2 schema: %v", err)
	}
	db.Close()

	state, err := LoadState(dir, testIdentity)
	if err != nil {
		t.Fatalf("LoadState (v2 → v3): %v", err)
	}
	defer state.Close()

	if state.Cursor != "" {
		t.Errorf("Cursor = %q after schema reset, want empty", state.Cursor)
	}
	if len(state.Files) != 0 {
		t.Errorf("Files len = %d after schema reset, want 0", len(state.Files))
	}

	entries, _ := filepath.Glob(dbPath + ".corrupt.*")
	if len(entries) == 0 {
		t.Error("expected v2 DB to be quarantined under .corrupt.*")
	}
}

func TestLoadState_DoubleLockFails(t *testing.T) {
	dir := t.TempDir()
	state, err := LoadState(dir, testIdentity)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	defer state.Close()

	if _, err := LoadState(dir, testIdentity); err == nil {
		t.Fatal("second LoadState should fail on held lock")
	}
}

func TestPushedSeqs_AddAndCheck(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadState(dir, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.AddPushedSeq(10)
	s.AddPushedSeq(20)

	if !s.IsPushedSeq(10) {
		t.Error("10 should be pushed")
	}
	if s.IsPushedSeq(15) {
		t.Error("15 should not be pushed")
	}
}

func TestRecordFile(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadState(dir, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.RecordFile("docs/a.txt", "h1", 42, "rev_1")

	fs, ok := s.Files["docs/a.txt"]
	if !ok {
		t.Fatal("expected docs/a.txt in Files")
	}
	if fs.LocalHash != "h1" || fs.ContentVersion != 42 || fs.RevisionID != "rev_1" {
		t.Errorf("FileState = %+v", fs)
	}
}

func TestDeletePrefix(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadState(dir, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.RecordFile("foo/a.txt", "h1", 1, "rev_1")
	s.RecordFile("foo/b.txt", "h2", 2, "rev_2")
	s.RecordFile("foo/bar/c.txt", "h3", 3, "rev_3")
	s.RecordFile("foobar.txt", "h4", 4, "rev_4")
	s.RecordFile("other/d.txt", "h5", 5, "rev_5")

	n := s.DeletePrefix("foo/")
	if n != 3 {
		t.Errorf("DeletePrefix returned %d, want 3", n)
	}

	for _, p := range []string{"foo/a.txt", "foo/b.txt", "foo/bar/c.txt"} {
		if _, ok := s.Files[p]; ok {
			t.Errorf("%s should be deleted from Files", p)
		}
	}
	for _, p := range []string{"foobar.txt", "other/d.txt"} {
		if _, ok := s.Files[p]; !ok {
			t.Errorf("%s should still be in Files", p)
		}
	}

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := LoadState(dir, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	for _, p := range []string{"foo/a.txt", "foo/b.txt", "foo/bar/c.txt"} {
		if _, ok := s2.Files[p]; ok {
			t.Errorf("%s should not survive reload", p)
		}
	}
	for _, p := range []string{"foobar.txt", "other/d.txt"} {
		if _, ok := s2.Files[p]; !ok {
			t.Errorf("%s should survive reload", p)
		}
	}
}

func TestDeletePrefix_NoMatch(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadState(dir, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.RecordFile("foo/a.txt", "h1", 1, "rev_1")

	n := s.DeletePrefix("missing/")
	if n != 0 {
		t.Errorf("DeletePrefix returned %d, want 0", n)
	}
	if _, ok := s.Files["foo/a.txt"]; !ok {
		t.Error("foo/a.txt should still be present")
	}
}

func TestPushedSeqs_Prune(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadState(dir, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.AddPushedSeq(10)
	s.AddPushedSeq(20)
	s.AddPushedSeq(30)

	s.PrunePushedSeqs(20)

	if s.IsPushedSeq(10) {
		t.Error("10 should be pruned")
	}
	if !s.IsPushedSeq(20) {
		t.Error("20 should remain")
	}
	if !s.IsPushedSeq(30) {
		t.Error("30 should remain")
	}

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
}
