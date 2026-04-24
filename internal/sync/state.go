// state.go — SQLite-backed sync archive. The archive persists across
// runs: cursor, per-file hash/version, and pushed seqs for self-change
// filtering. See ADR 0047 for design rationale.

package sync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/selfbase-dev/s2-sync/internal/types"
)

// Identity is the (endpoint, user, scope) tuple under which the archive
// was populated. A mismatch at load time means the archive is stale for
// the current token and must be discarded.
type Identity struct {
	Endpoint string
	UserID   string
	BasePath string
}

// State wraps the SQLite-backed archive plus in-memory working set.
// Callers mutate Files via methods so dirty tracking can emit a
// minimal flush in Save.
//
// Lifetime: LoadState → use → Save (optional) → Close.
// One *State per process per sync root; Close releases both the DB
// handle and the .s2/state.lock advisory lock.
type State struct {
	mu sync.Mutex

	// Files is the live archive map, exposed read-only to callers that
	// consume it as `map[string]types.FileState` (Walk, Compare,
	// dir_events). Mutations must route through RecordFile /
	// DeleteFile / ClearFiles so dirty tracking captures them.
	Files map[string]types.FileState

	// Cursor is the remote change-log position.
	Cursor string

	// ReportedCollisions is the set of FoldKeys for collisions that
	// were already warned about in a previous sync. Compared against
	// the current sync's collisions to suppress repeated warnings.
	// Sorted, unique.
	ReportedCollisions []string

	pushedSeqs map[int64]struct{}

	identity Identity

	// dirty tracks paths whose row differs from what's in the DB
	// (either updated or deleted). clearAll means the next Save wipes
	// files table first.
	dirty    map[string]struct{}
	clearAll bool

	// addSeqs are seqs added since the last flush; pruneBelow triggers
	// a DELETE WHERE seq < pruneBelow at the next flush.
	addSeqs    []int64
	pruneBelow *int64

	db   any // *sql.DB; kept as any so test helpers don't import sqlite
	lock *fileLock
	root string
}

// StateDir returns the .s2 directory path within the sync root.
func StateDir(syncRoot string) string { return filepath.Join(syncRoot, ".s2") }

// DBPath returns the path to state.db.
func DBPath(syncRoot string) string { return filepath.Join(StateDir(syncRoot), "state.db") }

// LockPath returns the path to the advisory lock file.
func LockPath(syncRoot string) string { return filepath.Join(StateDir(syncRoot), "state.lock") }

// LoadState opens .s2/state.db for syncRoot and loads the archive into
// memory. Steps:
//  1. Acquire non-blocking advisory lock on .s2/state.lock.
//  2. Open/create state.db with the expected schema.
//     Corruption or version mismatch → quarantine + recreate.
//  3. Read state_meta + files + pushed_seqs into memory.
//  4. If identity (endpoint/user/base_path) doesn't match the loaded
//     row, discard the archive and start fresh (full resync).
//
// identity carries the current token's fingerprint; at first run the
// stored identity is empty and gets written on the first Save.
func LoadState(syncRoot string, identity Identity) (*State, error) {
	if err := os.MkdirAll(StateDir(syncRoot), 0700); err != nil {
		return nil, fmt.Errorf("create .s2 dir: %w", err)
	}

	lock, err := tryLock(LockPath(syncRoot))
	if err != nil {
		return nil, err
	}

	state, err := openAndLoad(syncRoot, identity)
	if err != nil {
		lock.Close()
		return nil, err
	}
	state.lock = lock
	return state, nil
}

func openAndLoad(syncRoot string, identity Identity) (*State, error) {
	dbPath := DBPath(syncRoot)

	db, err := openDB(dbPath)
	if err != nil {
		// Any open/schema failure → treat the DB as disposable cache,
		// quarantine and retry once.
		if qErr := quarantineDB(dbPath); qErr != nil {
			return nil, fmt.Errorf("open state.db: %w (quarantine also failed: %v)", err, qErr)
		}
		db, err = openDB(dbPath)
		if err != nil {
			return nil, fmt.Errorf("re-open after quarantine: %w", err)
		}
	}

	snap, err := loadSnapshot(dbFromAny(db))
	if err != nil {
		// Corrupt payload in an otherwise openable DB. Quarantine and
		// start fresh.
		_ = closeDB(db)
		if qErr := quarantineDB(dbPath); qErr != nil {
			return nil, fmt.Errorf("load snapshot: %w (quarantine also failed: %v)", err, qErr)
		}
		db, err = openDB(dbPath)
		if err != nil {
			return nil, fmt.Errorf("re-open after snapshot quarantine: %w", err)
		}
		snap, err = loadSnapshot(dbFromAny(db))
		if err != nil {
			_ = closeDB(db)
			return nil, fmt.Errorf("load snapshot after quarantine: %w", err)
		}
	}

	state := &State{
		Files:              snap.Files,
		Cursor:             snap.Cursor,
		ReportedCollisions: parseCollisionKeys(snap.CollisionKeys),
		pushedSeqs:         make(map[int64]struct{}, len(snap.PushedSeqs)),
		identity:           Identity{Endpoint: snap.Endpoint, UserID: snap.UserID, BasePath: snap.BasePath},
		dirty:              make(map[string]struct{}),
		db:                 db,
		root:               syncRoot,
	}
	for _, seq := range snap.PushedSeqs {
		state.pushedSeqs[seq] = struct{}{}
	}

	// Identity fingerprint: if the stored identity is set and doesn't
	// match the current token, the archive belongs to a different
	// scope. Treat it like corruption — quarantine the DB (including
	// -wal / -shm sidecars) and reopen empty so initial sync rebuilds
	// from scratch. In-memory wipe alone isn't enough: a dry-run or a
	// failure before Save would leave stale rows + WAL behind.
	if !state.identity.isEmpty() && state.identity != identity {
		if err := state.quarantineAndReopen(syncRoot, identity); err != nil {
			return nil, fmt.Errorf("identity mismatch reset: %w", err)
		}
	} else {
		state.identity = identity
	}

	return state, nil
}

func (s *State) quarantineAndReopen(syncRoot string, identity Identity) error {
	dbPath := DBPath(syncRoot)
	if s.db != nil {
		if err := closeDB(s.db); err != nil {
			return fmt.Errorf("close db before quarantine: %w", err)
		}
		s.db = nil
	}
	if err := quarantineDB(dbPath); err != nil {
		return fmt.Errorf("quarantine: %w", err)
	}
	db, err := openDB(dbPath)
	if err != nil {
		return fmt.Errorf("reopen after quarantine: %w", err)
	}
	snap, err := loadSnapshot(dbFromAny(db))
	if err != nil {
		_ = closeDB(db)
		return fmt.Errorf("load fresh snapshot: %w", err)
	}
	s.db = db
	s.Files = snap.Files
	s.Cursor = snap.Cursor
	s.pushedSeqs = make(map[int64]struct{})
	s.dirty = make(map[string]struct{})
	s.clearAll = false
	s.addSeqs = nil
	s.pruneBelow = nil
	s.identity = identity
	return nil
}

func (id Identity) isEmpty() bool {
	return id.Endpoint == "" && id.UserID == "" && id.BasePath == ""
}

// Close releases the DB handle and the advisory lock. Callers must
// call Save before Close to persist changes.
func (s *State) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var errs []string
	if s.db != nil {
		if err := closeDB(s.db); err != nil {
			errs = append(errs, fmt.Sprintf("close db: %v", err))
		}
		s.db = nil
	}
	if s.lock != nil {
		if err := s.lock.Close(); err != nil {
			errs = append(errs, fmt.Sprintf("release lock: %v", err))
		}
		s.lock = nil
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// Save writes the current in-memory state back to the DB in a single
// transaction. Only dirty entries (added/updated/deleted since the last
// load or save) are sent.
func (s *State) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.db == nil {
		return errors.New("state: Save after Close")
	}

	p := flushParams{
		Cursor:        s.Cursor,
		Endpoint:      s.identity.Endpoint,
		UserID:        s.identity.UserID,
		BasePath:      s.identity.BasePath,
		CollisionKeys: encodeCollisionKeys(s.ReportedCollisions),
		ClearAll:      s.clearAll,
	}

	if len(s.dirty) > 0 {
		upserts := make(map[string]types.FileState)
		var deletes []string
		for path := range s.dirty {
			if fs, ok := s.Files[path]; ok {
				upserts[path] = fs
			} else {
				deletes = append(deletes, path)
			}
		}
		p.Upserts = upserts
		p.Deletes = deletes
	}

	p.AddSeqs = s.addSeqs
	p.PruneBelow = s.pruneBelow

	if err := flush(dbFromAny(s.db), p); err != nil {
		return err
	}

	s.dirty = make(map[string]struct{})
	s.clearAll = false
	s.addSeqs = nil
	s.pruneBelow = nil
	return nil
}

// RecordFile upserts an archive entry. Called after a successful
// push/pull/conflict resolution.
func (s *State) RecordFile(path, hash string, cv int64, revisionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Files[path] = types.FileState{
		LocalHash:      hash,
		ContentVersion: cv,
		RevisionID:     revisionID,
	}
	s.dirty[path] = struct{}{}
}

// DeleteFile drops an archive entry. Called after a successful
// local-delete or remote-delete action.
func (s *State) DeleteFile(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Files, path)
	s.dirty[path] = struct{}{}
}

// MoveFile atomically renames an archive entry without losing the
// row's version metadata. Used by the directory-move handler.
func (s *State) MoveFile(oldPath, newPath string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fs, ok := s.Files[oldPath]
	if !ok {
		return
	}
	delete(s.Files, oldPath)
	s.Files[newPath] = fs
	s.dirty[oldPath] = struct{}{}
	s.dirty[newPath] = struct{}{}
}

// ClearFiles wipes the in-memory archive and arranges for the next
// Save to DELETE FROM files before re-populating. Used at the start
// of an initial sync.
func (s *State) ClearFiles() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Files = make(map[string]types.FileState)
	s.dirty = make(map[string]struct{})
	s.clearAll = true
}

// SetReportedCollisions replaces the debounce set with keys, deduped
// and sorted. Returns the newly-appeared and newly-resolved keys
// relative to the previous value, so the caller can log only the diff
//: warning is debounced; same collision does not re-log).
// Save writes state_meta unconditionally, so no dirty flag needed.
func (s *State) SetReportedCollisions(keys []string) (added, resolved []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sorted := uniqSorted(keys)
	prev := s.ReportedCollisions
	added, resolved = diffSortedStrings(prev, sorted)
	s.ReportedCollisions = sorted
	return added, resolved
}

// AddPushedSeq records a seq for self-change filtering.
func (s *State) AddPushedSeq(seq int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pushedSeqs[seq]; ok {
		return
	}
	s.pushedSeqs[seq] = struct{}{}
	s.addSeqs = append(s.addSeqs, seq)
}

// IsPushedSeq reports whether seq was emitted by this installation.
func (s *State) IsPushedSeq(seq int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.pushedSeqs[seq]
	return ok
}

// PrunePushedSeqs drops seqs older than minSeq from both the in-memory
// set and, at next Save, the DB.
func (s *State) PrunePushedSeqs(minSeq int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for seq := range s.pushedSeqs {
		if seq < minSeq {
			delete(s.pushedSeqs, seq)
		}
	}
	if len(s.addSeqs) > 0 {
		kept := s.addSeqs[:0]
		for _, seq := range s.addSeqs {
			if seq >= minSeq {
				kept = append(kept, seq)
			}
		}
		s.addSeqs = kept
	}
	v := minSeq
	s.pruneBelow = &v
}
