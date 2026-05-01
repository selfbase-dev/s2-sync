package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/selfbase-dev/s2-sync/internal/client"
	s2sync "github.com/selfbase-dev/s2-sync/internal/sync"
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

The remote scope is determined by the token. Watches for local file
changes and polls the remote for updates. Runs until interrupted (Ctrl-C).`,
	Args: cobra.ExactArgs(1),
	RunE: runWatch,
}

func init() {
	watchCmd.Flags().DurationVar(&watchPollInterval, "poll-interval", 10*time.Second, "Remote change polling interval")
	rootCmd.AddCommand(watchCmd)
}

func runWatch(cmd *cobra.Command, args []string) error {
	localDir := args[0]

	c, state, err := s2sync.Open(localDir, viper.GetString("endpoint"))
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// SIGINT/SIGTERM → ctx cancel, so the watch loop's shutdown discipline
	// (drain syncMu, close state) runs uniformly with the GUI service.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		Logger().Info("service.stop", "phase", "requested")
		cancel()
	}()

	exclude := s2sync.LoadExclude(localDir)
	opts := s2sync.WatchOptions{
		LocalDir:     localDir,
		Exclude:      exclude,
		PollInterval: watchPollInterval,
	}

	syncOpts := s2sync.SyncOptions{Logger: Logger()}
	cb := s2sync.WatchCallbacks{
		SyncFn: func() error {
			if state.Cursor == "" {
				return s2sync.RunInitialSync(c, localDir, state, syncOpts)
			}
			return s2sync.RunIncrementalSync(c, localDir, state, syncOpts)
		},
		PollFn: func() (bool, bool, error) {
			cursor := state.Cursor
			if cursor == "" {
				return false, true, nil
			}
			resp, err := c.PollChanges(cursor)
			if err == client.ErrCursorGone {
				return false, true, nil
			}
			if err != nil {
				return false, false, err
			}
			for _, ch := range resp.Changes {
				if !state.IsPushedSeq(ch.Seq) {
					return true, false, nil
				}
			}
			return len(resp.Changes) > 0, false, nil
		},
	}

	Logger().Info("sync.start", "phase", "initial")
	Logger().Info("watch.start",
		"local", localDir,
		"poll_interval", watchPollInterval.String(),
	)
	if err := s2sync.RunWatchLoop(ctx, opts, state, cb); err != nil {
		return fmt.Errorf("watch: %w", err)
	}
	return nil
}
