package types

// FileState represents the state of a file in the archive (state.json).
type FileState struct {
	LocalHash  string `json:"local_hash"`
	RemoteETag string `json:"remote_etag"`
	Size       int64  `json:"size"`
	SyncedAt   string `json:"synced_at"`
}

// LocalFile represents a file found during local walk.
type LocalFile struct {
	Hash    string
	Size    int64
	ModTime int64 // unix timestamp
}

// RemoteFile represents a file found during remote list.
type RemoteFile struct {
	ETag         string
	Size         int64
	LastModified string
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
