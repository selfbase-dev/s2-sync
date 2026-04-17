package sync

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/selfbase-dev/s2-sync/internal/types"
	_ "modernc.org/sqlite"
)

// schemaVersion bumps whenever the SQL layout changes. A mismatch in
// .s2/state.db triggers a full reset (quarantine + recreate), per the
// "state.db is a cache" policy.
const schemaVersion = 1

const schemaSQL = `
PRAGMA journal_mode = WAL;

CREATE TABLE IF NOT EXISTS state_meta (
    id        INTEGER PRIMARY KEY CHECK (id = 1),
    cursor    TEXT NOT NULL DEFAULT '',
    endpoint  TEXT NOT NULL DEFAULT '',
    user_id   TEXT NOT NULL DEFAULT '',
    base_path TEXT NOT NULL DEFAULT ''
);
INSERT OR IGNORE INTO state_meta (id) VALUES (1);

CREATE TABLE IF NOT EXISTS files (
    path            TEXT PRIMARY KEY,
    local_hash      TEXT NOT NULL,
    content_version INTEGER NOT NULL,
    revision_id     TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS pushed_seqs (seq INTEGER PRIMARY KEY);
`

// dbFromAny is a tiny helper to keep *sql.DB hidden in State.db as
// `any` (so test files that don't touch the driver don't need to).
func dbFromAny(db any) *sql.DB { return db.(*sql.DB) }

// closeDB closes the underlying sql.DB.
func closeDB(db any) error { return db.(*sql.DB).Close() }

// openDB opens .s2/state.db with WAL mode. If the file is corrupt or
// the schema version does not match, quarantines the old DB (including
// -wal/-shm sidecars) and creates a fresh one.
func openDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}
	// Single connection avoids locking surprises; we already serialize
	// through the State's own mutex.
	db.SetMaxOpenConns(1)

	if err := ensureSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func ensureSchema(db *sql.DB) error {
	var ver int
	if err := db.QueryRow("PRAGMA user_version").Scan(&ver); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if ver != 0 && ver != schemaVersion {
		return fmt.Errorf("schema version %d does not match expected %d", ver, schemaVersion)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	if ver == 0 {
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			return fmt.Errorf("set user_version: %w", err)
		}
	}
	return nil
}

// quarantineDB moves state.db plus -wal / -shm sidecars under
// .s2/state.db.corrupt.<timestamp>/ so the next open starts clean.
// Missing sidecars are tolerated.
func quarantineDB(dbPath string) error {
	stamp := time.Now().UTC().Format("20060102T150405Z")
	dstDir := dbPath + ".corrupt." + stamp
	if err := os.MkdirAll(dstDir, 0700); err != nil {
		return fmt.Errorf("mkdir quarantine: %w", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		src := dbPath + suffix
		if _, err := os.Stat(src); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		dst := filepath.Join(dstDir, filepath.Base(src))
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("move %s: %w", src, err)
		}
	}
	return nil
}

// dbSnapshot is everything pulled from the DB at LoadState time.
type dbSnapshot struct {
	Cursor     string
	Endpoint   string
	UserID     string
	BasePath   string
	Files      map[string]types.FileState
	PushedSeqs []int64
}

func loadSnapshot(db *sql.DB) (*dbSnapshot, error) {
	snap := &dbSnapshot{
		Files: make(map[string]types.FileState),
	}

	row := db.QueryRow(`SELECT cursor, endpoint, user_id, base_path FROM state_meta WHERE id = 1`)
	if err := row.Scan(&snap.Cursor, &snap.Endpoint, &snap.UserID, &snap.BasePath); err != nil {
		return nil, fmt.Errorf("read state_meta: %w", err)
	}

	rows, err := db.Query(`SELECT path, local_hash, content_version, revision_id FROM files`)
	if err != nil {
		return nil, fmt.Errorf("read files: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		var fs types.FileState
		if err := rows.Scan(&path, &fs.LocalHash, &fs.ContentVersion, &fs.RevisionID); err != nil {
			return nil, fmt.Errorf("scan files: %w", err)
		}
		snap.Files[path] = fs
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	seqRows, err := db.Query(`SELECT seq FROM pushed_seqs ORDER BY seq`)
	if err != nil {
		return nil, fmt.Errorf("read pushed_seqs: %w", err)
	}
	defer seqRows.Close()
	for seqRows.Next() {
		var seq int64
		if err := seqRows.Scan(&seq); err != nil {
			return nil, fmt.Errorf("scan pushed_seqs: %w", err)
		}
		snap.PushedSeqs = append(snap.PushedSeqs, seq)
	}
	return snap, seqRows.Err()
}

// flushParams bundles what Save writes in a single transaction.
type flushParams struct {
	Cursor   string
	Endpoint string
	UserID   string
	BasePath string

	Upserts map[string]types.FileState
	Deletes []string

	ClearAll bool // true → DELETE FROM files before applying upserts (initial sync)

	AddSeqs    []int64
	PruneBelow *int64 // if non-nil, DELETE FROM pushed_seqs WHERE seq < *PruneBelow
}

func flush(db *sql.DB, p flushParams) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`UPDATE state_meta SET cursor = ?, endpoint = ?, user_id = ?, base_path = ? WHERE id = 1`,
		p.Cursor, p.Endpoint, p.UserID, p.BasePath,
	); err != nil {
		return fmt.Errorf("update state_meta: %w", err)
	}

	if p.ClearAll {
		if _, err := tx.Exec(`DELETE FROM files`); err != nil {
			return fmt.Errorf("clear files: %w", err)
		}
	}

	if len(p.Deletes) > 0 {
		stmt, err := tx.Prepare(`DELETE FROM files WHERE path = ?`)
		if err != nil {
			return fmt.Errorf("prepare delete: %w", err)
		}
		for _, path := range p.Deletes {
			if _, err := stmt.Exec(path); err != nil {
				stmt.Close()
				return fmt.Errorf("delete file %s: %w", path, err)
			}
		}
		stmt.Close()
	}

	if len(p.Upserts) > 0 {
		stmt, err := tx.Prepare(
			`INSERT INTO files (path, local_hash, content_version, revision_id)
             VALUES (?, ?, ?, ?)
             ON CONFLICT(path) DO UPDATE SET
               local_hash = excluded.local_hash,
               content_version = excluded.content_version,
               revision_id = excluded.revision_id`,
		)
		if err != nil {
			return fmt.Errorf("prepare upsert: %w", err)
		}
		for path, fs := range p.Upserts {
			if _, err := stmt.Exec(path, fs.LocalHash, fs.ContentVersion, fs.RevisionID); err != nil {
				stmt.Close()
				return fmt.Errorf("upsert file %s: %w", path, err)
			}
		}
		stmt.Close()
	}

	if len(p.AddSeqs) > 0 {
		stmt, err := tx.Prepare(`INSERT OR IGNORE INTO pushed_seqs (seq) VALUES (?)`)
		if err != nil {
			return fmt.Errorf("prepare seq insert: %w", err)
		}
		for _, seq := range p.AddSeqs {
			if _, err := stmt.Exec(seq); err != nil {
				stmt.Close()
				return fmt.Errorf("insert seq %d: %w", seq, err)
			}
		}
		stmt.Close()
	}
	if p.PruneBelow != nil {
		if _, err := tx.Exec(`DELETE FROM pushed_seqs WHERE seq < ?`, *p.PruneBelow); err != nil {
			return fmt.Errorf("prune pushed_seqs: %w", err)
		}
	}

	return tx.Commit()
}
