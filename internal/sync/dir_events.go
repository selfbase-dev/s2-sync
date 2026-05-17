// dir_events.go — Hybrid strategy for is_dir change events (ADR 0040).

package sync

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/selfbase-dev/s2-sync/internal/client"
	slog2 "github.com/selfbase-dev/s2-sync/internal/log"
	"github.com/selfbase-dev/s2-sync/internal/types"
)

// SubtreeSnapshot carries a remote file map that the caller must
// Compare() AFTER re-walking the local tree.
type SubtreeSnapshot struct {
	Prefix string
	Remote map[string]types.RemoteFile
}

// DirEventOutcome carries the results of hybrid-strategy processing.
type DirEventOutcome struct {
	ArchiveWalkPlans []types.SyncPlan
	SubtreeSnapshots []SubtreeSnapshot
	NewPrimaryCursor string
	LocalChanged     bool
}

// HandleIncrementalDirEvents applies the hybrid strategy from ADR 0040
// to every is_dir entry in `changes`. Dir-level moves rename archive
// rows in-place via `state.MoveFile`; dir-level deletes expand into
// per-file plans that the executor will later carry out.
//
// Each dir change is wrapped in a `dir.event` lifecycle
// (kind=received/applied/done/error) so operators can correlate
// high-level events with the per-file fan-out that follows.
func HandleIncrementalDirEvents(
	c *client.Client,
	localDir string,
	state *State,
	changes []types.ChangeEntry,
	log *slog.Logger,
) (*DirEventOutcome, error) {
	if log == nil {
		log = slog.Default()
	}
	archive := state.Files
	outcome := &DirEventOutcome{}

	for _, ch := range changes {
		if !ch.IsDir {
			continue
		}
		switch ch.Action {
		case "mkdir":
			p, err := safeRelPrefix(ch.PathAfter)
			if err != nil {
				logDirEventError(log, ch.Action, ch.PathBefore, ch.PathAfter, err)
				return nil, fmt.Errorf("mkdir %s: %w", ch.PathAfter, err)
			}
			if p == "" {
				continue
			}
			logDirEventReceived(log, ch.Action, ch.PathBefore, ch.PathAfter)
			target, err := safeJoin(localDir, strings.TrimSuffix(p, "/"))
			if err != nil {
				logDirEventError(log, ch.Action, ch.PathBefore, ch.PathAfter, err)
				return nil, fmt.Errorf("mkdir %s: %w", ch.PathAfter, err)
			}
			if err := os.MkdirAll(target, 0755); err != nil {
				logDirEventError(log, ch.Action, ch.PathBefore, ch.PathAfter, err)
				return nil, fmt.Errorf("mkdir %s: %w", p, err)
			}
			log.Info(slog2.DirMkdir, "path", p, "side", "local", "kind", "dir_event")
			outcome.LocalChanged = true
			logDirEventDone(log, ch.Action, ch.PathBefore, ch.PathAfter, 1)

		case "delete":
			prefix, err := safeRelPrefix(ch.PathBefore)
			if err != nil {
				logDirEventError(log, ch.Action, ch.PathBefore, ch.PathAfter, err)
				return nil, fmt.Errorf("delete %s: %w", ch.PathBefore, err)
			}
			logDirEventReceived(log, ch.Action, ch.PathBefore, ch.PathAfter)
			plans, err := expandArchiveDelete(archive, localDir, prefix)
			if err != nil {
				logDirEventError(log, ch.Action, ch.PathBefore, ch.PathAfter, err)
				return nil, fmt.Errorf("expand delete %s: %w", prefix, err)
			}
			outcome.ArchiveWalkPlans = append(outcome.ArchiveWalkPlans, plans...)
			// `applied` = plans materialised (executor runs them
			// later); `done` = handler returned successfully.
			log.Info(slog2.DirEvent,
				"kind", "applied",
				"action", ch.Action,
				"path_before", ch.PathBefore,
				"expanded_count", len(plans),
			)
			logDirEventDone(log, ch.Action, ch.PathBefore, ch.PathAfter, len(plans))

		case "move":
			fromPrefix, err := safeRelPrefix(ch.PathBefore)
			if err != nil {
				logDirEventError(log, ch.Action, ch.PathBefore, ch.PathAfter, err)
				return nil, fmt.Errorf("move from %s: %w", ch.PathBefore, err)
			}
			toPrefix, err := safeRelPrefix(ch.PathAfter)
			if err != nil {
				logDirEventError(log, ch.Action, ch.PathBefore, ch.PathAfter, err)
				return nil, fmt.Errorf("move to %s: %w", ch.PathAfter, err)
			}
			logDirEventReceived(log, ch.Action, ch.PathBefore, ch.PathAfter)
			plans, localMutated, err := expandArchiveMove(
				state, log, localDir, fromPrefix, toPrefix,
			)
			if err != nil {
				logDirEventError(log, ch.Action, ch.PathBefore, ch.PathAfter, err)
				return nil, fmt.Errorf("move %s → %s: %w", fromPrefix, toPrefix, err)
			}
			outcome.ArchiveWalkPlans = append(outcome.ArchiveWalkPlans, plans...)
			if localMutated {
				outcome.LocalChanged = true
			}
			// expanded_count covers BOTH the in-place renames that
			// already happened (mutated locally and in archive) and
			// the deferred PreserveLocalRename plans the executor
			// will run. There is no cheap exact count of in-place
			// renames here without retracing the archive, so we
			// approximate with `len(plans) + mutated?` and call it
			// out in the field name.
			deferred := len(plans)
			log.Info(slog2.DirEvent,
				"kind", "applied",
				"action", ch.Action,
				"path_before", ch.PathBefore,
				"path_after", ch.PathAfter,
				"deferred_plans", deferred,
				"local_mutated", localMutated,
			)
			logDirEventDone(log, ch.Action, ch.PathBefore, ch.PathAfter, deferred)

		case "put":
			isRoot := ch.PathAfter == "" || ch.PathAfter == "/"

			if isRoot {
				logDirEventReceived(log, ch.Action, ch.PathBefore, ch.PathAfter)
				outcome.ArchiveWalkPlans = nil
				outcome.SubtreeSnapshots = nil

				remote, cursor, err := Bootstrap(c)
				if err != nil {
					logDirEventError(log, ch.Action, ch.PathBefore, ch.PathAfter, err)
					return nil, fmt.Errorf("bootstrap (scope-root put): %w", err)
				}
				outcome.SubtreeSnapshots = append(outcome.SubtreeSnapshots, SubtreeSnapshot{
					Prefix: "",
					Remote: remote,
				})
				outcome.NewPrimaryCursor = cursor
				log.Info(slog2.DirEvent,
					"kind", "applied",
					"action", ch.Action,
					"path_after", "/",
					"snapshot_files", len(remote),
				)
				logDirEventDone(log, ch.Action, ch.PathBefore, ch.PathAfter, len(remote))
			} else {
				if _, err := safeRelPrefix(ch.PathAfter); err != nil {
					logDirEventError(log, ch.Action, ch.PathBefore, ch.PathAfter, err)
					return nil, fmt.Errorf("put %s: %w", ch.PathAfter, err)
				}
				logDirEventReceived(log, ch.Action, ch.PathBefore, ch.PathAfter)
				remote, err := FetchRemoteMap(c, ch.PathAfter)
				if err != nil {
					if errors.Is(err, client.ErrNotFound) {
						log.Info(slog2.DirEvent,
							"kind", "done",
							"action", ch.Action,
							"path_after", ch.PathAfter,
							"result", "not_found",
						)
						continue
					}
					logDirEventError(log, ch.Action, ch.PathBefore, ch.PathAfter, err)
					return nil, fmt.Errorf("fetch subtree %s: %w", ch.PathAfter, err)
				}
				prefix, _ := safeRelPrefix(ch.PathAfter)
				outcome.SubtreeSnapshots = append(outcome.SubtreeSnapshots, SubtreeSnapshot{
					Prefix: prefix,
					Remote: remote,
				})
				log.Info(slog2.DirEvent,
					"kind", "applied",
					"action", ch.Action,
					"path_after", ch.PathAfter,
					"snapshot_files", len(remote),
				)
				logDirEventDone(log, ch.Action, ch.PathBefore, ch.PathAfter, len(remote))
			}
		}
	}
	return outcome, nil
}

func logDirEventReceived(log *slog.Logger, action, pathBefore, pathAfter string) {
	attrs := []any{"kind", "received", "action", action}
	if pathBefore != "" {
		attrs = append(attrs, "path_before", pathBefore)
	}
	if pathAfter != "" {
		attrs = append(attrs, "path_after", pathAfter)
	}
	log.Info(slog2.DirEvent, attrs...)
}

func logDirEventDone(log *slog.Logger, action, pathBefore, pathAfter string, expanded int) {
	attrs := []any{"kind", "done", "action", action, "expanded_count", expanded}
	if pathBefore != "" {
		attrs = append(attrs, "path_before", pathBefore)
	}
	if pathAfter != "" {
		attrs = append(attrs, "path_after", pathAfter)
	}
	log.Info(slog2.DirEvent, attrs...)
}

func logDirEventError(log *slog.Logger, action, pathBefore, pathAfter string, err error) {
	attrs := []any{"kind", "error", "action", action, "err", err.Error()}
	if pathBefore != "" {
		attrs = append(attrs, "path_before", pathBefore)
	}
	if pathAfter != "" {
		attrs = append(attrs, "path_after", pathAfter)
	}
	log.Error(slog2.DirEvent, attrs...)
}

// SubtreeComparePlans runs Compare() against each snapshot in the outcome.
func (o *DirEventOutcome) SubtreeComparePlans(
	localFiles map[string]types.LocalFile,
	archive map[string]types.FileState,
) []types.SyncPlan {
	var plans []types.SyncPlan
	for _, snap := range o.SubtreeSnapshots {
		localSub := filterByPrefix(localFiles, snap.Prefix)
		archiveSub := filterByPrefix(archive, snap.Prefix)
		plans = append(plans, Compare(localSub, snap.Remote, archiveSub)...)
	}
	return plans
}

// MergePlansByPath returns the union of plan lists with last-writer-wins dedup.
func MergePlansByPath(lists ...[]types.SyncPlan) []types.SyncPlan {
	total := 0
	for _, l := range lists {
		total += len(l)
	}
	byPath := make(map[string]types.SyncPlan, total)
	for _, list := range lists {
		for _, p := range list {
			byPath[p.Path] = p
		}
	}
	out := make([]types.SyncPlan, 0, len(byPath))
	for _, p := range byPath {
		out = append(out, p)
	}
	sortPlansByPath(out)
	return out
}

// filterByPrefix returns a new map limited to keys matching `prefix`.
func filterByPrefix[T any](m map[string]T, prefix string) map[string]T {
	out := make(map[string]T)
	for k, v := range m {
		if prefix == "" || strings.HasPrefix(k, prefix) {
			out[k] = v
		}
	}
	return out
}
