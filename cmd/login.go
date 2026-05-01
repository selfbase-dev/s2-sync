package cmd

import (
	"fmt"

	"github.com/selfbase-dev/s2-sync/internal/auth"
	"github.com/selfbase-dev/s2-sync/internal/client"
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

	// Reuse a previously-registered client_id when present; otherwise
	// Login will perform Dynamic Client Registration to obtain one.
	clientID := ""
	if sess, err := auth.LoadSession(); err == nil {
		clientID = sess.ClientID
	}

	res, err := oauth.Login(cmd.Context(), endpoint, clientID)
	if err != nil {
		return fmt.Errorf("sign-in failed: %w", err)
	}

	if err := auth.SaveSession(endpoint, res.ClientID, res.Tokens); err != nil {
		return fmt.Errorf("save session: %w", err)
	}

	// Verify by hitting /api/v1/token with the freshly-issued access token.
	source, err := auth.NewSource(endpoint)
	if err != nil {
		return err
	}
	ti, err := client.New(endpoint, source).Introspect()
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Signed in as %s (token %s)\n", ti.UserID, ti.TokenID)
	return nil
}
