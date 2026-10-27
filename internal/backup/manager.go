package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
)

// BackupOptions configures a backup execution run
type BackupOptions struct {
	DryRun               bool
	ConfigFile           string
	ConfigDir            string
	Targets              []string // Filter by target name, label, subdirectory name, or path
	Group                string   // Filter by parent target group (e.g. "var-www" or "home-maxx")
	AdHocPaths           []string // Explicit paths to back up via --path
	AdditionalExcludes   []string // Extra excludes provided via --exclude CLI flag
	NoGitRefs            bool     // Disable .git-backup-ref.txt overlay generation if true
	RespectCapacityLimit bool
	CapacityThreshold    float64
	StorageCheckPath     string
	EstimateMethod       string
	SampleSize           int64
	SmartRetention       *SmartRetentionPolicy
	AutoFixDatabaseTools bool
}

// TargetInfo represents a concrete resolved directory/container target to back up
type TargetInfo struct {
	Name       string
	Label      string
	Group      string
	WorkingDir string
	Type       string
	BucketPath string
	Config     *TargetConfig
}

// BackupManager manages local backup operations and Hetzner S3 storage
type BackupManager struct {
	s3Client  *minio.Client
	s3Config  *S3Config
	verbosity int // 0=quiet, 1=normal, 2=verbose, 3=debug, 4=trace
}

// NewBackupManagerWithS3 creates a BackupManager configured for Hetzner S3
func NewBackupManagerWithS3(s3Config *S3Config) *BackupManager {
	return &BackupManager{
		s3Config:  s3Config,
		verbosity: 1,
	}
}

// SetVerbosity sets the logging verbosity level (0=quiet, 1=normal, 2=verbose, 3=debug, 4=trace)
func (bm *BackupManager) SetVerbosity(level int) {
	bm.verbosity = level
}

func (bm *BackupManager) logVerbose(format string, args ...interface{}) {
	if bm.verbosity >= 2 {
		fmt.Printf("[VERBOSE] "+format+"\n", args...)
	}
}

func (bm *BackupManager) logDebug(format string, args ...interface{}) {
	if bm.verbosity >= 3 {
		fmt.Printf("[DEBUG] "+format+"\n", args...)
	}
}

func (bm *BackupManager) logTrace(format string, args ...interface{}) {
	if bm.verbosity >= 4 {
		fmt.Printf("[TRACE] "+format+"\n", args...)
	}
}

// GetBucketPath returns the globally configured S3 bucket path prefix
func (bm *BackupManager) GetBucketPath() string {
	if bm.s3Config != nil {
		return bm.s3Config.BucketPath
	}
	return ""
}

// executeCommand runs a local shell command and returns stdout, stderr, and error
func (bm *BackupManager) executeCommand(cmd string) (string, string, error) {
	c := exec.Command("bash", "-lc", cmd)
	var out bytes.Buffer
	var stderr bytes.Buffer
	c.Stdout = &out
	c.Stderr = &stderr
	err := c.Run()
	return out.String(), stderr.String(), err
}

// GetTargetsFromOptions resolves and filters all backup targets based on options
func (bm *BackupManager) GetTargetsFromOptions(options *BackupOptions) ([]TargetInfo, error) {
	return bm.resolveTargets(options)
}

func (bm *BackupManager) resolveTargets(options *BackupOptions) ([]TargetInfo, error) {
	var rawTargets []TargetInfo

	// 1. If explicit --path flags were provided, build ad-hoc targets
	if len(options.AdHocPaths) > 0 {
		defaultCfg := DefaultConfig()
		for _, p := range options.AdHocPaths {
			cleanPath, err := filepath.Abs(strings.TrimSpace(p))
			if err != nil {
				return nil, fmt.Errorf("invalid path %q: %w", p, err)
			}
			base := filepath.Base(cleanPath)
			genGit := !options.NoGitRefs
			tCfg := TargetConfig{
				Name:            base,
				Label:           base,
				Mode:            "single",
				Type:            "filesystem",
				Paths:           PathsConfig{WorkingDir: cleanPath},
				Excludes:        MergeUniqueStrings(defaultCfg.Defaults.Excludes, options.AdditionalExcludes),
				GenerateGitRefs: &genGit,
				BucketPath:       filepath.ToSlash(filepath.Join("custom", base)),
			}
			rawTargets = append(rawTargets, TargetInfo{
				Name:       base,
				Label:      base,
				Group:      "custom",
				WorkingDir: cleanPath,
				Type:       "filesystem",
				BucketPath: tCfg.BucketPath,
				Config:     &tCfg,
			})
		}
		return filterTargets(rawTargets, options), nil
	}

	// 2. Load config from ConfigFile, ConfigDir, or fallback to DefaultConfig()
	var configs []*BackupConfig
	if options.ConfigFile != "" {
		cfg, err := LoadConfigFromFile(options.ConfigFile)
		if err != nil {
			return nil, err
		}
		configs = append(configs, cfg)
	}
	if options.ConfigDir != "" {
		entries, err := os.ReadDir(options.ConfigDir)
		if err != nil {
			return nil, fmt.Errorf("failed to read config directory %s: %w", options.ConfigDir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			ext := strings.ToLower(filepath.Ext(entry.Name()))
			if ext != ".yml" && ext != ".yaml" {
				continue
			}
			cfg, err := LoadConfigFromFile(filepath.Join(options.ConfigDir, entry.Name()))
			if err != nil {
				return nil, err
			}
			configs = append(configs, cfg)
		}
	}
	if len(configs) == 0 {
		configs = append(configs, DefaultConfig())
	}

	for _, cfg := range configs {
		for _, targetCfg := range cfg.Targets {
			if targetCfg.Skip {
				bm.logVerbose("Skipping target %s (marked as skip in config)", targetCfg.Name)
				continue
			}

			tCopy := targetCfg
			cfg.ApplyDefaults(&tCopy)
			if len(options.AdditionalExcludes) > 0 {
				tCopy.Excludes = MergeUniqueStrings(tCopy.Excludes, options.AdditionalExcludes)
			}
			if options.NoGitRefs {
				f := false
				tCopy.GenerateGitRefs = &f
			}

			expanded, err := bm.expandTargetConfig(tCopy)
			if err != nil {
				fmt.Printf("⚠️  Warning: could not expand target %s: %v\n", tCopy.Name, err)
				continue
			}
			rawTargets = append(rawTargets, expanded...)
		}
	}

	return filterTargets(rawTargets, options), nil
}

// expandTargetConfig expands a TargetConfig into one or more concrete TargetInfo entries
func (bm *BackupManager) expandTargetConfig(tCfg TargetConfig) ([]TargetInfo, error) {
	workingDir := tCfg.Paths.WorkingDir
	if workingDir == "" && strings.HasPrefix(tCfg.Name, "/") {
		workingDir = tCfg.Name
	}
	if workingDir == "" {
		return nil, fmt.Errorf("working_dir is required for target %q", tCfg.Name)
	}

	mode := strings.ToLower(tCfg.Mode)
	if mode == "" || mode == "single" || mode == "directory" {
		label := tCfg.Label
		if label == "" {
			label = tCfg.Name
		}
		if label == "" {
			label = filepath.Base(workingDir)
		}
		bucketPath := tCfg.BucketPath
		if bucketPath == "" {
			bucketPath = filepath.ToSlash(filepath.Join("backups", label))
		}
		cfgCopy := tCfg
		return []TargetInfo{
			{
				Name:       tCfg.Name,
				Label:      label,
				Group:      tCfg.Name,
				WorkingDir: workingDir,
				Type:       tCfg.Type,
				BucketPath: bucketPath,
				Config:     &cfgCopy,
			},
		}, nil
	}

	if mode == "subdirectories" {
		return bm.expandSubdirectoryTarget(tCfg, workingDir)
	}

	return nil, fmt.Errorf("unsupported mode %q for target %q", tCfg.Mode, tCfg.Name)
}

func (bm *BackupManager) expandSubdirectoryTarget(parentCfg TargetConfig, parentDir string) ([]TargetInfo, error) {
	entries, err := os.ReadDir(parentDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read directory %s: %w", parentDir, err)
	}

	skipSet := make(map[string]bool)
	for _, s := range parentCfg.SkipDirs {
		skipSet[strings.TrimSpace(s)] = true
	}

	nestedSet := make(map[string]bool)
	for _, n := range parentCfg.NestedGroups {
		nestedSet[strings.TrimSpace(n)] = true
	}

	baseBucketPrefix := parentCfg.BucketPath
	if baseBucketPrefix == "" {
		baseBucketPrefix = parentCfg.Name
	}

	var results []TargetInfo

	for _, entry := range entries {
		name := entry.Name()
		// Skip hidden system/dot directories at top-level of /var/www unless explicitly overridden
		if strings.HasPrefix(name, ".") {
			continue
		}
		if skipSet[name] {
			bm.logVerbose("Skipping directory %s (listed in skip_dirs)", name)
			continue
		}

		// Skip non-directories and symlinks (entry.IsDir() is false for symlinks and regular files)
		if !entry.IsDir() {
			continue
		}

		fullPath := filepath.Join(parentDir, name)

		// Check if this subdirectory is a nested group (e.g. /var/www/edfx-labs)
		if nestedSet[name] {
			subEntries, subErr := os.ReadDir(fullPath)
			if subErr != nil {
				fmt.Printf("⚠️  Warning: failed to read nested group %s: %v\n", fullPath, subErr)
				continue
			}
			for _, subEntry := range subEntries {
				subName := subEntry.Name()
				if strings.HasPrefix(subName, ".") || !subEntry.IsDir() {
					continue
				}
				subFullPath := filepath.Join(fullPath, subName)
				relKey := name + "/" + subName
				if skipSet[relKey] || skipSet[subName] {
					continue
				}

				childTarget := buildChildTargetInfo(parentCfg, subName, relKey, subFullPath, filepath.ToSlash(filepath.Join(baseBucketPrefix, name, subName)))
				if childTarget != nil {
					childTarget.Group = name
					results = append(results, *childTarget)
				}
			}
			continue
		}

		childTarget := buildChildTargetInfo(parentCfg, name, name, fullPath, filepath.ToSlash(filepath.Join(baseBucketPrefix, name)))
		if childTarget != nil {
			results = append(results, *childTarget)
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].WorkingDir < results[j].WorkingDir
	})

	return results, nil
}

func buildChildTargetInfo(parentCfg TargetConfig, defaultLabel, overrideKey, fullPath, defaultBucketPath string) *TargetInfo {
	childCfg := TargetConfig{
		Name:               defaultLabel,
		Label:              defaultLabel,
		Mode:               "single",
		Type:               parentCfg.Type,
		Paths:              PathsConfig{WorkingDir: fullPath},
		PreBackupCommands:  append([]string{}, parentCfg.PreBackupCommands...),
		PostBackupCommands: append([]string{}, parentCfg.PostBackupCommands...),
		Excludes:           append([]string{}, parentCfg.Excludes...),
		GenerateGitRefs:    parentCfg.GenerateGitRefs,
		BucketPath:         defaultBucketPath,
		Env:                parentCfg.Env,
	}

	if override, ok := parentCfg.Overrides[overrideKey]; ok {
		if override.Skip {
			return nil
		}
		if override.Label != "" {
			childCfg.Label = override.Label
		}
		if override.Type != "" {
			childCfg.Type = override.Type
		}
		if override.Database.Type != "" || override.Database.ExportCommand != "" {
			childCfg.Database = override.Database
			childCfg.Database.Password = resolveEnvVar(childCfg.Database.Password)
		}
		if len(override.PreBackupCommands) > 0 {
			childCfg.PreBackupCommands = override.PreBackupCommands
		}
		if len(override.PostBackupCommands) > 0 {
			childCfg.PostBackupCommands = override.PostBackupCommands
		}
		if len(override.Excludes) > 0 {
			childCfg.Excludes = MergeUniqueStrings(childCfg.Excludes, override.Excludes)
		}
		if override.BucketPath != "" {
			childCfg.BucketPath = override.BucketPath
		}
	}

	return &TargetInfo{
		Name:       childCfg.Name,
		Label:      childCfg.Label,
		Group:      parentCfg.Name,
		WorkingDir: fullPath,
		Type:       childCfg.Type,
		BucketPath: childCfg.BucketPath,
		Config:     &childCfg,
	}
}

func filterTargets(targets []TargetInfo, options *BackupOptions) []TargetInfo {
	if options == nil || (len(options.Targets) == 0 && options.Group == "") {
		return targets
	}

	var filtered []TargetInfo
	for _, t := range targets {
		if options.Group != "" && !strings.EqualFold(t.Group, options.Group) {
			continue
		}

		if len(options.Targets) > 0 {
			matched := false
			baseDir := filepath.Base(t.WorkingDir)
			for _, f := range options.Targets {
				f = strings.TrimSpace(f)
				if f == "" {
					continue
				}
				if strings.EqualFold(t.Name, f) ||
					strings.EqualFold(t.Label, f) ||
					strings.EqualFold(t.Group, f) ||
					strings.EqualFold(baseDir, f) ||
					filepath.Clean(t.WorkingDir) == filepath.Clean(f) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}

		filtered = append(filtered, t)
	}

	return filtered
}

// CreateBackups orchestrates backing up all resolved targets to the Hetzner EDFX S3 bucket
func (bm *BackupManager) CreateBackups(options *BackupOptions) error {
	if options.RespectCapacityLimit {
		threshold := options.CapacityThreshold
		if threshold <= 0 {
			threshold = 95.0
		}
		checkPath := options.StorageCheckPath
		if checkPath == "" {
			checkPath = "/"
		}
		capacity, err := bm.GetStorageCapacity(checkPath)
		if err != nil {
			return fmt.Errorf("failed to check local disk capacity: %w", err)
		}
		if capacity.UsedPercent > threshold {
			return fmt.Errorf("local disk usage at %s exceeds %.1f%% (current: %.1f%%); aborting backup",
				checkPath, threshold, capacity.UsedPercent)
		}
		fmt.Printf("✓ Local disk capacity check passed: %.1f%% used (threshold: %.1f%%)\n", capacity.UsedPercent, threshold)
	}

	if !options.DryRun {
		if err := bm.initS3Client(); err != nil {
			return err
		}
	}

	targets, err := bm.resolveTargets(options)
	if err != nil {
		return err
	}

	if len(targets) == 0 {
		fmt.Println("No matching targets found to back up.")
		return nil
	}

	total := len(targets)
	processed := 0
	successCount := 0
	failedCount := 0
	var totalCompressed int64
	var totalUncompressed int64

	for idx, target := range targets {
		processed++
		fmt.Printf("\n--- [%d/%d] Processing target: %s (group: %s) ---\n", idx+1, total, target.Label, target.Group)
		compressedSize, uncompressedSize, err := bm.processTarget(target, options)
		if err != nil {
			fmt.Printf("❌ Error processing target %s: %v\n", target.Label, err)
			failedCount++
			continue
		}
		successCount++
		totalCompressed += compressedSize
		totalUncompressed += uncompressedSize

		fmt.Printf("Progress: %d/%d processed, %d succeeded, %d failed\n", processed, total, successCount, failedCount)
		fmt.Printf("Aggregate compressed: %.2f MB, Aggregate uncompressed: %.2f MB\n",
			float64(totalCompressed)/(1024*1024),
			float64(totalUncompressed)/(1024*1024))
		if totalUncompressed > 0 && totalCompressed > 0 {
			ratio := (1.0 - float64(totalCompressed)/float64(totalUncompressed)) * 100
			fmt.Printf("Overall compression: %.1f%% space saved\n", ratio)
		}
	}

	if failedCount > 0 && successCount == 0 {
		return fmt.Errorf("all %d target(s) failed to back up", failedCount)
	}

	return nil
}

func (bm *BackupManager) processTarget(target TargetInfo, options *BackupOptions) (int64, int64, error) {
	fmt.Printf("Target: %s (type: %s)\n", target.Label, target.Type)
	fmt.Printf("Source directory: %s\n", target.WorkingDir)

	var excludes []string
	generateGitRefs := true
	if target.Config != nil {
		excludes = target.Config.Excludes
		generateGitRefs = target.Config.ShouldGenerateGitRefs()
	}
	if options.NoGitRefs {
		generateGitRefs = false
	}

	timestamp := time.Now().Format("20060102-150405")
	backupName := fmt.Sprintf("%s-%s.tgz", target.Label, timestamp)
	objectName := bm.ResolveObjectKey(target, backupName)

	if options.DryRun {
		fmt.Printf("[DRY RUN] Would back up %s\n", target.WorkingDir)
		fmt.Printf("[DRY RUN] Target S3 Bucket: %s\n", bm.s3Config.Bucket)
		fmt.Printf("[DRY RUN] Target S3 Object Key: %s\n", objectName)
		fmt.Printf("[DRY RUN] Excludes (%d patterns): %s\n", len(excludes), strings.Join(excludes, ", "))

		if generateGitRefs && containsExclude(excludes, ".git") {
			repos, err := ScanGitRepositories(target.WorkingDir, excludes)
			if err != nil {
				fmt.Printf("[DRY RUN] ⚠️  Git repository scan warning: %v\n", err)
			} else {
				fmt.Printf("[DRY RUN] 🔗 Found %d Git repository(s) — would generate %s and %s in archive:\n",
					len(repos), GitBackupRefFileName, GitManifestTextFileName)
				for _, r := range repos {
					fmt.Printf("          • %s (branch: %s, commit: %s, remote: %s)\n",
						r.RelativePath,
						defaultIfEmpty(r.Branch, "none"),
						defaultIfEmpty(r.ShortHash, "none"),
						defaultIfEmpty(r.PrimaryRemoteURL, "local-only"))
				}
			}
		}

		var estimatedCompressed, uncompressedSize int64
		if options.EstimateMethod != "" {
			fmt.Printf("\n[DRY RUN] Estimating compressed size using '%s' method...\n", options.EstimateMethod)
			startTime := time.Now()
			var err error
			estimatedCompressed, uncompressedSize, err = bm.EstimateCompressedSize(
				target.WorkingDir,
				excludes,
				options.EstimateMethod,
				options.SampleSize,
			)
			duration := time.Since(startTime)
			if err != nil {
				fmt.Printf("[DRY RUN] ⚠️  Estimation failed: %v\n", err)
			} else {
				compressedMB := float64(estimatedCompressed) / (1024 * 1024)
				uncompressedMB := float64(uncompressedSize) / (1024 * 1024)
				ratio := 0.0
				if uncompressedSize > 0 {
					ratio = (1.0 - float64(estimatedCompressed)/float64(uncompressedSize)) * 100
				}
				fmt.Printf("[DRY RUN] 📊 Estimation complete (took %s):\n", duration.Round(time.Millisecond))
				fmt.Printf("[DRY RUN]    Uncompressed (after excludes): %.2f MB (%d bytes)\n", uncompressedMB, uncompressedSize)
				fmt.Printf("[DRY RUN]    Estimated compressed:          %.2f MB (%d bytes)\n", compressedMB, estimatedCompressed)
				fmt.Printf("[DRY RUN]    Compression ratio:             %.1f%% space saved\n", ratio)
			}
		}
		fmt.Printf("Done with %s\n", target.Label)
		return estimatedCompressed, uncompressedSize, nil
	}

	// 1. Run pre-backup commands if specified
	if target.Config != nil && len(target.Config.PreBackupCommands) > 0 {
		fmt.Printf("Running pre-backup commands...\n")
		for _, cmd := range target.Config.PreBackupCommands {
			fmt.Printf("  Running: %s\n", cmd)
			if _, stderr, err := bm.executeCommand(cmd); err != nil {
				return 0, 0, fmt.Errorf("pre-backup command failed: %w (stderr: %s)", err, stderr)
			}
		}
	}

	// 2. Run database export if configured
	if target.Type == "wordpress" {
		if err := bm.exportWordPressDatabase(target); err != nil {
			return 0, 0, err
		}
	} else if target.Config != nil && (target.Config.Database.Type != "" || target.Config.Database.ExportCommand != "") {
		if err := bm.exportDatabase(target, options); err != nil {
			return 0, 0, err
		}
	}

	// 3. Determine archive source directory
	backupDir := target.WorkingDir
	if target.Config != nil && target.Config.Paths.AppDir != "" {
		backupDir = target.Config.Paths.AppDir
	}

	// 4. Generate Git Reference Overlay if .git is excluded
	var overlayDir string
	if generateGitRefs && containsExclude(excludes, ".git") {
		fmt.Printf("   🔍 Scanning for Git repositories to create reference artifacts...\n")
		var repos []GitRepoMetadata
		var gitErr error
		overlayDir, repos, gitErr = CreateGitRefsOverlay(target.Label, backupDir, excludes)
		if gitErr != nil {
			fmt.Printf("   ⚠️  Warning: Git reference scan encountered an issue: %v\n", gitErr)
		} else if len(repos) > 0 {
			defer os.RemoveAll(overlayDir)
			fmt.Printf("   🔗 Cataloged %d Git repository(s) into %s & %s\n",
				len(repos), GitBackupRefFileName, GitManifestTextFileName)
		}
	}

	// 5. Calculate uncompressed source size (respecting excludes)
	fmt.Printf("   Calculating source size (with exclusions)...\n")
	uncompressedSize, err := bm.getDirectorySize(backupDir, excludes)
	if err != nil {
		fmt.Printf("   ⚠️  Warning: Could not determine source size: %v\n", err)
		uncompressedSize = 0
	} else {
		fmt.Printf("   Uncompressed: %.2f MB\n", float64(uncompressedSize)/(1024*1024))
	}

	// 6. Stream tar archive directly to Hetzner S3
	fmt.Printf("   Streaming archive to S3: s3://%s/%s\n", bm.s3Config.Bucket, objectName)
	compressedSize, err := bm.streamBackupToS3(backupDir, overlayDir, objectName, excludes, uncompressedSize)
	if err != nil {
		return 0, uncompressedSize, fmt.Errorf("failed to stream backup to S3: %w", err)
	}

	if uncompressedSize > 0 && compressedSize > 0 {
		compressionRatio := (1.0 - float64(compressedSize)/float64(uncompressedSize)) * 100
		fmt.Printf("   💾 Compression: %.1f%% space saved\n", compressionRatio)
	}

	// 7. Run post-backup commands if specified
	if target.Config != nil && len(target.Config.PostBackupCommands) > 0 {
		fmt.Printf("Running post-backup commands...\n")
		for _, cmd := range target.Config.PostBackupCommands {
			fmt.Printf("  Running: %s\n", cmd)
			if _, stderr, err := bm.executeCommand(cmd); err != nil {
				fmt.Printf("Warning: post-backup command failed: %v (stderr: %s)\n", err, stderr)
			}
		}
	}

	fmt.Printf("Done with %s\n", target.Label)
	return compressedSize, uncompressedSize, nil
}

// ResolveObjectKey computes the full S3 object key for a target backup file
func (bm *BackupManager) ResolveObjectKey(target TargetInfo, backupName string) string {
	prefix :=bm.ResolveTargetPrefix(target)
	return filepath.ToSlash(filepath.Join(prefix, backupName))
}

// ResolveTargetPrefix computes the S3 key prefix (with trailing slash) for a target
func (bm *BackupManager) ResolveTargetPrefix(target TargetInfo) string {
	var parts []string
	if globalPrefix := strings.TrimSpace(bm.GetBucketPath()); globalPrefix != "" {
		parts = append(parts, strings.Trim(globalPrefix, "/"))
	}
	if target.BucketPath != "" {
		parts = append(parts, strings.Trim(target.BucketPath, "/"))
	} else {
		parts = append(parts, "backups", target.Label)
	}
	return strings.Join(parts, "/") + "/"
}

// BuildTarStreamCommand constructs the shell command for creating a compressed tar stream to stdout
func BuildTarStreamCommand(workingDir, overlayDir string, excludes []string) string {
	excludeFlags := BuildTarExcludeFlags(excludes)
	cmd := fmt.Sprintf(`tar -czf - --warning=no-file-changed %s -C %s .`, excludeFlags, shellEscape(workingDir))
	if overlayDir != "" {
		cmd += fmt.Sprintf(` -C %s .`, shellEscape(overlayDir))
	}
	return cmd
}

func (bm *BackupManager) streamBackupToS3(workingDir, overlayDir, objectName string, excludes []string, uncompressedSize int64) (int64, error) {
	if err := bm.initS3Client(); err != nil {
		return 0, fmt.Errorf("failed to initialize S3 client: %w", err)
	}

	tarCmd := BuildTarStreamCommand(workingDir, overlayDir, excludes)
	bm.logDebug("Executing tar command: %s", tarCmd)

	cmd := exec.Command("bash", "-lc", tarCmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, fmt.Errorf("failed to create stdout pipe for tar: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("failed to start tar command: %w", err)
	}

	ctx := context.Background()
	var reader io.Reader = stdout
	if bm.verbosity >= 1 {
		reader = NewProgressReader(stdout, 0, "Uploading")
	}

	info, err := bm.s3Client.PutObject(ctx, bm.s3Config.Bucket, objectName, reader, -1, minio.PutObjectOptions{
		ContentType: "application/gzip",
	})
	if err != nil {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return 0, fmt.Errorf("failed to upload to S3: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderrStr := stderr.String()
			if exitErr.ExitCode() == 1 && (strings.Contains(stderrStr, "file changed as we read it") || strings.TrimSpace(stderrStr) == "") {
				fmt.Printf("⚠️  Warning: tar reported non-fatal file change during read\n")
			} else {
				return 0, fmt.Errorf("tar command failed: %w (stderr: %s)", err, strings.TrimSpace(stderrStr))
			}
		} else {
			return 0, fmt.Errorf("tar command failed: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
		}
	}

	sizeMB := float64(info.Size) / (1024 * 1024)
	fmt.Printf("✓ Successfully uploaded to Hetzner S3: s3://%s/%s (%.2f MB)\n", bm.s3Config.Bucket, objectName, sizeMB)
	return info.Size, nil
}
