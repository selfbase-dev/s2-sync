package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/selfbase-dev/s2-cli/internal/auth"
	"github.com/selfbase-dev/s2-cli/internal/client"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate with S2",
	Long:  "Store an S2 API token for future use. Get your token from the S2 dashboard.",
	RunE:  runLogin,
}

func init() {
	rootCmd.AddCommand(loginCmd)
}

func runLogin(cmd *cobra.Command, args []string) error {
	fmt.Fprint(cmd.OutOrStdout(), "Enter your S2 token (s2_...): ")

	reader := bufio.NewReader(os.Stdin)
	token, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed to read token: %w", err)
	}
	token = strings.TrimSpace(token)

	if !strings.HasPrefix(token, "s2_") {
		return fmt.Errorf("invalid token: must start with s2_")
	}

	// Validate token by calling /api/me
	endpoint := viper.GetString("endpoint")
	c := client.New(endpoint, token)
	me, err := c.Me()
	if err != nil {
		return fmt.Errorf("token validation failed: %w", err)
	}

	// Store in keyring
	if err := auth.SetKeyring(token); err != nil {
		// Fallback: store in config file
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: keyring unavailable (%v), storing in config file\n", err)
		viper.Set("token", token)
		dir, err := ensureConfigDir()
		if err != nil {
			return err
		}
		if err := viper.WriteConfigAs(dir + "/config.toml"); err != nil {
			return fmt.Errorf("failed to save config: %w", err)
		}
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Login successful! (token: %s, user: %s)\n", me.TokenID, me.UserID)
	return nil
}

func ensureConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := home + "/.config/s2"
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return dir, nil
}
