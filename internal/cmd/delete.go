package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"edfx-backup/internal/backup"
)

var deleteCmd = &cobra.Command{
	Use:   "delete [object]",
	Short: "Delete backup object(s) from the Hetzner EDFX S3 bucket",
	Long: `Delete one or more backup objects from the configured Hetzner S3 bucket.

Deletion modes (mutually exclusive):
  - Single object: Pass object key as argument
  - By prefix: Use --prefix to delete objects matching a prefix
  - Latest: Use --latest with --prefix to delete only the most recent match
  - Delete all: Use --delete-all to delete all objects (respects --prefix)
  - Numeric range: Use --delete-range "1-10" to delete the 1st through 10th most recent backups
  - Date range: Use --delete-range-by-date "YYYYMMDD-YYYYMMDD"`,
	Args: cobra.MaximumNArgs(1),
	RunE: runBackupDelete,
}

func init() {
	addS3Flags(deleteCmd)
	deleteCmd.Flags().Bool("dry-run", false, "Preview deletions without performing them")
	deleteCmd.Flags().String("prefix", "", "Prefix to select objects to delete (e.g. var-www/ciwg-cli/)")
	deleteCmd.Flags().Int("limit", 100, "Maximum number of objects to consider when using --prefix")
	deleteCmd.Flags().Bool("latest", false, "If set with --prefix, delete only the most recent object matching --prefix")
	deleteCmd.Flags().Bool("delete-all", false, "Delete all backups (respects --prefix if provided)")
	deleteCmd.Flags().String("delete-range", "", "Delete backups by numeric range (e.g., '1-10' for 1st through 10th most recent)")
	deleteCmd.Flags().String("delete-range-by-date", "", "Delete backups by date range (YYYYMMDD-YYYYMMDD or YYYYMMDD:HHMMSS-YYYYMMDD:HHMMSS)")
	deleteCmd.Flags().Bool("skip-confirmation", false, "Skip interactive confirmation prompt")
}

func runBackupDelete(cmd *cobra.Command, args []string) error {
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

	prefix := mustGetStringFlag(cmd, "prefix")
	latest := mustGetBoolFlag(cmd, "latest")
	deleteAll := mustGetBoolFlag(cmd, "delete-all")
	deleteRange := mustGetStringFlag(cmd, "delete-range")
	deleteRangeByDate := mustGetStringFlag(cmd, "delete-range-by-date")
	skipConfirm := mustGetBoolFlag(cmd, "skip-confirmation")
	dryRun := mustGetBoolFlag(cmd, "dry-run")

	flagCount := 0
	if objectName != "" {
		flagCount++
	}
	if latest {
		flagCount++
	}
	if deleteAll {
		flagCount++
	}
	if deleteRange != "" {
		flagCount++
	}
	if deleteRangeByDate != "" {
		flagCount++
	}
	if flagCount > 1 {
		return fmt.Errorf("only one of: object argument, --latest, --delete-all, --delete-range, or --delete-range-by-date can be specified")
	}

	var toDelete []string
	if objectName != "" {
		toDelete = append(toDelete, objectName)
	} else if prefix != "" || deleteAll || deleteRange != "" || deleteRangeByDate != "" {
		limit := 0
		if !deleteAll && deleteRange == "" && deleteRangeByDate == "" {
			limit = mustGetIntFlag(cmd, "limit")
		}
		objs, err := bm.ListBackups(prefix, limit)
		if err != nil {
			return fmt.Errorf("failed to list objects for prefix '%s': %w", prefix, err)
		}
		if len(objs) == 0 {
			fmt.Println("No objects found for prefix")
			return nil
		}

		if latest {
			latestKey := objs[0].Key
			latestTime := objs[0].LastModified
			for _, o := range objs[1:] {
				if o.LastModified.After(latestTime) {
					latestKey = o.Key
					latestTime = o.LastModified
				}
			}
			toDelete = append(toDelete, latestKey)
		} else if deleteAll {
			for _, o := range objs {
				toDelete = append(toDelete, o.Key)
			}
		} else if deleteRange != "" {
			start, end, err := bm.ParseNumericRange(deleteRange)
			if err != nil {
				return fmt.Errorf("invalid --delete-range format: %w", err)
			}
			selected, err := bm.SelectObjectsByNumericRange(objs, start, end)
			if err != nil {
				return fmt.Errorf("failed to select objects by range: %w", err)
			}
			for _, o := range selected {
				toDelete = append(toDelete, o.Key)
			}
		} else if deleteRangeByDate != "" {
			startTime, endTime, err := bm.ParseDateRange(deleteRangeByDate)
			if err != nil {
				return fmt.Errorf("invalid --delete-range-by-date format: %w", err)
			}
			filtered := bm.FilterObjectsByDateRange(objs, startTime, endTime)
			for _, o := range filtered {
				toDelete = append(toDelete, o.Key)
			}
		} else {
			for _, o := range objs {
				toDelete = append(toDelete, o.Key)
			}
		}
	} else {
		return fmt.Errorf("object name argument or --prefix is required")
	}

	if dryRun {
		fmt.Printf("Dry run: %d object(s) would be deleted:\n", len(toDelete))
		for _, k := range toDelete {
			fmt.Println(" - ", k)
		}
		return nil
	}

	if !skipConfirm {
		fmt.Printf("About to delete %d object(s). Continue? [y/N]: ", len(toDelete))
		var resp string
		if _, err := fmt.Scanln(&resp); err != nil {
			return fmt.Errorf("confirmation failed: %w", err)
		}
		resp = strings.TrimSpace(strings.ToLower(resp))
		if resp != "y" && resp != "yes" {
			fmt.Println("Aborted by user")
			return nil
		}
	}

	if err := bm.DeleteObjects(toDelete); err != nil {
		return fmt.Errorf("failed to delete objects: %w", err)
	}

	fmt.Printf("Deleted %d object(s)\n", len(toDelete))
	return nil
}
