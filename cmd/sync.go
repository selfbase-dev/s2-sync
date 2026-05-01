package cmd

import (
	s2sync "github.com/selfbase-dev/s2-sync/internal/sync"
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

The remote scope is determined by the token. On conflict, local wins on
first sync. Subsequent syncs use saved state to detect changes.`,
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

	c, state, err := s2sync.Open(localDir, viper.GetString("endpoint"))
	if err != nil {
		return err
	}
	defer state.Close()

	opts := s2sync.SyncOptions{
		DryRun:       dryRun,
		Force:        force,
		MaxDeletePct: maxDelete,
		Logger:       Logger(),
	}

	if state.Cursor == "" {
		Logger().Info("sync.start", "phase", "initial")
		return s2sync.RunInitialSync(c, localDir, state, opts)
	}
	Logger().Info("sync.start", "phase", "incremental")
	return s2sync.RunIncrementalSync(c, localDir, state, opts)
}
