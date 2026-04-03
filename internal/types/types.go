package types

// FileState represents a file in the archive (state.json).
type FileState struct {
	LocalHash      string `json:"local_hash"`
	ContentVersion int64  `json:"content_version"`
	Size           int64  `json:"size"`
	SyncedAt       string `json:"synced_at"`
}

// LocalFile represents a file found during local walk.
type LocalFile struct {
	Hash    string
	Size    int64
	ModTime int64 // unix timestamp
}

// RemoteFile represents a file from remote listing.
type RemoteFile struct {
	Name       string
	Size       int64
	ModifiedAt string
	IsDir      bool
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
	default:
		return "unknown"
	}
}

// SyncPlan represents the classified action for a single file path.
type SyncPlan struct {
	Path   string
	Action SyncAction
}

// --- API response types (matching OpenAPI spec) ---

// MeResponse from GET /api/me (token auth).
type MeTokenResponse struct {
	Type        string       `json:"type"` // "token"
	UserID      string       `json:"user_id"`
	TokenID     string       `json:"token_id"`
	CanDelegate bool         `json:"can_delegate"`
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
}

// ChangeEntry from GET /api/changes.
type ChangeEntry struct {
	Seq            int64  `json:"seq"`
	TokenID        string `json:"token_id"`         // needs ADR 0033
	ContentVersion *int64 `json:"content_version"`  // needs ADR 0033
	Action         string `json:"action"`           // put, mkdir, delete, move
	PathBefore     string `json:"path_before"`
	PathAfter      string `json:"path_after"`
	IsDir          bool   `json:"is_dir"`
	Size           *int64 `json:"size"`
	Hash           string `json:"hash"`
	RevisionID     string `json:"revision_id"`
	CreatedAt      string `json:"created_at"`
}

// ChangesResponse from GET /api/changes.
type ChangesResponse struct {
	Changes        []ChangeEntry `json:"changes"`
	NextCursor     string        `json:"next_cursor"`
	ResyncRequired bool          `json:"resync_required"`
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
