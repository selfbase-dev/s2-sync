package types

// --- API response types (matching OpenAPI spec) ---

// TokenIntrospection from GET /api/v1/token. Token holders never learn
// the absolute position of their base_path within the delegator's tree;
// all paths in API responses are relative to the token's base_path.
type TokenIntrospection struct {
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

// FileItem from directory listing (GET /api/v1/files/{path}/).
type FileItem struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Type           string  `json:"type"`
	Size           *int64  `json:"size,omitempty"`
	ModifiedAt     string  `json:"modified_at,omitempty"`
	Hash           *string `json:"hash,omitempty"`
	RevisionID     *string `json:"revision_id,omitempty"`
	ContentVersion *int64  `json:"content_version,omitempty"`
	ContentType    *string `json:"content_type,omitempty"`
}

// ListResponse from GET /api/v1/files/{path}/.
type ListResponse struct {
	Items []FileItem `json:"items"`
}

// UploadResult from PUT /api/v1/files/{path} or POST /api/v1/uploads/{id}/complete.
type UploadResult struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Size           int64  `json:"size"`
	Hash           string `json:"hash"`
	ContentVersion int64  `json:"content_version"`
	Seq            *int64 `json:"seq"`
}

// DeleteResult from DELETE /api/v1/files/{path}.
type DeleteResult struct {
	Seq *int64 `json:"seq"`
}

// MoveResult from POST /api/v1/file-moves/{path}. Carries the changelog
// seq (so the client can self-change-filter) and the node's
// content_version (for archive updates after a case-only rename).
type MoveResult struct {
	ID             string `json:"id"`
	Seq            *int64 `json:"seq"`
	ContentVersion int64  `json:"content_version"`
}

// ChangeEntry from GET /api/v1/changes.
type ChangeEntry struct {
	Seq            int64  `json:"seq"`
	TokenID        string `json:"token_id"`
	ContentVersion *int64 `json:"content_version"`
	Action         string `json:"action"`
	PathBefore     string `json:"path_before"`
	PathAfter      string `json:"path_after"`
	IsDir          bool   `json:"is_dir"`
	Size           *int64 `json:"size"`
	Hash           string `json:"hash"`
	RevisionID     string `json:"revision_id"`
	CreatedAt      string `json:"created_at"`
}

// ChangesResponse from GET /api/v1/changes.
type ChangesResponse struct {
	Changes    []ChangeEntry `json:"changes"`
	NextCursor string        `json:"next_cursor"`
}

// --- Snapshot primitive (ADR 0039) ---

// SnapshotItem is one metadata entry returned by GET /api/v1/snapshot.
type SnapshotItem struct {
	Path           string `json:"path"`
	Type           string `json:"type"`
	ContentVersion int64  `json:"content_version"`
	RevisionID     string `json:"revision_id"`
	Size           *int64 `json:"size"`
	Hash           string `json:"hash"`
	ContentType    string `json:"content_type"`
}

// SnapshotResponse from GET /api/v1/snapshot[?path=X].
type SnapshotResponse struct {
	Items  []SnapshotItem `json:"items"`
	Cursor string         `json:"cursor"`
}

// LatestCursorResponse from GET /api/v1/changes/latest.
type LatestCursorResponse struct {
	Cursor string `json:"cursor"`
}

// UploadSession from POST /api/v1/uploads.
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
