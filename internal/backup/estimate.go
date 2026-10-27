package backup

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// countingWriter counts bytes written but discards the data
type countingWriter struct {
	written int64
}

func (w *countingWriter) Write(p []byte) (n int, err error) {
	n = len(p)
	w.written += int64(n)
	return n, nil
}

// StorageCapacity represents local disk usage statistics
type StorageCapacity struct {
	Total       uint64  `json:"total"`
	Used        uint64  `json:"used"`
	Available   uint64  `json:"available"`
	UsedPercent float64 `json:"used_percent"`
	Path        string  `json:"path"`
}

// CapacityEstimateOptions contains parameters for capacity estimation
type CapacityEstimateOptions struct {
	DailyRetention   int
	WeeklyRetention  int
	MonthlyRetention int

	GrowthRate       float64
	ProjectionMonths int
	BufferPercent    float64

	S3PricePerGB        float64
	RetrievalPricePerGB float64

	AvgCompressedSize int64
	SiteCount         int
}

// SiteEstimate represents capacity estimates for a single target/site
type SiteEstimate struct {
	SiteName         string  `json:"site_name"`
	UncompressedSize int64   `json:"uncompressed_size"`
	CompressedSize   int64   `json:"compressed_size"`
	CompressionRatio float64 `json:"compression_ratio"`
	HotStorageSize   int64   `json:"hot_storage_size"`
	ColdStorageSize  int64   `json:"cold_storage_size"`
	TotalStorageSize int64   `json:"total_storage_size"`
}

// GrowthProjection represents storage projections at a specific month
type GrowthProjection struct {
	Month          int     `json:"month"`
	TotalStorageGB float64 `json:"total_storage_gb"`
	HotStorageGB   float64 `json:"hot_storage_gb"`
	ColdStorageGB  float64 `json:"cold_storage_gb"`
	MonthlyCost    float64 `json:"monthly_cost,omitempty"`
}

// CapacityEstimate represents the complete capacity estimation results
type CapacityEstimate struct {
	EstimationMethod string `json:"estimation_method"`
	SitesScanned     int    `json:"sites_scanned"`

	DailyRetention      int `json:"daily_retention"`
	WeeklyRetention     int `json:"weekly_retention"`
	MonthlyRetention    int `json:"monthly_retention"`
	TotalBackupsPerSite int `json:"total_backups_per_site"`

	AvgUncompressedSize int64   `json:"avg_uncompressed_size"`
	AvgCompressedSize   int64   `json:"avg_compressed_size"`
	AvgCompressionRatio float64 `json:"avg_compression_ratio"`

	PerSiteHotStorage   int64 `json:"per_site_hot_storage"`
	PerSiteColdStorage  int64 `json:"per_site_cold_storage"`
	PerSiteTotalStorage int64 `json:"per_site_total_storage"`

	FleetHotStorage   int64 `json:"fleet_hot_storage"`
	FleetColdStorage  int64 `json:"fleet_cold_storage"`
	FleetTotalStorage int64 `json:"fleet_total_storage"`

	FleetTotalWithBuffer int64   `json:"fleet_total_with_buffer"`
	BufferPercent        float64 `json:"buffer_percent"`

	GrowthProjections []GrowthProjection `json:"growth_projections,omitempty"`

	MonthlyCost        float64 `json:"monthly_cost,omitempty"`
	RetrievalCost10Pct float64 `json:"retrieval_cost_10pct,omitempty"`

	Sites []SiteEstimate `json:"sites,omitempty"`
}

// DefaultExcludes are always excluded from tar archives unless already specified
var DefaultArchiveExcludes = []string{
	"*.tgz",
	"*.tar.gz",
	"*.zip",
}

// BuildTarExcludeFlags returns shell-escaped `--exclude='...'` flags for tar/du commands
func BuildTarExcludeFlags(excludes []string) string {
	merged := MergeUniqueStrings(DefaultArchiveExcludes, excludes)
	var parts []string
	for _, ex := range merged {
		ex = strings.TrimSpace(ex)
		ex = strings.TrimSuffix(ex, "/")
		if ex == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("--exclude=%s", shellEscape(ex)))
		// Also exclude ./<pattern> so tar -C <dir> . matches root-anchored relative paths like .config/google-chrome
		if strings.Contains(ex, "/") && !strings.HasPrefix(ex, "./") && !strings.HasPrefix(ex, "/") && !strings.HasPrefix(ex, "*") {
			parts = append(parts, fmt.Sprintf("--exclude=%s", shellEscape("./"+ex)))
		}
	}
	return strings.Join(parts, " ")
}

// GetStorageCapacity checks local disk usage for the given path using syscall.Statfs
func (bm *BackupManager) GetStorageCapacity(path string) (*StorageCapacity, error) {
	if path == "" {
		path = "/"
	}

	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return nil, fmt.Errorf("failed to get filesystem stats for %s: %w", path, err)
	}

	total := stat.Blocks * uint64(stat.Bsize)
	available := stat.Bavail * uint64(stat.Bsize)
	used := total - available
	usedPercent := 0.0
	if total > 0 {
		usedPercent = (float64(used) / float64(total)) * 100
	}

	return &StorageCapacity{
		Total:       total,
		Used:        used,
		Available:   available,
		UsedPercent: usedPercent,
		Path:        path,
	}, nil
}

// getDirectorySize returns the total size of a directory in bytes, honoring the exclude patterns
func (bm *BackupManager) getDirectorySize(dirPath string, excludes []string) (int64, error) {
	excludeFlags := BuildTarExcludeFlags(excludes)
	duCmd := fmt.Sprintf(`du -sb %s %s 2>/dev/null | awk '{print $1}'`, excludeFlags, shellEscape(dirPath))

	stdout, stderr, err := bm.executeCommand(duCmd)
	if err != nil && strings.TrimSpace(stdout) == "" {
		return 0, fmt.Errorf("failed to get directory size: %w (stderr: %s)", err, stderr)
	}

	trimmedOutput := strings.TrimSpace(stdout)
	if trimmedOutput == "" {
		return 0, fmt.Errorf("directory not found or empty output from du command")
	}

	// If du outputs multiple lines for any reason, take the last line
	lines := strings.Split(trimmedOutput, "\n")
	lastLine := strings.TrimSpace(lines[len(lines)-1])

	size, err := strconv.ParseInt(lastLine, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse directory size '%s': %w", lastLine, err)
	}

	return size, nil
}

// EstimateCompressedSize estimates the compressed size of a backup using the specified method
func (bm *BackupManager) EstimateCompressedSize(workingDir string, excludes []string, method string, sampleSize int64) (compressedSize, uncompressedSize int64, err error) {
	uncompressedSize, err = bm.getDirectorySize(workingDir, excludes)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get directory size: %w", err)
	}

	switch method {
	case "heuristic":
		compressedSize, err = bm.estimateHeuristic(workingDir, excludes, uncompressedSize)
	case "sample":
		compressedSize, err = bm.estimateSample(workingDir, excludes, sampleSize, uncompressedSize)
	case "accurate":
		compressedSize, err = bm.estimateAccurate(workingDir, excludes)
	default:
		return 0, uncompressedSize, fmt.Errorf("unknown estimation method: %s (use 'heuristic', 'sample', or 'accurate')", method)
	}

	if err != nil {
		return 0, uncompressedSize, err
	}

	return compressedSize, uncompressedSize, nil
}

func (bm *BackupManager) estimateHeuristic(workingDir string, excludes []string, uncompressedSize int64) (int64, error) {
	cleanRoot := filepath.Clean(workingDir)
	mergedExcludes := MergeUniqueStrings(DefaultArchiveExcludes, excludes)

	var estimatedSize int64
	var fileCount int64

	err := filepath.WalkDir(cleanRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if path == cleanRoot {
			return nil
		}

		name := d.Name()
		relPath, relErr := filepath.Rel(cleanRoot, path)
		if relErr != nil {
			return nil
		}

		if d.IsDir() {
			if name == ".git" && containsExclude(mergedExcludes, ".git") {
				return filepath.SkipDir
			}
			if shouldSkipDirDuringGitScan(relPath, name, mergedExcludes) {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip symlinks or irregular files
		if !d.Type().IsRegular() {
			return nil
		}

		// Check file-level exclusions (e.g. *.tgz, *.swp)
		for _, ex := range mergedExcludes {
			if matched, matchErr := filepath.Match(ex, name); matchErr == nil && matched {
				return nil
			}
		}

		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		size := info.Size()
		fileCount++

		ext := strings.ToLower(filepath.Ext(name))
		switch ext {
		case ".txt", ".log", ".sql", ".php", ".go", ".py", ".ts", ".tsx", ".js", ".jsx", ".html", ".css", ".scss", ".json", ".xml", ".csv", ".md", ".yml", ".yaml", ".toml", ".sh", ".env":
			estimatedSize += size * 30 / 100
		case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".mp4", ".mp3", ".avi", ".mov", ".zip", ".gz", ".tgz", ".bz2", ".zst", ".pdf", ".woff", ".woff2", ".ttf", ".eot", ".gguf", ".bin":
			estimatedSize += size * 95 / 100
		case ".svg", ".otf":
			estimatedSize += size * 50 / 100
		default:
			estimatedSize += size * 50 / 100
		}

		return nil
	})
	if err != nil {
		return 0, err
	}

	estimatedSize += fileCount * 512
	return estimatedSize, nil
}

func containsExclude(excludes []string, target string) bool {
	for _, e := range excludes {
		if strings.TrimSuffix(strings.TrimSpace(e), "/") == target {
			return true
		}
	}
	return false
}

func (bm *BackupManager) estimateSample(workingDir string, excludes []string, sampleSize, uncompressedSize int64) (int64, error) {
	if sampleSize <= 0 {
		sampleSize = 100 * 1024 * 1024
	}
	excludeFlags := BuildTarExcludeFlags(excludes)
	tarCmd := fmt.Sprintf(`tar -cf - %s -C %s . | head -c %d | gzip -c`,
		excludeFlags, shellEscape(workingDir), sampleSize)

	counter := &countingWriter{}
	cmd := exec.Command("bash", "-c", tarCmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = counter

	if err := cmd.Run(); err != nil {
		if !strings.Contains(err.Error(), "exit status 141") {
			return 0, fmt.Errorf("sample compression failed: %w (stderr: %s)", err, stderr.String())
		}
	}

	if counter.written == 0 {
		return 0, fmt.Errorf("no data captured in sample")
	}

	effectiveSample := sampleSize
	if uncompressedSize > 0 && uncompressedSize < sampleSize {
		effectiveSample = uncompressedSize
	}

	ratio := float64(counter.written) / float64(effectiveSample)
	if ratio > 1.0 {
		ratio = 1.0
	}
	return int64(float64(uncompressedSize) * ratio), nil
}

func (bm *BackupManager) estimateAccurate(workingDir string, excludes []string) (int64, error) {
	excludeFlags := BuildTarExcludeFlags(excludes)
	tarCmd := fmt.Sprintf(`tar -czf - %s -C %s .`, excludeFlags, shellEscape(workingDir))

	counter := &countingWriter{}
	cmd := exec.Command("bash", "-lc", tarCmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = counter

	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("accurate estimation failed: %w (stderr: %s)", err, stderr.String())
	}

	return counter.written, nil
}

// EstimateCapacityFromScan scans targets and estimates storage capacity requirements
func (bm *BackupManager) EstimateCapacityFromScan(targets []TargetInfo, estimateMethod string, sampleSize int64, options *CapacityEstimateOptions) (*CapacityEstimate, error) {
	if len(targets) == 0 {
		return nil, fmt.Errorf("no targets provided")
	}

	result := &CapacityEstimate{
		EstimationMethod:    estimateMethod,
		SitesScanned:        len(targets),
		DailyRetention:      options.DailyRetention,
		WeeklyRetention:     options.WeeklyRetention,
		MonthlyRetention:    options.MonthlyRetention,
		TotalBackupsPerSite: options.DailyRetention + options.WeeklyRetention + options.MonthlyRetention,
		BufferPercent:       options.BufferPercent,
		Sites:               make([]SiteEstimate, 0, len(targets)),
	}

	var totalCompressed int64
	var totalUncompressed int64

	fmt.Printf("Scanning %d target(s)...\n", len(targets))
	startTime := time.Now()

	for i, target := range targets {
		targetStart := time.Now()
		fmt.Printf("  [%d/%d] Analyzing %s (%s)...\n", i+1, len(targets), target.Label, target.WorkingDir)

		var excludes []string
		if target.Config != nil {
			excludes = target.Config.Excludes
		}

		compressedSize, uncompressedSize, err := bm.EstimateCompressedSize(
			target.WorkingDir,
			excludes,
			estimateMethod,
			sampleSize,
		)
		targetDuration := time.Since(targetStart)

		if err != nil {
			fmt.Printf("    ⚠️  Warning: Could not estimate %s: %v\n", target.Label, err)
			continue
		}

		compressionRatio := 0.0
		if uncompressedSize > 0 {
			compressionRatio = (1.0 - float64(compressedSize)/float64(uncompressedSize)) * 100
		}

		siteEst := SiteEstimate{
			SiteName:         target.Label,
			UncompressedSize: uncompressedSize,
			CompressedSize:   compressedSize,
			CompressionRatio: compressionRatio,
			HotStorageSize:   compressedSize * int64(options.DailyRetention),
			ColdStorageSize:  compressedSize * int64(options.WeeklyRetention+options.MonthlyRetention),
		}
		siteEst.TotalStorageSize = siteEst.HotStorageSize + siteEst.ColdStorageSize

		result.Sites = append(result.Sites, siteEst)
		totalCompressed += compressedSize
		totalUncompressed += uncompressedSize

		fmt.Printf("    Compressed: %.2f MB, Uncompressed: %.2f MB (%.1f%% saved) [took %s]\n",
			float64(compressedSize)/(1024*1024),
			float64(uncompressedSize)/(1024*1024),
			compressionRatio,
			targetDuration.Round(time.Millisecond))
	}

	if len(result.Sites) == 0 {
		return nil, fmt.Errorf("no targets successfully analyzed")
	}

	totalDuration := time.Since(startTime)
	fmt.Printf("\n✅ Scan complete in %s\n\n", totalDuration.Round(time.Millisecond))

	result.AvgCompressedSize = totalCompressed / int64(len(result.Sites))
	result.AvgUncompressedSize = totalUncompressed / int64(len(result.Sites))
	if result.AvgUncompressedSize > 0 {
		result.AvgCompressionRatio = (1.0 - float64(result.AvgCompressedSize)/float64(result.AvgUncompressedSize)) * 100
	}

	result.PerSiteHotStorage = result.AvgCompressedSize * int64(options.DailyRetention)
	result.PerSiteColdStorage = result.AvgCompressedSize * int64(options.WeeklyRetention+options.MonthlyRetention)
	result.PerSiteTotalStorage = result.PerSiteHotStorage + result.PerSiteColdStorage

	result.FleetHotStorage = totalCompressed * int64(options.DailyRetention)
	result.FleetColdStorage = totalCompressed * int64(options.WeeklyRetention+options.MonthlyRetention)
	result.FleetTotalStorage = result.FleetHotStorage + result.FleetColdStorage
	result.FleetTotalWithBuffer = int64(float64(result.FleetTotalStorage) * (1.0 + options.BufferPercent/100.0))

	if options.GrowthRate > 0 && options.ProjectionMonths > 0 {
		result.GrowthProjections = calculateGrowthProjections(
			result.FleetHotStorage,
			result.FleetColdStorage,
			options.GrowthRate,
			options.ProjectionMonths,
			options.S3PricePerGB,
		)
	}

	if options.S3PricePerGB > 0 {
		totalStorageGB := float64(result.FleetTotalStorage) / (1024 * 1024 * 1024)
		result.MonthlyCost = totalStorageGB * options.S3PricePerGB
		if options.RetrievalPricePerGB > 0 {
			result.RetrievalCost10Pct = totalStorageGB * 0.10 * options.RetrievalPricePerGB
		}
	}

	return result, nil
}

func calculateGrowthProjections(hotStorage, coldStorage int64, monthlyGrowthRate float64, months int, pricePerGB float64) []GrowthProjection {
	projections := make([]GrowthProjection, 0, months)
	growthMultiplier := 1.0 + (monthlyGrowthRate / 100.0)

	for m := 1; m <= months; m++ {
		factor := math.Pow(growthMultiplier, float64(m))
		hotGB := (float64(hotStorage) * factor) / (1024 * 1024 * 1024)
		coldGB := (float64(coldStorage) * factor) / (1024 * 1024 * 1024)
		totalGB := hotGB + coldGB

		proj := GrowthProjection{
			Month:          m,
			TotalStorageGB: totalGB,
			HotStorageGB:   hotGB,
			ColdStorageGB:  coldGB,
		}
		if pricePerGB > 0 {
			proj.MonthlyCost = totalGB * pricePerGB
		}
		projections = append(projections, proj)
	}

	return projections
}

// Ensure io.Reader/os are referenced if needed
var _ = io.EOF
var _ = os.Stdout
