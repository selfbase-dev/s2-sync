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
	// This ensures remote edits are detected even when cursor was lost.
	state.Files = make(map[string]types.FileState)

	// Walk local
	exclude := s2sync.LoadExclude(localDir)
	localFiles, err := s2sync.Walk(localDir, state.Files, exclude)
	if err != nil {
		return fmt.Errorf("local scan failed: %w", err)
	}

	// List remote recursively
	fmt.Fprintln(cmd.OutOrStdout(), "Listing remote files...")
	remoteFiles, err := c.ListAllRecursive(remotePrefix)
	if err != nil {
		return fmt.Errorf("remote list failed: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Local: %d files, Remote: %d files\n",
		len(localFiles), len(remoteFiles))

	// Three-way compare (no archive for initial sync)
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

	// Get cursor AFTER sync completes (not before — avoids double-applying changes).
	// Only set cursor if all operations succeeded; otherwise retry next time.
	hasErrors := execResult != nil && len(execResult.Errors) > 0
	if !hasErrors {
		cursor, err := c.LatestCursor()
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to get cursor: %v\n", err)
		} else {
			state.Cursor = cursor
		}
	}

	// Save state
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

	// ADR 0038: expand is_dir events into file-level events. Non-is_dir
	// events pass through unchanged; directory-level operations (mkdir,
	// dir delete) are collected in order so ApplyDirSideEffects can
	// apply them after the file plans run without losing event ordering.
	fileChanges, dirOps, err := s2sync.ExpandDirEvents(remoteChanges, state.Files, c, remotePrefix)
	if err != nil {
		return fmt.Errorf("expand dir events failed: %w", err)
	}

	// Incremental three-way compare
	plans := s2sync.CompareIncremental(localFiles, state.Files, fileChanges)

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

	// ADR 0038: apply directory-level side effects (mkdir / empty-dir
	// cleanup) in event order. Best effort — failures here shouldn't
	// block cursor advancement.
	if !dryRun {
		s2sync.ApplyDirSideEffects(cmd.OutOrStdout(), localDir, dirOps)
	}

	// Only advance cursor if all operations succeeded.
	// If some failed, keep old cursor so those changes are retried next time.
	hasErrors := execResult != nil && len(execResult.Errors) > 0
	if !hasErrors && resp.NextCursor != "" {
		state.Cursor = resp.NextCursor
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

