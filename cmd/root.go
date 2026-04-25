package cmd

import (
	"fmt"
	"io"
	"log/slog"
	"os"

	slog2 "github.com/selfbase-dev/s2-sync/internal/log"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	logFormat string
	logLevel  string
	logFile   string

	rootLogger *slog.Logger
	logCloser  io.Closer
)

var rootCmd = &cobra.Command{
	Use:               "s2",
	Short:             "S2 file sync CLI",
	Long:              "Sync local files with S2 remote storage.",
	PersistentPreRunE: setupLogger,
	PersistentPostRun: closeLogger,
}

func Execute() error {
	return rootCmd.Execute()
}

func init() {
	cobra.OnInitialize(initConfig)
	rootCmd.PersistentFlags().String("endpoint", "", "S2 endpoint URL (default: https://scopeds.dev)")
	rootCmd.PersistentFlags().String("token", "", "S2 API token (overrides keyring/config)")
	rootCmd.PersistentFlags().StringVar(&logFormat, "log-format", "text", "Log format: text|json")
	rootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "info", "Log level: debug|info|warn|error")
	rootCmd.PersistentFlags().StringVar(&logFile, "log-file", slog2.DefaultLogPath(), "JSON Lines log file (empty disables)")
	viper.BindPFlag("endpoint", rootCmd.PersistentFlags().Lookup("endpoint"))
	viper.BindPFlag("token", rootCmd.PersistentFlags().Lookup("token"))
}

func setupLogger(cmd *cobra.Command, _ []string) error {
	logger, closer, err := slog2.CLILogger(
		os.Stderr,
		slog2.CLIFormat(logFormat),
		slog2.ParseLevel(logLevel),
		logFile,
	)
	if err != nil {
		return fmt.Errorf("logger setup: %w", err)
	}
	slog.SetDefault(logger)
	rootLogger = logger
	logCloser = closer
	return nil
}

func closeLogger(_ *cobra.Command, _ []string) {
	if logCloser != nil {
		_ = logCloser.Close()
	}
}

// Logger returns the configured root logger. Always non-nil after
// PersistentPreRunE has run.
func Logger() *slog.Logger {
	if rootLogger == nil {
		return slog.Default()
	}
	return rootLogger
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
