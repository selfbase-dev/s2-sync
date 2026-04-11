package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/selfbase-dev/s2-cli/internal/auth"
	"github.com/selfbase-dev/s2-cli/internal/client"
	s2sync "github.com/selfbase-dev/s2-cli/internal/sync"
	"github.com/selfbase-dev/s2-cli/internal/types"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	dryRun    bool
	force     bool
	maxDelete int
)

var syncCmd = &cobra.Command{
	Use:   "sync <local-dir>",
	Short: "Sync local directory with S2 remote",
	Long: `Bidirectional sync between a local directory and the S2 remote.

The remote path is determined by the token's base_path.
On conflict, local wins on first sync. Subsequent syncs use saved state to detect changes.`,
	Args: cobra.ExactArgs(1),
	RunE: runSync,
}

func init() {
	syncCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview changes without applying")
	syncCmd.Flags().BoolVar(&force, "force", false, "Skip safety checks (max-delete threshold)")
	syncCmd.Flags().IntVar(&maxDelete, "max-delete", 50, "Abort if deletes exceed this percentage of tracked files")
	rootCmd.AddCommand(syncCmd)
}

func runSync(cmd *cobra.Command, args []string) error {
	localDir := args[0]

	// Validate local directory
	info, err := os.Stat(localDir)
	if err != nil {
		return fmt.Errorf("local directory not found: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", localDir)
	}

	// Create default .s2ignore if missing
	if err := s2sync.EnsureIgnoreFile(localDir); err != nil {
		return fmt.Errorf("failed to create .s2ignore: %w", err)
	}

	// Load token and create client
	token, err := auth.LoadToken()
	if err != nil {
		return err
	}
	endpoint := viper.GetString("endpoint")
	c := client.New(endpoint, token)

	// Get token ID for self-change filtering; derive remotePrefix from base_path
	me, err := c.Me()
	if err != nil {
		return fmt.Errorf("failed to get auth context: %w", err)
	}
	remotePrefix := strings.TrimPrefix(me.BasePath, "/")

	// Load state
	state, err := s2sync.LoadState(localDir)
	if err != nil {
		return fmt.Errorf("failed to load state: %w", err)
	}
	state.TokenID = me.TokenID

	// Decide: initial sync or incremental
	if state.Cursor == "" {
		return runInitialSync(cmd, localDir, remotePrefix, c, state)
	}
	return runIncrementalSync(cmd, localDir, remotePrefix, c, state)
}

func runInitialSync(cmd *cobra.Command, localDir, remotePrefix string, c *client.Client, state *s2sync.State) error {
	fmt.Fprintln(cmd.OutOrStdout(), "Running initial sync...")

	// Clear archive: initial sync must compare fresh, not rely on stale archive.
	state.Files = make(map[string]types.FileState)

	// Walk local
	exclude := s2sync.LoadExclude(localDir)
	localFiles, err := s2sync.Walk(localDir, state.Files, exclude)
	if err != nil {
		return fmt.Errorf("local scan failed: %w", err)
	}

	// Atomic scope-root snapshot (ADR 0039 / ADR 0040 §hybrid 戦略).
	// Returns metadata + an atomic cursor in one request — replaces the
	// old "ListAllRecursive then LatestCursor" pair which had a race
	// window between the listing and the cursor.
	fmt.Fprintln(cmd.OutOrStdout(), "Fetching remote snapshot...")
	remoteFiles, snapshotCursor, err := s2sync.FetchSnapshotAsRemoteFiles(c, "")
	if err != nil {
		return fmt.Errorf("remote snapshot failed: %w", err)
	}

	// Idempotent apply: pre-populate the archive for files whose local
	// hash already matches the snapshot hash. Compare then short-circuits
	// to NoOp for those paths instead of routing them through Conflict
	// and forcing an unnecessary download round-trip (ADR 0040 §conflict
	// 検出).
	prefilled := s2sync.PrefillArchiveForIdempotentApply(state.Files, localFiles, remoteFiles)

	fmt.Fprintf(cmd.OutOrStdout(), "Local: %d files, Remote: %d files (%d already in sync)\n",
		len(localFiles), len(remoteFiles), prefilled)

	plans := s2sync.Compare(localFiles, remoteFiles, state.Files)

	var execResult *s2sync.ExecuteResult
	if len(plans) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "Everything is in sync.")
	} else {
		var err error
		execResult, err = executePlansAndReport(cmd, plans, localDir, remotePrefix, c, state)
		if err != nil {
			return err
		}
	}

	// Only adopt the snapshot cursor if all operations succeeded. Partial
	// failures would leave the client thinking it is at a sync boundary
	// it has not actually reached, causing the next incremental poll to
	// miss the retried files.
	hasErrors := execResult != nil && len(execResult.Errors) > 0
	if !hasErrors && snapshotCursor != "" {
		state.Cursor = snapshotCursor
	}

	if !dryRun {
		if err := s2sync.SaveState(localDir, state); err != nil {
			return fmt.Errorf("failed to save state: %w", err)
		}
	}

	return nil
}

func runIncrementalSync(cmd *cobra.Command, localDir, remotePrefix string, c *client.Client, state *s2sync.State) error {
	fmt.Fprintln(cmd.OutOrStdout(), "Checking for changes...")

	// Walk local
	exclude := s2sync.LoadExclude(localDir)
	localFiles, err := s2sync.Walk(localDir, state.Files, exclude)
	if err != nil {
		return fmt.Errorf("local scan failed: %w", err)
	}

	// Poll remote changes
	resp, err := c.PollChanges(state.Cursor)
	if err == client.ErrCursorGone {
		fmt.Fprintln(cmd.OutOrStdout(), "Cursor expired, falling back to full sync...")
		state.Cursor = ""
		return runInitialSync(cmd, localDir, remotePrefix, c, state)
	}
	if err != nil {
		return fmt.Errorf("poll changes failed: %w", err)
	}

	// ADR 0038 decision 4 removed `resync_required`. Scope-wide events
	// (ancestor move / base_path delete etc.) now arrive as explicit
	// `delete /` / `put /` entries and are handled by the hybrid
	// strategy below (ADR 0040 §操作×スコープマトリクス).

	// Filter remote changes (server already returns base_path-relative client paths)
	var remoteChanges []types.ChangeEntry
	for _, ch := range resp.Changes {
		// Self-change filter: seq-based (primary) + token_id (defense-in-depth)
		if state.IsPushedSeq(ch.Seq) {
			continue
		}
		if ch.TokenID != "" && ch.TokenID == state.TokenID {
			continue
		}
		remoteChanges = append(remoteChanges, ch)
	}

	// Split dir events (hybrid strategy) from file events (existing pipeline)
	var dirChanges, fileChanges []types.ChangeEntry
	for _, ch := range remoteChanges {
		if ch.IsDir {
			dirChanges = append(dirChanges, ch)
		} else {
			fileChanges = append(fileChanges, ch)
		}
	}

	// Apply the hybrid strategy to dir events: archive walk for
	// deletes / scope-internal moves, /api/snapshot for put dirs, and
	// os.MkdirAll for mkdir. May mutate state.Files and the local
	// tree, and may return a new primary cursor when a scope-root
	// snapshot is performed. Subtree snapshots are returned raw —
	// Compare() runs on them AFTER the post-side-effects re-walk so a
	// concurrent user edit can't be overwritten by a stale dir-plan.
	dirOutcome, err := s2sync.HandleIncrementalDirEvents(
		c, localDir, state.Files, dirChanges,
	)
	if err != nil {
		return fmt.Errorf("dir event handling: %w", err)
	}
	if dirOutcome.LocalChanged || len(dirOutcome.SubtreeSnapshots) > 0 {
		// Re-walk after mkdir / rename side effects AND before the
		// subtree Compare so the snapshot sees the fresh local tree.
		localFiles, err = s2sync.Walk(localDir, state.Files, exclude)
		if err != nil {
			return fmt.Errorf("local re-scan failed: %w", err)
		}
	}

	// Compare subtree snapshots against the post-re-walk local state.
	subtreePlans := dirOutcome.SubtreeComparePlans(localFiles, state.Files)

	// Incremental three-way compare for the file-level events. Merge
	// order (first → last, last wins): file-level events, then subtree
	// compare plans (fresh snapshots), then archive-walk plans
	// (dir-delete / move authoritative for those paths).
	fileLevelPlans := s2sync.CompareIncremental(localFiles, state.Files, fileChanges)
	plans := s2sync.MergePlansByPath(
		fileLevelPlans,
		subtreePlans,
		dirOutcome.ArchiveWalkPlans,
	)

	hasLocalChanges := false
	for path, l := range localFiles {
		if a, ok := state.Files[path]; !ok || l.Hash != a.LocalHash {
			hasLocalChanges = true
			break
		}
	}
	// Check for local deletions
	if !hasLocalChanges {
		for path := range state.Files {
			if _, ok := localFiles[path]; !ok {
				hasLocalChanges = true
				break
			}
		}
	}

	var execResult *s2sync.ExecuteResult
	if len(plans) == 0 && len(remoteChanges) == 0 && !hasLocalChanges {
		fmt.Fprintln(cmd.OutOrStdout(), "Everything is in sync.")
	} else if len(plans) > 0 {
		var err error
		execResult, err = executePlansAndReport(cmd, plans, localDir, remotePrefix, c, state)
		if err != nil {
			return err
		}
	}

	// Only advance cursor if all operations succeeded. ADR 0040 §cursor
	// semantics: a scope-root snapshot (from `put /` / ancestor enter
	// events) REPLACES the primary cursor wholesale instead of
	// advancing incrementally, effectively re-bootstrapping.
	hasErrors := execResult != nil && len(execResult.Errors) > 0
	if !hasErrors {
		if dirOutcome.NewPrimaryCursor != "" {
			state.Cursor = dirOutcome.NewPrimaryCursor
		} else if resp.NextCursor != "" {
			state.Cursor = resp.NextCursor
		}
	}

	if len(resp.Changes) > 0 {
		state.PrunePushedSeqs(resp.Changes[0].Seq)
	}

	if !dryRun {
		if err := s2sync.SaveState(localDir, state); err != nil {
			return fmt.Errorf("failed to save state: %w", err)
		}
	}

	return nil
}

func executePlansAndReport(cmd *cobra.Command, plans []types.SyncPlan, localDir, remotePrefix string, c *client.Client, state *s2sync.State) (*s2sync.ExecuteResult, error) {
	// Safety check: max-delete
	deleteCount := 0
	for _, p := range plans {
		if p.Action == types.DeleteLocal || p.Action == types.DeleteRemote {
			deleteCount++
		}
	}
	totalTracked := len(state.Files)
	if totalTracked > 0 && !force && deleteCount*100/totalTracked > maxDelete {
		return nil, fmt.Errorf("safety: %d deletes out of %d tracked files (%d%%) exceeds --max-delete=%d%%. Use --force to override",
			deleteCount, totalTracked, deleteCount*100/totalTracked, maxDelete)
	}

	// Print summary
	counts := make(map[types.SyncAction]int)
	for _, p := range plans {
		counts[p.Action]++
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Plan: %d push, %d pull, %d delete, %d conflict\n",
		counts[types.Push], counts[types.Pull],
		counts[types.DeleteLocal]+counts[types.DeleteRemote], counts[types.Conflict])

	if dryRun {
		fmt.Fprintln(cmd.OutOrStdout(), "\n--- Dry Run ---")
	}

	// Execute
	result, err := s2sync.Execute(plans, localDir, remotePrefix, c, state, dryRun)
	if err != nil {
		return nil, err
	}

	// Summary
	fmt.Fprintf(cmd.OutOrStdout(), "Done: %d pushed, %d pulled, %d deleted, %d conflicts",
		result.Pushed, result.Pulled, result.Deleted, result.Conflicts)
	if len(result.Errors) > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), ", %d errors", len(result.Errors))
		for _, e := range result.Errors {
			fmt.Fprintf(cmd.ErrOrStderr(), "  error: %v\n", e)
		}
	}
	fmt.Fprintln(cmd.OutOrStdout())

	return result, nil
}
