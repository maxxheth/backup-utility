package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/joho/godotenv"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "edfx-backup",
	Short: "EDFX Local Backup Utility for /var/www and /home/maxx to Hetzner S3",
	Long: `edfx-backup is a local backup utility derived from ciwg-cli, designed specifically
for streaming compressed backups of /var/www (per-project subdirectories) and /home/maxx
to the EDFX S3 bucket on Hetzner Object Storage.

Features:
  • Hybrid packaging: per-subdirectory archives for /var/www/* + single archive for /home/maxx
  • Streaming tar-to-S3 uploads with zero local temporary tarball storage
  • Automatic Git Reference Artifact generation (.git-backup-ref.txt & GIT_REPOS_MANIFEST.txt)
    when .git directories are excluded
  • Optional pre/post shell hooks and Docker database dumps (Postgres, MySQL, MariaDB, MongoDB, WP)
  • Smart retention policies (daily/weekly/monthly) and simple remainder pruning`,
	Version: "1.0.0",
}

// Execute runs the root command
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	loadInitialEnv()

	rootCmd.PersistentFlags().String("env", "", "Path to .env file to load (overrides defaults)")
	rootCmd.PersistentFlags().CountP("vflag", "v", "Increase verbosity (-v=verbose, -vv=debug, -vvv=trace)")
	rootCmd.PersistentFlags().Int("log-level", getEnvIntWithDefault("BACKUP_LOG_LEVEL", 1), "Logging level: 0=quiet, 1=normal, 2=verbose, 3=debug, 4=trace")

	rootCmd.AddCommand(createCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(readCmd)
	rootCmd.AddCommand(deleteCmd)
	rootCmd.AddCommand(connCmd)
	rootCmd.AddCommand(testS3Cmd)
	rootCmd.AddCommand(estimateCapacityCmd)
	rootCmd.AddCommand(gitRefsCmd)
}

// loadInitialEnv loads .env files in priority order before flags are evaluated
func loadInitialEnv() {
	if envPath := findEnvArg(os.Args); envPath != "" {
		_ = godotenv.Load(envPath)
		return
	}

	candidates := []string{
		".env",
		"/var/www/edfx-labs/backup-utility/.env",
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".edfx-backup.env"))
	}
	// Fallback to /var/www/ciwg-cli/.env so existing Hetzner S3 credentials work out of the box
	candidates = append(candidates, "/var/www/ciwg-cli/.env")

	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			_ = godotenv.Load(c)
		}
	}
}

func loadExplicitEnvIfSet(cmd *cobra.Command) error {
	if envPath := mustGetStringFlag(cmd, "env"); envPath != "" {
		if err := godotenv.Overload(envPath); err != nil {
			return fmt.Errorf("failed to load env file '%s': %w", envPath, err)
		}
	}
	return nil
}

func resolveVerbosity(cmd *cobra.Command) int {
	logLevel := mustGetIntFlag(cmd, "log-level")
	vflag := mustGetCountFlag(cmd, "vflag")
	if vflag > 0 {
		return 1 + vflag
	}
	return logLevel
}
