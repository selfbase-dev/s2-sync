package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/selfbase-dev/s2-cli/internal/auth"
	"github.com/selfbase-dev/s2-cli/internal/client"
	s2sync "github.com/selfbase-dev/s2-cli/internal/sync"
	"github.com/selfbase-dev/s2-cli/internal/types"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	watchPollInterval time.Duration
)

var watchCmd = &cobra.Command{
	Use:   "watch <local-dir> <remote-prefix>",
	Short: "Watch and sync local directory with S2 remote",
	Long: `Continuously watch for changes and sync bidirectionally.

Local changes are detected via fsnotify (OS-level file events).
Remote changes are detected via change_log cursor polling.

On cursor invalidation (410 Gone), falls back to full resync.`,
	Args: cobra.ExactArgs(2),
	RunE: runWatch,
}

func init() {
	watchCmd.Flags().DurationVar(&watchPollInterval, "poll-interval", 10*time.Second, "Remote change polling interval")
	rootCmd.AddCommand(watchCmd)
}

// --- Testable extracted functions ---

// shouldProcessEvent returns true if the event path should trigger a sync.
func shouldProcessEvent(rel string, exclude func(string) bool) bool {
	if rel == ".s2" || strings.HasPrefix(rel, ".s2/") {
		return false
	}
	if exclude != nil && exclude(rel) {
		return false
	}
	return true
}

// WatchLoopConfig holds injectable dependencies for the watch loop.
type WatchLoopConfig struct {
	SyncFn       func() error
	PollFn       func() (hasChanges, needResync bool, err error)
	LocalEvents  <-chan struct{}
	PollInterval time.Duration
	Debounce     time.Duration
	Ctx          context.Context
}

// watchLoop is the testable core of the watch command.
func watchLoop(cfg WatchLoopConfig) {
	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	var debounceTimer *time.Timer

	for {
		select {
		case <-cfg.Ctx.Done():
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			return

		case <-cfg.LocalEvents:
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.AfterFunc(cfg.Debounce, func() {
				_ = cfg.SyncFn()
			})

		case <-ticker.C:
			hasChanges, needResync, err := cfg.PollFn()
			if err != nil {
				continue
			}
			if hasChanges || needResync {
				if debounceTimer != nil {
					debounceTimer.Stop()
				}
				_ = cfg.SyncFn()
			}
		}
	}
}

// filterFsEventsWithAutoWatch reads fsnotify events, filters them, signals
// localChanged, and auto-watches newly created directories.
func filterFsEventsWithAutoWatch(watcher *fsnotify.Watcher, localDir string, exclude func(string) bool, localChanged chan<- struct{}) {
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			rel, err := filepath.Rel(localDir, event.Name)
			if err != nil {
				continue
			}
			rel = filepath.ToSlash(rel)
			if !shouldProcessEvent(rel, exclude) {
				continue
			}

			// Auto-watch new directories
			if event.Has(fsnotify.Create) {
				if fi, err := os.Stat(event.Name); err == nil && fi.IsDir() {
					_ = addWatchDirs(watcher, event.Name, exclude)
				}
			}

			select {
			case localChanged <- struct{}{}:
			default:
			}
		case _, ok := <-watcher.Errors:
			if !ok {
				return
			}
		}
	}
}

// --- Command implementation ---

func runWatch(cmd *cobra.Command, args []string) error {
	localDir := args[0]
	remotePrefix := args[1]

	// Normalize
	if !strings.HasSuffix(remotePrefix, "/") {
		remotePrefix += "/"
	}
	if strings.HasPrefix(remotePrefix, "/") {
		remotePrefix = remotePrefix[1:]
	}

	info, err := os.Stat(localDir)
	if err != nil {
		return fmt.Errorf("local directory not found: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", localDir)
	}

	token, err := auth.LoadToken()
	if err != nil {
		return err
	}

	endpoint := viper.GetString("endpoint")
	c := client.New(endpoint, token)

	// Get token ID
	me, err := c.Me()
	if err != nil {
		return fmt.Errorf("failed to get auth context: %w", err)
	}

	// Mutex for sync serialization
	var syncMu sync.Mutex

	// Initial full sync
	fmt.Fprintln(cmd.OutOrStdout(), "Running initial sync...")
	if err := doSync(cmd, localDir, remotePrefix, c, me.TokenID, &syncMu); err != nil {
		return fmt.Errorf("initial sync failed: %w", err)
	}

	// Setup context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Setup fsnotify watcher
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create file watcher: %w", err)
	}
	defer watcher.Close()

	exclude := s2sync.LoadExclude(localDir)
	if err := addWatchDirs(watcher, localDir, exclude); err != nil {
		return fmt.Errorf("failed to watch directory: %w", err)
	}

	localChanged := make(chan struct{}, 1)
	go filterFsEventsWithAutoWatch(watcher, localDir, exclude, localChanged)

	fmt.Fprintf(cmd.OutOrStdout(), "Watching %s ↔ %s (poll every %s, Ctrl+C to stop)\n",
		localDir, remotePrefix, watchPollInterval)

	// Build poll function
	pollFn := func() (bool, bool, error) {
		state, err := s2sync.LoadState(localDir)
		if err != nil {
			return false, false, err
		}
		if state.Cursor == "" {
			return false, true, nil // need full sync
		}
		resp, err := c.PollChanges(state.Cursor)
		if err == client.ErrCursorGone {
			return false, true, nil
		}
		if err != nil {
			return false, false, err
		}
		if resp.ResyncRequired {
			return false, true, nil
		}
		// Filter self-changes (same logic as sync.go)
		hasRemoteChanges := false
		for _, ch := range resp.Changes {
			if state.IsPushedSeq(ch.Seq) {
				continue
			}
			if ch.TokenID != "" && ch.TokenID == state.TokenID {
				continue
			}
			hasRemoteChanges = true
			break
		}
		return hasRemoteChanges, false, nil
	}

	// Build sync function
	syncFn := func() error {
		return doSync(cmd, localDir, remotePrefix, c, me.TokenID, &syncMu)
	}

	// Run watch loop
	go watchLoop(WatchLoopConfig{
		SyncFn:       syncFn,
		PollFn:       pollFn,
		LocalEvents:  localChanged,
		PollInterval: watchPollInterval,
		Debounce:     2 * time.Second,
		Ctx:          ctx,
	})

	// Wait for signal
	<-sigCh
	fmt.Fprintln(cmd.OutOrStdout(), "\nShutting down...")
	cancel()
	return nil
}

// doSync performs a full sync cycle with mutex for serialization.
func doSync(cmd *cobra.Command, localDir, remotePrefix string, c *client.Client, tokenID string, mu *sync.Mutex) error {
	mu.Lock()
	defer mu.Unlock()

	state, err := s2sync.LoadState(localDir)
	if err != nil {
		return fmt.Errorf("failed to load state: %w", err)
	}
	state.RemotePrefix = remotePrefix
	state.TokenID = tokenID

	if state.Cursor == "" {
		return runInitialSyncInner(cmd, localDir, remotePrefix, c, state)
	}
	return runIncrementalSyncInner(cmd, localDir, remotePrefix, c, state)
}

func runInitialSyncInner(cmd *cobra.Command, localDir, remotePrefix string, c *client.Client, state *s2sync.State) error {
	// Clear archive for fresh comparison (see sync.go runInitialSync comment)
	state.Files = make(map[string]types.FileState)

	exclude := s2sync.LoadExclude(localDir)
	localFiles, err := s2sync.Walk(localDir, state.Files, exclude)
	if err != nil {
		return fmt.Errorf("local scan failed: %w", err)
	}

	remoteFiles, err := c.ListAllRecursive(remotePrefix)
	if err != nil {
		return fmt.Errorf("remote list failed: %w", err)
	}

	plans := s2sync.Compare(localFiles, remoteFiles, state.Files)

	var hasErrors bool
	if len(plans) > 0 {
		ts := time.Now().Format("15:04:05")
		counts := make(map[types.SyncAction]int)
		for _, p := range plans {
			counts[p.Action]++
		}
		fmt.Fprintf(cmd.OutOrStdout(), "[%s] sync: %d push, %d pull, %d delete, %d conflict\n",
			ts, counts[types.Push], counts[types.Pull],
			counts[types.DeleteLocal]+counts[types.DeleteRemote], counts[types.Conflict])

		result, err := s2sync.Execute(plans, localDir, remotePrefix, c, state, false)
		if err != nil {
			return err
		}
		if len(result.Errors) > 0 {
			hasErrors = true
			for _, e := range result.Errors {
				fmt.Fprintf(cmd.ErrOrStderr(), "  error: %v\n", e)
			}
		}
	}

	// Only advance cursor if no errors
	if !hasErrors {
		cursor, err := c.LatestCursor()
		if err == nil {
			state.Cursor = cursor
		}
	}

	return s2sync.SaveState(localDir, state)
}

func runIncrementalSyncInner(cmd *cobra.Command, localDir, remotePrefix string, c *client.Client, state *s2sync.State) error {
	exclude := s2sync.LoadExclude(localDir)
	localFiles, err := s2sync.Walk(localDir, state.Files, exclude)
	if err != nil {
		return fmt.Errorf("local scan failed: %w", err)
	}

	resp, err := c.PollChanges(state.Cursor)
	if err == client.ErrCursorGone {
		state.Cursor = ""
		return runInitialSyncInner(cmd, localDir, remotePrefix, c, state)
	}
	if err != nil {
		return fmt.Errorf("poll changes failed: %w", err)
	}
	if resp.ResyncRequired {
		state.Cursor = ""
		return runInitialSyncInner(cmd, localDir, remotePrefix, c, state)
	}

	// Filter and normalize remote changes (same logic as sync.go)
	var remoteChanges []types.ChangeEntry
	for _, ch := range resp.Changes {
		if state.IsPushedSeq(ch.Seq) {
			continue
		}
		if ch.TokenID != "" && ch.TokenID == state.TokenID {
			continue
		}
		ch = stripAndFilterPrefix(ch, remotePrefix)
		if ch.Action == "" {
			continue
		}
		remoteChanges = append(remoteChanges, ch)
	}

	plans := s2sync.CompareIncremental(localFiles, state.Files, remoteChanges)

	var hasErrors bool
	if len(plans) > 0 {
		ts := time.Now().Format("15:04:05")
		counts := make(map[types.SyncAction]int)
		for _, p := range plans {
			counts[p.Action]++
		}
		fmt.Fprintf(cmd.OutOrStdout(), "[%s] sync: %d push, %d pull, %d delete, %d conflict\n",
			ts, counts[types.Push], counts[types.Pull],
			counts[types.DeleteLocal]+counts[types.DeleteRemote], counts[types.Conflict])

		result, err := s2sync.Execute(plans, localDir, remotePrefix, c, state, false)
		if err != nil {
			return err
		}
		if len(result.Errors) > 0 {
			hasErrors = true
			for _, e := range result.Errors {
				fmt.Fprintf(cmd.ErrOrStderr(), "  error: %v\n", e)
			}
		}
	}

	// Only advance cursor if no errors — failed changes need to be retried
	if !hasErrors {
		if resp.NextCursor != "" {
			state.Cursor = resp.NextCursor
		}
		if len(resp.Changes) > 0 {
			state.PrunePushedSeqs(resp.Changes[0].Seq)
		}
	}

	return s2sync.SaveState(localDir, state)
}

// addWatchDirs recursively adds directories to the fsnotify watcher.
func addWatchDirs(watcher *fsnotify.Watcher, root string, exclude func(string) bool) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		if rel == ".s2" || strings.HasPrefix(rel, ".s2/") {
			return filepath.SkipDir
		}
		if rel != "." && exclude != nil && exclude(rel) {
			return filepath.SkipDir
		}

		return watcher.Add(path)
	})
}
