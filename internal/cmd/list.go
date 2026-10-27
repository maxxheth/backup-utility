package cmd

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"edfx-backup/internal/backup"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List backup objects in the Hetzner EDFX S3 bucket",
	Long:  `List backup objects in the configured Hetzner S3 bucket, optionally filtered by prefix.`,
	Args:  cobra.NoArgs,
	RunE:  runBackupList,
}

func init() {
	addS3Flags(listCmd)
	listCmd.Flags().String("prefix", "", "Prefix to filter listed objects (e.g. var-www/ or home-maxx/)")
	listCmd.Flags().Int("limit", 100, "Maximum number of objects to list (0 = unlimited)")
	listCmd.Flags().Bool("json", false, "Output JSON")
}

func runBackupList(cmd *cobra.Command, args []string) error {
	if err := loadExplicitEnvIfSet(cmd); err != nil {
		return err
	}

	s3Config, err := getS3Config(cmd, false)
	if err != nil {
		return err
	}

	bm := backup.NewBackupManagerWithS3(s3Config)
	bm.SetVerbosity(resolveVerbosity(cmd))

	prefix := mustGetStringFlag(cmd, "prefix")
	limit := mustGetIntFlag(cmd, "limit")

	objs, err := bm.ListBackups(prefix, limit)
	if err != nil {
		return fmt.Errorf("failed to list backups: %w", err)
	}

	if len(objs) == 0 {
		fmt.Println("No objects found")
		return nil
	}

	if mustGetBoolFlag(cmd, "json") {
		b, err := json.MarshalIndent(objs, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal objects to JSON: %w", err)
		}
		fmt.Println(string(b))
		return nil
	}

	for _, o := range objs {
		fmt.Printf("%s\t%s (%d B)\t%s\n", o.Key, formatBytes(o.Size), o.Size, o.LastModified.Format(time.RFC3339))
	}

	return nil
}
