package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"edfx-backup/internal/backup"
)

var createCmd = &cobra.Command{
	Use:   "create [target...]",
	Short: "Create local backups of /var/www and /home/maxx and stream them to the EDFX Hetzner S3 bucket",
	Long: `Create compressed .tgz backups of configured targets (/var/www subdirectories and /home/maxx)
and stream them directly to the EDFX S3 bucket on Hetzner Object Storage.

Examples:
  # Back up everything configured in backup-config.yml (/var/www/* + /home/maxx)
  edfx-backup create

  # Back up only /home/maxx
  edfx-backup create --target home-maxx

  # Back up only /var/www projects
  edfx-backup create --group var-www

  # Back up a specific project inside /var/www (or /var/www/edfx-labs)
  edfx-backup create --target ciwg-cli
  edfx-backup create --target backup-utility

  # Dry-run with instant compression estimation and Git repository reference preview
  edfx-backup create --dry-run --estimate-method heuristic --target backup-utility

  # Create backups and prune old backups using smart retention
  edfx-backup create --prune --smart-retention`,
	RunE: runBackupCreate,
}

func init() {
	addS3Flags(createCmd)

	createCmd.Flags().Bool("dry-run", false, "Preview backup actions, exclusions, Git refs, and size estimates without uploading")
	createCmd.Flags().String("estimate-method", "", "Compression estimation method for dry-run: 'heuristic', 'sample', or 'accurate'")
	createCmd.Flags().Int64("sample-size", 100*1024*1024, "Sample size in bytes for 'sample' estimation method (default: 100MB)")

	// Target selection & filtering
	createCmd.Flags().String("config-file", "", "Path to YAML configuration file (default: configs/backup-config.yml)")
	createCmd.Flags().String("config-dir", getEnvWithDefault("BACKUP_CONFIG_DIR", ""), "Directory containing YAML config files")
	createCmd.Flags().StringSliceP("target", "t", nil, "Target name(s), label(s), or directory basename(s) to back up (can be specified multiple times or comma-separated)")
	createCmd.Flags().String("group", "", "Filter by target group name (e.g. 'var-www' or 'home-maxx')")
	createCmd.Flags().StringSlice("path", nil, "Ad-hoc directory path(s) to back up directly")
	createCmd.Flags().StringSlice("exclude", nil, "Additional file/directory exclusion patterns to pass to tar")
	createCmd.Flags().Bool("no-git-refs", false, "Disable generating .git-backup-ref.txt and GIT_REPOS_MANIFEST.txt when .git is excluded")
	createCmd.Flags().Bool("auto-fix-database-tools", getEnvBoolWithDefault("BACKUP_AUTO_FIX_DATABASE_TOOLS", false), "Automatically install database client tools in container if missing (exit code 127)")

	// Capacity & retention flags
	createCmd.Flags().Bool("respect-capacity-limit", getEnvBoolWithDefault("BACKUP_RESPECT_CAPACITY_LIMIT", false), "Check local disk capacity before creating backups")
	createCmd.Flags().Float64("capacity-threshold", getEnvFloat64WithDefault("BACKUP_CAPACITY_THRESHOLD", 95.0), "Local disk usage threshold percentage (default: 95.0)")
	createCmd.Flags().String("storage-check-path", "/", "Local filesystem path to check for capacity limit")

	createCmd.Flags().Bool("prune", false, "After creating backup, prune old backups according to retention settings")
	createCmd.Flags().Int("remainder", 5, "Number of most recent backups to keep when using --prune without smart retention (default: 5)")
	createCmd.Flags().String("retention-policy", getEnvWithDefault("BACKUP_RETENTION_POLICY", ""), "Path to YAML retention policy file")

	// Smart retention flags
	createCmd.Flags().Bool("smart-retention", getEnvBoolWithDefault("BACKUP_SMART_RETENTION", false), "Enable date-aware retention (preserves daily/weekly/monthly backups)")
	createCmd.Flags().Int("keep-daily", getEnvIntWithDefault("BACKUP_KEEP_DAILY", 14), "Daily backups to keep with smart retention (default: 14)")
	createCmd.Flags().Int("keep-weekly", getEnvIntWithDefault("BACKUP_KEEP_WEEKLY", 26), "Weekly backups to keep with smart retention (default: 26)")
	createCmd.Flags().Int("keep-monthly", getEnvIntWithDefault("BACKUP_KEEP_MONTHLY", 6), "Monthly backups to keep with smart retention (default: 6)")
	createCmd.Flags().Int("weekly-day", getEnvIntWithDefault("BACKUP_WEEKLY_DAY", 0), "Day of week for weekly backups, 0=Sunday (default: 0)")
	createCmd.Flags().Int("monthly-day", getEnvIntWithDefault("BACKUP_MONTHLY_DAY", 1), "Day of month for monthly backups (default: 1)")
}

func runBackupCreate(cmd *cobra.Command, args []string) error {
	if err := loadExplicitEnvIfSet(cmd); err != nil {
		return err
	}

	dryRun := mustGetBoolFlag(cmd, "dry-run")
	s3Config, err := getS3Config(cmd, dryRun)
	if err != nil {
		return err
	}

	bm := backup.NewBackupManagerWithS3(s3Config)
	bm.SetVerbosity(resolveVerbosity(cmd))

	// Combine positional args and --target flags (ignoring "localhost" or hostname if passed from ciwg-cli style scripts unless it matches a target)
	targets := mustGetStringSliceFlag(cmd, "target")
	for _, a := range args {
		a = strings.TrimSpace(a)
		if a != "" && a != "localhost" {
			targets = append(targets, a)
		}
	}

	var smartRetention *backup.SmartRetentionPolicy
	if mustGetBoolFlag(cmd, "smart-retention") {
		smartRetention = &backup.SmartRetentionPolicy{
			Enabled:     true,
			KeepDaily:   mustGetIntFlag(cmd, "keep-daily"),
			KeepWeekly:  mustGetIntFlag(cmd, "keep-weekly"),
			KeepMonthly: mustGetIntFlag(cmd, "keep-monthly"),
			WeeklyDay:   mustGetIntFlag(cmd, "weekly-day"),
			MonthlyDay:  mustGetIntFlag(cmd, "monthly-day"),
		}
	}

	configFile := resolveDefaultConfigPath(mustGetStringFlag(cmd, "config-file"))

	options := &backup.BackupOptions{
		DryRun:               dryRun,
		ConfigFile:           configFile,
		ConfigDir:            mustGetStringFlag(cmd, "config-dir"),
		Targets:              targets,
		Group:                mustGetStringFlag(cmd, "group"),
		AdHocPaths:           mustGetStringSliceFlag(cmd, "path"),
		AdditionalExcludes:   mustGetStringSliceFlag(cmd, "exclude"),
		NoGitRefs:            mustGetBoolFlag(cmd, "no-git-refs"),
		RespectCapacityLimit: mustGetBoolFlag(cmd, "respect-capacity-limit"),
		CapacityThreshold:    mustGetFloat64Flag(cmd, "capacity-threshold"),
		StorageCheckPath:     mustGetStringFlag(cmd, "storage-check-path"),
		EstimateMethod:       mustGetStringFlag(cmd, "estimate-method"),
		SampleSize:           mustGetInt64Flag(cmd, "sample-size"),
		SmartRetention:       smartRetention,
		AutoFixDatabaseTools: mustGetBoolFlag(cmd, "auto-fix-database-tools"),
	}

	if configFile != "" {
		fmt.Printf("Using backup config: %s\n", configFile)
	} else {
		fmt.Printf("Using built-in default backup configuration (/var/www + /home/maxx)\n")
	}

	if err := bm.CreateBackups(options); err != nil {
		return err
	}

	prune := mustGetBoolFlag(cmd, "prune")
	if prune {
		return runPruneAfterCreate(cmd, bm, options, smartRetention)
	}

	return nil
}

func runPruneAfterCreate(cmd *cobra.Command, bm *backup.BackupManager, options *backup.BackupOptions, smartRetention *backup.SmartRetentionPolicy) error {
	retentionPolicyFile := mustGetStringFlag(cmd, "retention-policy")
	if retentionPolicyFile != "" {
		fmt.Printf("\n--- Applying Retention Policy from %s ---\n", retentionPolicyFile)
		rulesets, err := backup.LoadRetentionPolicyFromFile(retentionPolicyFile)
		if err != nil {
			return fmt.Errorf("failed to load retention policy: %w", err)
		}
		return applyRetentionPolicy(bm, rulesets, options)
	}

	remainder := mustGetIntFlag(cmd, "remainder")
	if remainder <= 0 {
		remainder = 5
	}

	if smartRetention != nil && smartRetention.Enabled {
		fmt.Printf("\n--- Smart Retention Pruning (daily=%d, weekly=%d, monthly=%d) ---\n",
			smartRetention.KeepDaily, smartRetention.KeepWeekly, smartRetention.KeepMonthly)
		fmt.Printf("Weekly backups: every %s | Monthly backups: day %d of month\n",
			time.Weekday(smartRetention.WeeklyDay), smartRetention.MonthlyDay)
	} else {
		fmt.Printf("\n--- Pruning old backups from Hetzner S3 (keeping %d most recent) ---\n", remainder)
	}

	targets, err := bm.GetTargetsFromOptions(options)
	if err != nil {
		return fmt.Errorf("failed to resolve targets for pruning: %w", err)
	}

	for _, target := range targets {
		prefix := bm.ResolveTargetPrefix(target)
		if options.DryRun {
			fmt.Printf("[DRY RUN] Would evaluate pruning for target %s (prefix: %s)\n", target.Label, prefix)
			continue
		}

		objs, err := bm.ListBackups(prefix, 0)
		if err != nil {
			fmt.Printf("Warning: failed to list backups for %s: %v\n", target.Label, err)
			continue
		}

		var toDelete []backup.ObjectInfo
		if smartRetention != nil && smartRetention.Enabled {
			toDelete = bm.SelectObjectsWithSmartRetention(objs, smartRetention)
			if len(toDelete) == 0 {
				fmt.Printf("Target %s: Found %d backup(s), all preserved by retention policy\n", target.Label, len(objs))
				continue
			}
			fmt.Printf("Target %s: Found %d backup(s), deleting %d older backup(s)\n", target.Label, len(objs), len(toDelete))
		} else {
			if len(objs) <= remainder {
				fmt.Printf("Target %s: Found %d backup(s), keeping all\n", target.Label, len(objs))
				continue
			}
			toDelete = bm.SelectObjectsForOverwrite(objs, remainder)
			if len(toDelete) == 0 {
				continue
			}
			fmt.Printf("Target %s: Found %d backup(s), keeping %d most recent, deleting %d older backup(s)\n",
				target.Label, len(objs), remainder, len(toDelete))
		}

		var deleteKeys []string
		for _, o := range toDelete {
			deleteKeys = append(deleteKeys, o.Key)
		}

		if err := bm.DeleteObjects(deleteKeys); err != nil {
			fmt.Printf("Warning: failed to delete old S3 backups for %s: %v\n", target.Label, err)
		} else {
			fmt.Printf("✓ Cleaned up %d old backup(s) for %s\n", len(deleteKeys), target.Label)
		}
	}

	return nil
}

func applyRetentionPolicy(bm *backup.BackupManager, rulesets map[string]backup.RetentionRuleset, options *backup.BackupOptions) error {
	targets, err := bm.GetTargetsFromOptions(options)
	if err != nil {
		return fmt.Errorf("failed to resolve targets: %w", err)
	}

	for _, target := range targets {
		prefix := bm.ResolveTargetPrefix(target)
		fmt.Printf("\nProcessing retention for %s (prefix: %s)\n", target.Label, prefix)

		if options.DryRun {
			fmt.Printf("  [DRY RUN] Would list and evaluate retention rulesets for %s\n", prefix)
			continue
		}

		objs, err := bm.ListBackups(prefix, 0)
		if err != nil {
			fmt.Printf("  ⚠️  Failed to list backups: %v\n", err)
			continue
		}
		if len(objs) == 0 {
			fmt.Printf("  No backups found\n")
			continue
		}

		toDelete := bm.ApplyRetentionPolicy(objs, rulesets)
		fmt.Printf("  Retention decision: %d to delete, %d to keep\n", len(toDelete), len(objs)-len(toDelete))

		if len(toDelete) > 0 {
			var keys []string
			for _, obj := range toDelete {
				keys = append(keys, obj.Key)
			}
			if err := bm.DeleteObjects(keys); err != nil {
				fmt.Printf("  ⚠️  Failed to delete backups: %v\n", err)
			} else {
				fmt.Printf("  ✓ Deleted %d backup(s)\n", len(toDelete))
			}
		}
	}

	return nil
}
