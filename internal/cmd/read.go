package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"edfx-backup/internal/backup"
)

var readCmd = &cobra.Command{
	Use:   "read [object]",
	Short: "Read/download a backup object from the Hetzner EDFX S3 bucket",
	Long:  `Download or stream a backup object from Hetzner S3. If --output is not specified and --save is not set, the object is written to stdout.`,
	Args:  cobra.MaximumNArgs(1),
	RunE:  runBackupRead,
}

func init() {
	addS3Flags(readCmd)
	readCmd.Flags().String("output", "", "Output file path (if empty, writes to stdout unless --save is set)")
	readCmd.Flags().Bool("save", false, "Save backup object to current working directory using its basename")
	readCmd.Flags().String("prefix", "", "Prefix to search for when using --latest (e.g. var-www/ciwg-cli/)")
	readCmd.Flags().Bool("latest", false, "Resolve the most recent object matching --prefix when object argument is omitted")
}

func runBackupRead(cmd *cobra.Command, args []string) error {
	if err := loadExplicitEnvIfSet(cmd); err != nil {
		return err
	}

	s3Config, err := getS3Config(cmd, false)
	if err != nil {
		return err
	}

	bm := backup.NewBackupManagerWithS3(s3Config)
	bm.SetVerbosity(resolveVerbosity(cmd))

	var objectName string
	if len(args) > 0 {
		objectName = args[0]
	}

	if objectName == "" {
		latest := mustGetBoolFlag(cmd, "latest")
		prefix := mustGetStringFlag(cmd, "prefix")
		if latest && prefix != "" {
			latestObj, err := bm.GetLatestObject(prefix)
			if err != nil {
				return fmt.Errorf("failed to resolve latest object for prefix '%s': %w", prefix, err)
			}
			objectName = latestObj
			fmt.Printf("Resolved latest object: %s\n", objectName)
		} else {
			return fmt.Errorf("object name argument is required unless --latest and --prefix are used")
		}
	}

	outputPath := mustGetStringFlag(cmd, "output")
	if mustGetBoolFlag(cmd, "save") && outputPath == "" {
		outputPath = filepath.Base(objectName)
	}

	return bm.ReadBackup(objectName, outputPath)
}
