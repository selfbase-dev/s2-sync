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
	Use:   "sync <local-dir> <remote-prefix>",
	Short: "Sync local directory with S2 remote",
	Long: `Bidirectional sync using archive-based three-way comparison.
Local files are compared against remote files and the last-known state (.s2/state.json).

On first sync (no state.json), local wins on conflicts.`,
	Args: cobra.ExactArgs(2),
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
	remotePrefix := args[1]

	// Normalize remote prefix
	if !strings.HasSuffix(remotePrefix, "/") {
		remotePrefix += "/"
	}
	if strings.HasPrefix(remotePrefix, "/") {
		remotePrefix = remotePrefix[1:]
	}

	// Validate local directory
	info, err := os.Stat(localDir)
	if err != nil {
		return fmt.Errorf("local directory not found: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", localDir)
	}

	// Load token
	token, err := auth.LoadToken()
	if err != nil {
		return err
	}

	endpoint := viper.GetString("endpoint")
	c := client.New(endpoint, token)

	// Load state
	state, err := s2sync.LoadState(localDir)
	if err != nil {
		return fmt.Errorf("failed to load state: %w", err)
	}
	state.RemotePrefix = remotePrefix

	// Walk local
	fmt.Fprintln(cmd.OutOrStdout(), "Scanning local files...")
	exclude := s2sync.LoadExclude(localDir)
	localFiles, err := s2sync.Walk(localDir, state.Files, exclude)
	if err != nil {
		return fmt.Errorf("local scan failed: %w", err)
	}

	// List remote
	fmt.Fprintln(cmd.OutOrStdout(), "Listing remote files...")
	remoteObjects, err := c.ListAll(remotePrefix)
	if err != nil {
		return fmt.Errorf("remote list failed: %w", err)
	}

	// Convert remote objects to map
	remoteFiles := make(map[string]types.RemoteFile)
	for _, obj := range remoteObjects {
		// Strip remote prefix to get relative path
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

	fmt.Fprintf(cmd.OutOrStdout(), "Local: %d files, Remote: %d files, Archive: %d files\n",
		len(localFiles), len(remoteFiles), len(state.Files))

	// Three-way compare
	plans := s2sync.Compare(localFiles, remoteFiles, state.Files)

	if len(plans) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "Everything is in sync.")
		return nil
	}

	// Safety check: max-delete
	deleteCount := 0
	for _, p := range plans {
		if p.Action == types.DeleteLocal || p.Action == types.DeleteRemote {
			deleteCount++
		}
	}
	totalTracked := len(state.Files)
	if totalTracked > 0 && !force && deleteCount*100/totalTracked > maxDelete {
		return fmt.Errorf("safety: %d deletes out of %d tracked files (%d%%) exceeds --max-delete=%d%%. Use --force to override",
			deleteCount, totalTracked, deleteCount*100/totalTracked, maxDelete)
	}

	// Print summary
	pushCount, pullCount, conflictCount := 0, 0, 0
	for _, p := range plans {
		switch p.Action {
		case types.Push:
			pushCount++
		case types.Pull:
			pullCount++
		case types.Conflict:
			conflictCount++
		}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Plan: %d push, %d pull, %d delete, %d conflict\n",
		pushCount, pullCount, deleteCount, conflictCount)

	if dryRun {
		fmt.Fprintln(cmd.OutOrStdout(), "\n--- Dry Run ---")
	}

	// Execute
	result, err := s2sync.Execute(plans, localDir, remotePrefix, c, state, dryRun)
	if err != nil {
		return err
	}

	// Save state (even if there were some errors, save partial progress)
	if !dryRun {
		if err := s2sync.SaveState(localDir, state); err != nil {
			return fmt.Errorf("failed to save state: %w", err)
		}
	}

	// Summary
	fmt.Fprintf(cmd.OutOrStdout(), "\nDone: %d pushed, %d pulled, %d deleted, %d conflicts",
		result.Pushed, result.Pulled, result.Deleted, result.Conflicts)
	if len(result.Errors) > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), ", %d errors", len(result.Errors))
		for _, e := range result.Errors {
			fmt.Fprintf(cmd.ErrOrStderr(), "  error: %v\n", e)
		}
	}
	fmt.Fprintln(cmd.OutOrStdout())

	return nil
}
