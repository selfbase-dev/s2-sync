package types

// FileState represents a file in the archive (state.json).
type FileState struct {
	LocalHash      string `json:"local_hash"`
	ContentVersion int64  `json:"content_version"`
	RevisionID     string `json:"revision_id,omitempty"` // for idempotent apply (ADR 0040)
	Size           int64  `json:"size"`
	SyncedAt       string `json:"synced_at"`
}

// LocalFile represents a file found during local walk.
type LocalFile struct {
	Hash    string
	Size    int64
	ModTime int64 // unix timestamp
}

// RemoteFile represents a file from remote listing or a snapshot item.
//
// Hash / RevisionID / ContentVersion are populated from /api/snapshot
// (ADR 0039) — they let the CLI compare without downloading and issue
// race-free content fetches via /api/revisions/{id} (ADR 0040
// §2段階fetchflow). They are zero values for legacy ListDir callers.
type RemoteFile struct {
	Name       string
	Size       int64
	ModifiedAt string
	IsDir      bool

	Hash           string
	RevisionID     string
	ContentVersion int64
}

// SyncAction represents what to do with a file during sync.
type SyncAction int

const (
	NoOp SyncAction = iota
	Push
	Pull
	DeleteLocal
	DeleteRemote
	Conflict
	// PreserveLocalRename is used when the server has authoritatively
	// removed a file (dir delete / move-out) but the local copy has
	// been edited since the last sync. The executor renames the local
	// file to a `.sync-conflict-*` copy and untracks it from the
	// archive — it does NOT push the local back (which would resurrect
	// the subtree the server just removed). See ADR 0040 §conflict
	// detection + the fix for codex review blocker #4.
	PreserveLocalRename
)

func (a SyncAction) String() string {
	switch a {
	case NoOp:
		return "no-op"
	case Push:
		return "push"
	case Pull:
		return "pull"
	case DeleteLocal:
		return "delete-local"
	case DeleteRemote:
		return "delete-remote"
	case Conflict:
		return "conflict"
	case PreserveLocalRename:
		return "preserve-local-rename"
	default:
		return "unknown"
	}
}

// SyncPlan represents the classified action for a single file path.
//
// `RevisionID` is populated when Compare / CompareIncremental knows the
// exact revision to fetch — for Pull / Conflict plans this enables the
// race-free /api/revisions/:id fetch path (ADR 0040 §2段階fetchflow).
// Empty for Push / Delete plans or when the source doesn't carry it.
type SyncPlan struct {
	Path       string
	Action     SyncAction
	RevisionID string
	Hash       string // "" = unknown; for idempotent apply (ADR 0040)
}

// --- API request types ---

// CreateTokenRequest for POST /api/tokens.
type CreateTokenRequest struct {
	Name        string       `json:"name"`
	BasePath    string       `json:"base_path,omitempty"`
	CanDelegate bool         `json:"can_delegate,omitempty"`
	AccessPaths []AccessPath `json:"access_paths"`
}

// CreateTokenResponse from POST /api/tokens.
type CreateTokenResponse struct {
	Token struct {
		ID          string       `json:"id"`
		Name        string       `json:"name"`
		BasePath    string       `json:"base_path"`
		CanDelegate bool         `json:"can_delegate"`
		Origin      string       `json:"origin"`
		OriginID    *string      `json:"origin_id"`
		CreatedAt   string       `json:"created_at"`
		AccessPaths []AccessPath `json:"access_paths"`
	} `json:"token"`
	RawToken string `json:"raw_token"`
}

// --- API response types (matching OpenAPI spec) ---

// MeResponse from GET /api/me (token auth).
type MeTokenResponse struct {
	Type        string       `json:"type"` // "token"
	UserID      string       `json:"user_id"`
	TokenID     string       `json:"token_id"`
	CanDelegate bool         `json:"can_delegate"`
	BasePath    string       `json:"base_path"` // token's virtual root (e.g. "/" or "/agents/")
	AccessPaths []AccessPath `json:"access_paths"`
}

// AccessPath represents a token's permission on a path.
type AccessPath struct {
	Path     string `json:"path"`
	CanRead  bool   `json:"can_read"`
	CanWrite bool   `json:"can_write"`
}

// FileItem from directory listing (GET /api/files/{path}/).
type FileItem struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"` // "file" or "directory"
	Size       *int64 `json:"size,omitempty"`
	ModifiedAt string `json:"modified_at,omitempty"`
}

// ListResponse from GET /api/files/{path}/.
type ListResponse struct {
	Items []FileItem `json:"items"`
}

// UploadResult from PUT /api/files/{path} or POST /api/uploads/{id}/complete.
type UploadResult struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	Hash string `json:"hash"`
	ETag string `json:"etag"`
	Seq  *int64 `json:"seq"` // changelog seq for self-change filtering (ADR 0035 #3)
}

// DeleteResult from DELETE /api/files/{path}.
type DeleteResult struct {
	Seq *int64 `json:"seq"` // changelog seq for self-change filtering (ADR 0035 #3)
}

// ChangeEntry from GET /api/changes.
type ChangeEntry struct {
	Seq            int64  `json:"seq"`
	TokenID        string `json:"token_id"`        // needs ADR 0033
	ContentVersion *int64 `json:"content_version"` // needs ADR 0033
	Action         string `json:"action"`          // put, mkdir, delete, move
	PathBefore     string `json:"path_before"`
	PathAfter      string `json:"path_after"`
	IsDir          bool   `json:"is_dir"`
	Size           *int64 `json:"size"`
	Hash           string `json:"hash"`
	RevisionID     string `json:"revision_id"`
	CreatedAt      string `json:"created_at"`
}

// ChangesResponse from GET /api/changes.
//
// ADR 0038 decision 4 removed `resync_required` from the server (SELF-287
// merged, SELF-290 shipped the snapshot primitive that replaces the full-
// resync escape hatch). Clients now recover from scope-wide events via
// the hybrid strategy in ADR 0040 (archive walk + /api/snapshot fetch).
type ChangesResponse struct {
	Changes    []ChangeEntry `json:"changes"`
	NextCursor string        `json:"next_cursor"`
}

// --- Snapshot primitive (ADR 0039) ---

// SnapshotItem is one metadata entry returned by GET /api/snapshot.
// Per ADR 0039 §API endpoint: directories end with "/" in `Path`, files
// do not. `Type` is "file" or "dir".
type SnapshotItem struct {
	Path           string `json:"path"`
	Type           string `json:"type"`
	ContentVersion int64  `json:"content_version"`
	RevisionID     string `json:"revision_id"`
	Size           *int64 `json:"size"`
	Hash           string `json:"hash"`
	ContentType    string `json:"content_type"`
}

// SnapshotResponse from GET /api/snapshot[?path=X].
//
// `Cursor` is the atomic changelog position paired with `Items` (ADR 0039
// §Atomicity). The CLI uses it as the primary cursor on bootstrap and
// 410 recovery, and as a hint during mid-incremental subtree fetches
// (ADR 0040 §cursor semantics).
type SnapshotResponse struct {
	Items  []SnapshotItem `json:"items"`
	Cursor string         `json:"cursor"`
}

// LatestCursorResponse from GET /api/changes/latest.
type LatestCursorResponse struct {
	Cursor string `json:"cursor"`
}

// UploadSession from POST /api/uploads.
type UploadSession struct {
	SessionID string `json:"sessionId"`
	NodeID    string `json:"nodeId"`
	ChunkSize int    `json:"chunkSize"`
	ExpiresAt string `json:"expiresAt"`
}

// APIError from error responses.
type APIError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}
