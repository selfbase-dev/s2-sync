package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
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

On cursor invalidation (410 Gone), falls back to full R2 list resync.`,
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
	PollFn       func() (hasChanges, cursorInvalid bool, err error)
	LocalEvents  <-chan struct{}
	PollInterval time.Duration
	Debounce     time.Duration
	Ctx          context.Context
}

// watchLoop is the testable core of the watch command.
// It listens for local and remote changes, debounces, and triggers sync.
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
			hasChanges, cursorInvalid, err := cfg.PollFn()
			if err != nil {
				continue
			}
			if hasChanges || cursorInvalid {
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

	// Initial full sync
	fmt.Fprintln(cmd.OutOrStdout(), "Running initial sync...")
	if err := doSync(cmd, localDir, remotePrefix, c); err != nil {
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
		changes, err := c.PollChanges(state.Cursor, 1)
		if err == client.ErrCursorInvalid {
			return false, true, nil
		}
		if err != nil {
			return false, false, err
		}
		return len(changes) > 0, false, nil
	}

	// Build sync function
	syncFn := func() error {
		return doSync(cmd, localDir, remotePrefix, c)
	}

	// Run watch loop in goroutine, listen for signal in main
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

// doSync performs a full sync cycle (same logic as sync command).
func doSync(cmd *cobra.Command, localDir, remotePrefix string, c *client.Client) error {
	state, err := s2sync.LoadState(localDir)
	if err != nil {
		return fmt.Errorf("failed to load state: %w", err)
	}
	state.RemotePrefix = remotePrefix

	exclude := s2sync.LoadExclude(localDir)

	// Walk local
	localFiles, err := s2sync.Walk(localDir, state.Files, exclude)
	if err != nil {
		return fmt.Errorf("local scan failed: %w", err)
	}

	// Determine remote files
	remoteFiles, newCursor, err := getRemoteState(c, remotePrefix, state)
	if err != nil {
		return fmt.Errorf("remote state failed: %w", err)
	}

	// Three-way compare
	plans := s2sync.Compare(localFiles, remoteFiles, state.Files)

	if len(plans) == 0 {
		// Update cursor even if nothing to sync
		if newCursor > state.Cursor {
			state.Cursor = newCursor
			_ = s2sync.SaveState(localDir, state)
		}
		return nil
	}

	// Print summary
	counts := make(map[types.SyncAction]int)
	for _, p := range plans {
		counts[p.Action]++
	}
	ts := time.Now().Format("15:04:05")
	fmt.Fprintf(cmd.OutOrStdout(), "[%s] sync: %d push, %d pull, %d delete, %d conflict\n",
		ts, counts[types.Push], counts[types.Pull],
		counts[types.DeleteLocal]+counts[types.DeleteRemote], counts[types.Conflict])

	// Execute
	result, err := s2sync.Execute(plans, localDir, remotePrefix, c, state, false)
	if err != nil {
		return err
	}

	// Update cursor
	if newCursor > state.Cursor {
		state.Cursor = newCursor
	}

	// Save state
	if err := s2sync.SaveState(localDir, state); err != nil {
		return fmt.Errorf("failed to save state: %w", err)
	}

	if len(result.Errors) > 0 {
		for _, e := range result.Errors {
			fmt.Fprintf(cmd.ErrOrStderr(), "  error: %v\n", e)
		}
	}

	return nil
}

// getRemoteState builds the remote file map. Uses R2 list (full scan) since
// change_log only tells us what changed but not the current ETag.
// However, we use the cursor to know where we are for next poll.
func getRemoteState(c *client.Client, remotePrefix string, state *s2sync.State) (map[string]types.RemoteFile, int64, error) {
	// Get latest cursor before listing (Dropbox pattern: ADR 0009 §同期中の安全性)
	latestCursor, err := c.LatestCursor()
	if err != nil {
		// Non-fatal: proceed without cursor update
		latestCursor = state.Cursor
	}

	// R2 list for full remote state
	remoteObjects, err := c.ListAll(remotePrefix)
	if err != nil {
		return nil, 0, fmt.Errorf("remote list failed: %w", err)
	}

	remoteFiles := make(map[string]types.RemoteFile)
	for _, obj := range remoteObjects {
		relPath := strings.TrimPrefix(obj.Key, remotePrefix)
		if relPath == "" {
			continue
		}
		remoteFiles[relPath] = types.RemoteFile{
			ETag:         obj.ETag,
			Size:         obj.Size,
			LastModified: obj.LastModified,
		}
	}

	return remoteFiles, latestCursor, nil
}

// addWatchDirs recursively adds directories to the fsnotify watcher.
func addWatchDirs(watcher *fsnotify.Watcher, root string, exclude func(string) bool) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip errors
		}
		if !info.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		// Skip .s2/
		if rel == ".s2" || strings.HasPrefix(rel, ".s2/") {
			return filepath.SkipDir
		}

		// Skip excluded directories
		if rel != "." && exclude != nil && exclude(rel) {
			return filepath.SkipDir
		}

		return watcher.Add(path)
	})
}
