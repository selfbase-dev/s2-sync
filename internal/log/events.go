// Package log defines the structured logging foundation for s2-sync.
//
// All subsystems (CLI, sync engine, GUI service) emit through *slog.Logger.
// Sinks under sink/ fan the same record out to console, file, and Wails.
//
// Event names below are the canonical set. Filtering and color rules in
// the GUI key off the prefix (sync., file., dir., watch., oauth., service.).
package log

const (
	SyncStart          = "sync.start"
	SyncDone           = "sync.done"
	SyncError          = "sync.error"
	SyncIdle           = "sync.idle"
	SyncPlan           = "sync.plan"
	SyncWarn           = "sync.warn"
	SyncSkippedSummary = "sync.skipped_summary"
	SyncSkipDegenerate = "sync.skip_degenerate"

	FilePush     = "file.push"
	FilePull     = "file.pull"
	FileDelete   = "file.delete"
	FileMove     = "file.move"
	FileSkip     = "file.skip"
	FileConflict = "file.conflict"

	// Dir-event lifecycle and per-event mkdir. Per-file rename/delete
	// produced by dir events are emitted as FileMove/FileDelete with
	// the attr `kind: dir_event` so existing per-file filters still
	// surface them.
	DirEvent = "dir.event"
	DirMkdir = "dir.mkdir"

	ServiceStart = "service.start"
	ServiceStop  = "service.stop"
	ServiceError = "service.error"

	WatchStart = "watch.start"
	WatchError = "watch.error"

	StateError = "state.error"
)
