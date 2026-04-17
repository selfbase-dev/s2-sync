package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/selfbase-dev/s2-cli/internal/auth"
	"github.com/selfbase-dev/s2-cli/internal/client"
	s2sync "github.com/selfbase-dev/s2-cli/internal/sync"
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

	identity := s2sync.Identity{
		Endpoint: endpoint,
		UserID:   me.UserID,
		BasePath: me.BasePath,
	}
	state, err := s2sync.LoadState(localDir, identity)
	if err != nil {
		return fmt.Errorf("failed to load state: %w", err)
	}
	defer state.Close()

	opts := s2sync.SyncOptions{
		DryRun:       dryRun,
		Force:        force,
		MaxDeletePct: maxDelete,
		Stdout:       cmd.OutOrStdout(),
		Stderr:       cmd.ErrOrStderr(),
	}

	if state.Cursor == "" {
		fmt.Fprintln(cmd.OutOrStdout(), "Running initial sync...")
		return s2sync.RunInitialSync(c, localDir, remotePrefix, state, opts)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Checking for changes...")
	return s2sync.RunIncrementalSync(c, localDir, remotePrefix, state, opts)
}
