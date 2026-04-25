package cmd

import (
	"fmt"

	"github.com/selfbase-dev/s2-sync/internal/auth"
	"github.com/selfbase-dev/s2-sync/internal/client"
	"github.com/selfbase-dev/s2-sync/internal/installation"
	"github.com/selfbase-dev/s2-sync/internal/oauth"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate with S2",
	Long:  "Sign in with your S2 account via OAuth. Opens your browser to complete consent.",
	RunE:  runLogin,
}

func init() {
	rootCmd.AddCommand(loginCmd)
}

func runLogin(cmd *cobra.Command, args []string) error {
	endpoint := viper.GetString("endpoint")
	fmt.Fprintf(cmd.OutOrStdout(), "Opening %s in your browser to sign in...\n", endpoint)

	inst, err := installation.LoadOrCreate()
	if err != nil {
		return fmt.Errorf("load installation: %w", err)
	}
	tr, err := oauth.Login(cmd.Context(), endpoint, oauth.LoginOpts{
		InstallationID: inst.InstallationID,
		DeviceLabel:    inst.DeviceLabel,
	})
	if err != nil {
		return fmt.Errorf("sign-in failed: %w", err)
	}

	if err := auth.SaveSession(endpoint, tr); err != nil {
		return fmt.Errorf("save session: %w", err)
	}

	// Verify by hitting /api/me with the freshly-issued access token.
	source, err := auth.NewSource(endpoint)
	if err != nil {
		return err
	}
	me, err := client.New(endpoint, source).Me()
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Signed in as %s (token %s)\n", me.UserID, me.TokenID)
	return nil
}
