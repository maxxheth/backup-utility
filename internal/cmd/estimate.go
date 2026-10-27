package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"edfx-backup/internal/backup"
)

var estimateCapacityCmd = &cobra.Command{
	Use:   "estimate-capacity [target...]",
	Short: "Estimate compressed backup sizes and S3 storage requirements for /var/www and /home/maxx",
	Long: `Scan configured backup targets (/var/www and /home/maxx, or filtered targets)
with exclusions applied and estimate compressed archive sizes, retention storage requirements,
growth projections, and monthly S3 storage costs.

Examples:
  # Estimate capacity for a specific project using instant heuristic analysis
  edfx-backup estimate-capacity --target backup-utility --estimate-method heuristic

  # Estimate capacity for all /var/www projects
  edfx-backup estimate-capacity --group var-www --estimate-method heuristic

  # Estimate capacity for /home/maxx
  edfx-backup estimate-capacity --target home-maxx --estimate-method heuristic`,
	RunE: runEstimateCapacity,
}

func init() {
	addS3Flags(estimateCapacityCmd)
	estimateCapacityCmd.Flags().String("config-file", "", "Path to YAML configuration file")
	estimateCapacityCmd.Flags().String("config-dir", "", "Directory containing YAML config files")
	estimateCapacityCmd.Flags().StringSliceP("target", "t", nil, "Target name(s) or label(s) to scan")
	estimateCapacityCmd.Flags().String("group", "", "Target group to scan (e.g. 'var-www' or 'home-maxx')")
	estimateCapacityCmd.Flags().StringSlice("path", nil, "Ad-hoc directory path(s) to scan")
	estimateCapacityCmd.Flags().StringSlice("exclude", nil, "Additional file/directory exclusion patterns")

	estimateCapacityCmd.Flags().String("estimate-method", "heuristic", "Compression estimation method: 'heuristic', 'sample', or 'accurate'")
	estimateCapacityCmd.Flags().Int64("sample-size", 100*1024*1024, "Sample size in bytes for 'sample' estimation method (default: 100MB)")

	estimateCapacityCmd.Flags().Int("daily-retention", getEnvIntWithDefault("BACKUP_KEEP_DAILY", 14), "Number of daily backups to retain (default: 14)")
	estimateCapacityCmd.Flags().Int("weekly-retention", getEnvIntWithDefault("BACKUP_KEEP_WEEKLY", 26), "Number of weekly backups to retain (default: 26)")
	estimateCapacityCmd.Flags().Int("monthly-retention", getEnvIntWithDefault("BACKUP_KEEP_MONTHLY", 6), "Number of monthly backups to retain (default: 6)")

	estimateCapacityCmd.Flags().Float64("growth-rate", 0, "Monthly growth rate percentage for projections (e.g., 5 for 5%)")
	estimateCapacityCmd.Flags().Int("projection-months", 12, "Number of months to project growth (default: 12)")
	estimateCapacityCmd.Flags().Float64("buffer-percent", 20, "Safety buffer percentage (default: 20%)")
	estimateCapacityCmd.Flags().Float64("s3-price-per-gb", 0.005, "Hetzner S3 price per GB per month (default: $0.005)")
	estimateCapacityCmd.Flags().String("output", "stdout", "Output format: 'stdout' or 'json'")
}

func runEstimateCapacity(cmd *cobra.Command, args []string) error {
	if err := loadExplicitEnvIfSet(cmd); err != nil {
		return err
	}

	s3Config, _ := getS3Config(cmd, true)
	bm := backup.NewBackupManagerWithS3(s3Config)
	bm.SetVerbosity(resolveVerbosity(cmd))

	targets := mustGetStringSliceFlag(cmd, "target")
	for _, a := range args {
		if a = strings.TrimSpace(a); a != "" {
			targets = append(targets, a)
		}
	}

	configFile := resolveDefaultConfigPath(mustGetStringFlag(cmd, "config-file"))
	options := &backup.BackupOptions{
		DryRun:             true,
		ConfigFile:         configFile,
		ConfigDir:          mustGetStringFlag(cmd, "config-dir"),
		Targets:            targets,
		Group:              mustGetStringFlag(cmd, "group"),
		AdHocPaths:         mustGetStringSliceFlag(cmd, "path"),
		AdditionalExcludes: mustGetStringSliceFlag(cmd, "exclude"),
	}

	resolvedTargets, err := bm.GetTargetsFromOptions(options)
	if err != nil {
		return err
	}

	estOpts := &backup.CapacityEstimateOptions{
		DailyRetention:   mustGetIntFlag(cmd, "daily-retention"),
		WeeklyRetention:  mustGetIntFlag(cmd, "weekly-retention"),
		MonthlyRetention: mustGetIntFlag(cmd, "monthly-retention"),
		GrowthRate:       mustGetFloat64Flag(cmd, "growth-rate"),
		ProjectionMonths: mustGetIntFlag(cmd, "projection-months"),
		BufferPercent:    mustGetFloat64Flag(cmd, "buffer-percent"),
		S3PricePerGB:     mustGetFloat64Flag(cmd, "s3-price-per-gb"),
	}

	result, err := bm.EstimateCapacityFromScan(
		resolvedTargets,
		mustGetStringFlag(cmd, "estimate-method"),
		mustGetInt64Flag(cmd, "sample-size"),
		estOpts,
	)
	if err != nil {
		return err
	}

	if strings.EqualFold(mustGetStringFlag(cmd, "output"), "json") {
		b, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}

	fmt.Println("======================================================================")
	fmt.Println("📊 EDFX S3 STORAGE CAPACITY ESTIMATE")
	fmt.Println("======================================================================")
	fmt.Printf("Targets Scanned:         %d\n", result.SitesScanned)
	fmt.Printf("Estimation Method:       %s\n", result.EstimationMethod)
	fmt.Printf("Retention Policy:        %d daily + %d weekly + %d monthly (%d backups/target)\n",
		result.DailyRetention, result.WeeklyRetention, result.MonthlyRetention, result.TotalBackupsPerSite)
	fmt.Printf("Avg Uncompressed/Target: %s\n", formatBytes(result.AvgUncompressedSize))
	fmt.Printf("Avg Compressed/Target:   %s (%.1f%% space saved)\n", formatBytes(result.AvgCompressedSize), result.AvgCompressionRatio)
	fmt.Println("----------------------------------------------------------------------")
	fmt.Printf("Single Full Backup Run:  %s\n", formatBytes(result.FleetTotalStorage/int64(maxInt(result.TotalBackupsPerSite, 1))))
	fmt.Printf("Total Retained Storage:  %s\n", formatBytes(result.FleetTotalStorage))
	fmt.Printf("With %.0f%% Safety Buffer:  %s\n", result.BufferPercent, formatBytes(result.FleetTotalWithBuffer))
	if result.MonthlyCost > 0 {
		fmt.Printf("Estimated Monthly Cost:  $%.2f / month\n", result.MonthlyCost)
	}
	fmt.Println("======================================================================")

	return nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
