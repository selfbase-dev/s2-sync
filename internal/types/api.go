package types

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

// MeTokenResponse from GET /api/me (token auth).
type MeTokenResponse struct {
	Type        string       `json:"type"`
	UserID      string       `json:"user_id"`
	TokenID     string       `json:"token_id"`
	CanDelegate bool         `json:"can_delegate"`
	BasePath    string       `json:"base_path"`
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

// ListResponse from GET /api/files/{path}/.
type ListResponse struct {
	Items []FileItem `json:"items"`
}

// UploadResult from PUT /api/files/{path} or POST /api/uploads/{id}/complete.
type UploadResult struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Size           int64  `json:"size"`
	Hash           string `json:"hash"`
	ContentVersion int64  `json:"content_version"`
	Seq            *int64 `json:"seq"`
}

// DeleteResult from DELETE /api/files/{path}.
type DeleteResult struct {
	Seq *int64 `json:"seq"`
}

// MoveResult from POST /api/file-moves/{path}. Carries the changelog
// seq (so the client can self-change-filter) and the node's
// content_version (for archive updates after a case-only rename).
type MoveResult struct {
	ID             string `json:"id"`
	Seq            *int64 `json:"seq"`
	ContentVersion int64  `json:"content_version"`
}

// ChangeEntry from GET /api/changes.
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

// ChangesResponse from GET /api/changes.
type ChangesResponse struct {
	Changes    []ChangeEntry `json:"changes"`
	NextCursor string        `json:"next_cursor"`
}

// --- Snapshot primitive (ADR 0039) ---

// SnapshotItem is one metadata entry returned by GET /api/snapshot.
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
