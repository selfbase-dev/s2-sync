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
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	watchPollInterval time.Duration
)

var watchCmd = &cobra.Command{
	Use:   "watch <local-dir>",
	Short: "Watch and sync local directory with S2 remote",
	Long: `Continuously sync between a local directory and the S2 remote.

The remote path is determined by the token's base_path.
Watches for local file changes and polls the remote for updates. Runs until interrupted (Ctrl-C).`,
	Args: cobra.ExactArgs(1),
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

	info, err := os.Stat(localDir)
	if err != nil {
		return fmt.Errorf("local directory not found: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", localDir)
	}

	if err := s2sync.EnsureIgnoreFile(localDir); err != nil {
		return fmt.Errorf("failed to create .s2ignore: %w", err)
	}

	token, err := auth.LoadToken()
	if err != nil {
		return err
	}

	endpoint := viper.GetString("endpoint")
	c := client.New(endpoint, token)

	me, err := c.Me()
	if err != nil {
		return fmt.Errorf("failed to get auth context: %w", err)
	}
	remotePrefix := strings.TrimPrefix(me.BasePath, "/")

	var syncMu sync.Mutex

	// Initial full sync
	fmt.Fprintln(cmd.OutOrStdout(), "Running initial sync...")
	if err := doSync(cmd, localDir, remotePrefix, c, &syncMu); err != nil {
		return fmt.Errorf("initial sync failed: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

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

	pollFn := func() (bool, bool, error) {
		state, err := s2sync.LoadState(localDir)
		if err != nil {
			return false, false, err
		}
		if state.Cursor == "" {
			return false, true, nil
		}
		resp, err := c.PollChanges(state.Cursor)
		if err == client.ErrCursorGone {
			return false, true, nil
		}
		if err != nil {
			return false, false, err
		}
		hasRemoteChanges := false
		for _, ch := range resp.Changes {
			if state.IsPushedSeq(ch.Seq) {
				continue
			}
			hasRemoteChanges = true
			break
		}
		hasCursorWork := len(resp.Changes) > 0
		return hasRemoteChanges || hasCursorWork, false, nil
	}

	syncFn := func() error {
		return doSync(cmd, localDir, remotePrefix, c, &syncMu)
	}

	go watchLoop(WatchLoopConfig{
		SyncFn:       syncFn,
		PollFn:       pollFn,
		LocalEvents:  localChanged,
		PollInterval: watchPollInterval,
		Debounce:     2 * time.Second,
		Ctx:          ctx,
	})

	<-sigCh
	fmt.Fprintln(cmd.OutOrStdout(), "\nShutting down...")
	cancel()
	return nil
}

func doSync(cmd *cobra.Command, localDir, remotePrefix string, c *client.Client, mu *sync.Mutex) error {
	mu.Lock()
	defer mu.Unlock()

	state, err := s2sync.LoadState(localDir)
	if err != nil {
		return fmt.Errorf("failed to load state: %w", err)
	}

	opts := s2sync.SyncOptions{
		Stdout: cmd.OutOrStdout(),
		Stderr: cmd.ErrOrStderr(),
	}

	if state.Cursor == "" {
		return s2sync.RunInitialSync(c, localDir, remotePrefix, state, opts)
	}
	return s2sync.RunIncrementalSync(c, localDir, remotePrefix, state, opts)
}

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
