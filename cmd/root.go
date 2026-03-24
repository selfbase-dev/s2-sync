package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var rootCmd = &cobra.Command{
	Use:   "s2",
	Short: "S2 file sync CLI",
	Long:  "Sync local files with S2 remote storage.",
}

func Execute() error {
	return rootCmd.Execute()
}

func init() {
	cobra.OnInitialize(initConfig)
	rootCmd.PersistentFlags().String("endpoint", "", "S2 endpoint URL (default: https://scopeds.dev)")
	rootCmd.PersistentFlags().String("token", "", "S2 API token (overrides keyring/config)")
	viper.BindPFlag("endpoint", rootCmd.PersistentFlags().Lookup("endpoint"))
	viper.BindPFlag("token", rootCmd.PersistentFlags().Lookup("token"))
}

func initConfig() {
	viper.SetConfigName("config")
	viper.SetConfigType("toml")
	viper.AddConfigPath("$HOME/.config/s2")
	viper.SetEnvPrefix("S2")
	viper.AutomaticEnv()
	viper.SetDefault("endpoint", "https://scopeds.dev")

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			fmt.Fprintf(rootCmd.ErrOrStderr(), "Warning: config file error: %v\n", err)
		}
	}
}
