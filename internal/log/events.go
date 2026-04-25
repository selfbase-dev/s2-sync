// Package log defines the structured logging foundation for s2-sync.
//
// All subsystems (CLI, sync engine, GUI service) emit through *slog.Logger.
// Sinks under sink/ fan the same record out to console, file, and Wails.
//
// Event names below are the canonical set. Filtering and color rules in
// the GUI key off the prefix (sync., file., watch., oauth., service.).
package log

const (
	SyncStart = "sync.start"
	SyncDone  = "sync.done"
	SyncError = "sync.error"
	SyncIdle  = "sync.idle"
	SyncPlan  = "sync.plan"

	FilePush     = "file.push"
	FilePull     = "file.pull"
	FileDelete   = "file.delete"
	FileMove     = "file.move"
	FileSkip     = "file.skip"
	FileConflict = "file.conflict"

	ServiceStart = "service.start"
	ServiceStop  = "service.stop"
	ServiceError = "service.error"

	WatchStart = "watch.start"
	WatchError = "watch.error"

	StateError = "state.error"
)
