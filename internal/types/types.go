package types

// FileState is the archive row: what we last saw for a given path
// after a successful sync. Persisted in .s2/state.db (ADR 0047).
type FileState struct {
	LocalHash      string
	ContentVersion int64
	RevisionID     string
}

// LocalFile represents a file found during local walk.
type LocalFile struct {
	Hash    string
	Size    int64
	ModTime int64
}

// RemoteFile represents a file from remote listing or a snapshot item.
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
type SyncPlan struct {
	Path       string
	Action     SyncAction
	RevisionID string
	Hash       string
}
