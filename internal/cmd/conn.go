package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"edfx-backup/internal/backup"
)

var connCmd = &cobra.Command{
	Use:   "conn",
	Short: "Test connection and read/write permissions to the Hetzner EDFX S3 bucket",
	Long:  `Verify connectivity, bucket existence, and read/write/delete permissions for the EDFX S3 bucket on Hetzner Object Storage.`,
	Args:  cobra.NoArgs,
	RunE:  runTestS3,
}

var testS3Cmd = &cobra.Command{
	Use:   "test-s3",
	Short: "Test Hetzner S3 connection or run a simulated payload round-trip",
	Long:  `Test the connection to Hetzner S3 storage and perform a read/write test or a full SHA256 round-trip simulation (--simulate).`,
	Args:  cobra.NoArgs,
	RunE:  runTestS3,
}

func init() {
	addS3Flags(connCmd)
	connCmd.Flags().Bool("simulate", false, "Run a full S3 round-trip simulation (random payload, SHA256 checksum, list, download, verify)")
	connCmd.Flags().Bool("simulate-keep", false, "Keep simulated object instead of deleting it (only with --simulate)")
	connCmd.Flags().Int("simulate-max-mb", 25, "Maximum payload size in MB for simulate mode (default 25, min 1)")

	addS3Flags(testS3Cmd)
	testS3Cmd.Flags().Bool("simulate", false, "Run a full S3 round-trip simulation (random payload, SHA256 checksum, list, download, verify)")
	testS3Cmd.Flags().Bool("simulate-keep", false, "Keep simulated object instead of deleting it (only with --simulate)")
	testS3Cmd.Flags().Int("simulate-max-mb", 25, "Maximum payload size in MB for simulate mode (default 25, min 1)")
}

func runTestS3(cmd *cobra.Command, args []string) error {
	if err := loadExplicitEnvIfSet(cmd); err != nil {
		return err
	}

	s3Config, err := getS3Config(cmd, false)
	if err != nil {
		return err
	}

	fmt.Println("===========================================")
	fmt.Println("Testing Hetzner EDFX S3 Connection")
	fmt.Println("===========================================")
	fmt.Printf("Endpoint: %s\n", s3Config.Endpoint)
	fmt.Printf("Bucket:   %s\n", s3Config.Bucket)
	fmt.Printf("Region:   %s\n", s3Config.Region)
	fmt.Printf("Use SSL:  %v\n\n", s3Config.UseSSL)

	bm := backup.NewBackupManagerWithS3(s3Config)
	bm.SetVerbosity(resolveVerbosity(cmd))

	if mustGetBoolFlag(cmd, "simulate") {
		keep := mustGetBoolFlag(cmd, "simulate-keep")
		maxMB := mustGetIntFlag(cmd, "simulate-max-mb")
		fmt.Printf("Running S3 simulate round-trip (max %d MB)...\n\n", maxMB)
		if err := bm.SimulateS3RoundTrip(context.Background(), keep, maxMB); err != nil {
			return fmt.Errorf("Hetzner S3 simulate round-trip failed: %w", err)
		}
		fmt.Println("✓ Hetzner S3 simulate round-trip successful!")
		return nil
	}

	if err := bm.TestS3Connection(); err != nil {
		return fmt.Errorf("Hetzner S3 connection test failed: %w", err)
	}

	fmt.Println("\n✓ Hetzner EDFX S3 connection test successful!")
	return nil
}
