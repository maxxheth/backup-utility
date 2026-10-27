package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// BackupConfig represents the YAML configuration for EDFX local backup operations.
// It supports both `targets:` (preferred) and `containers:` (for backwards compatibility with ciwg-cli configs).
type BackupConfig struct {
	// Version of the config schema
	Version string `yaml:"version"`

	// Default settings applied to all targets unless overridden
	Defaults ConfigDefaults `yaml:"defaults,omitempty"`

	// List of backup targets (directories or subdirectory groups)
	Targets []TargetConfig `yaml:"targets,omitempty"`

	// Backwards-compatible alias for `targets` (from ciwg-cli)
	Containers []TargetConfig `yaml:"containers,omitempty"`

	// Retention policy rulesets for backup lifecycle management
	Rulesets map[string]RetentionRuleset `yaml:"rulesets,omitempty"`
}

// ConfigDefaults contains default settings applied to all targets
type ConfigDefaults struct {
	DatabaseType      string            `yaml:"database_type,omitempty"`
	ExportDir         string            `yaml:"export_dir,omitempty"`
	DatabaseExportDir string            `yaml:"database_export_dir,omitempty"`
	DatabaseName      string            `yaml:"database_name,omitempty"`
	DatabaseUser      string            `yaml:"database_user,omitempty"`
	BucketPath        string            `yaml:"bucket_path,omitempty"`
	Excludes          []string          `yaml:"excludes,omitempty"`
	GenerateGitRefs   *bool             `yaml:"generate_git_refs,omitempty"`
	Env               map[string]string `yaml:"env,omitempty"`
}

// TargetConfig defines the backup configuration for a directory, subdirectory group, or containerized app
type TargetConfig struct {
	// Name of the target (or container name)
	Name string `yaml:"name"`

	// Optional label for the backup object name (defaults to target name or directory basename)
	Label string `yaml:"label,omitempty"`

	// Mode controls how the directory is packaged:
	//   - "single" (or "directory", default): backs up Paths.WorkingDir as a single archive
	//   - "subdirectories": backs up each immediate child directory of Paths.WorkingDir as its own archive
	Mode string `yaml:"mode,omitempty"`

	// Type of target: filesystem (default), custom, wordpress, postgres, mysql, mariadb, mongodb
	Type string `yaml:"type,omitempty"`

	// Database configuration (optional, for Docker database dumps before directory archiving)
	Database DatabaseConfig `yaml:"database,omitempty"`

	// Paths configuration
	Paths PathsConfig `yaml:"paths,omitempty"`

	// Pre-backup shell commands to run before creating the archive
	PreBackupCommands []string `yaml:"pre_backup_commands,omitempty"`

	// Post-backup shell commands to run after uploading the archive
	PostBackupCommands []string `yaml:"post_backup_commands,omitempty"`

	// Files/directories to exclude from backup (merged with Defaults.Excludes)
	Excludes []string `yaml:"excludes,omitempty"`

	// SkipDirs lists subdirectory names to skip when Mode == "subdirectories"
	SkipDirs []string `yaml:"skip_dirs,omitempty"`

	// NestedGroups lists child directories (when Mode == "subdirectories") that should themselves
	// be expanded into per-sub-subdirectory targets (e.g., ["edfx-labs"] under /var/www)
	NestedGroups []string `yaml:"nested_groups,omitempty"`

	// Overrides allows per-subdirectory configuration overrides when Mode == "subdirectories"
	Overrides map[string]TargetOverride `yaml:"overrides,omitempty"`

	// GenerateGitRefs controls whether .git-backup-ref.txt and GIT_REPOS_MANIFEST.txt
	// are generated and injected into the archive when .git is excluded (default: true)
	GenerateGitRefs *bool `yaml:"generate_git_refs,omitempty"`

	// Additional environment variables
	Env map[string]string `yaml:"env,omitempty"`

	// Skip this target if true
	Skip bool `yaml:"skip,omitempty"`

	// Optional S3 bucket path prefix for this target (e.g., "var-www" or "home-maxx").
	// Overrides Defaults.BucketPath.
	BucketPath string `yaml:"bucket_path,omitempty"`
}

// TargetOverride allows customizing settings for a specific subdirectory inside a "subdirectories" target
type TargetOverride struct {
	Label              string         `yaml:"label,omitempty"`
	Type               string         `yaml:"type,omitempty"`
	Database           DatabaseConfig `yaml:"database,omitempty"`
	PreBackupCommands  []string       `yaml:"pre_backup_commands,omitempty"`
	PostBackupCommands []string       `yaml:"post_backup_commands,omitempty"`
	Excludes           []string       `yaml:"excludes,omitempty"`
	Skip               bool           `yaml:"skip,omitempty"`
	BucketPath         string         `yaml:"bucket_path,omitempty"`
}

// DatabaseConfig defines database-specific export configuration
type DatabaseConfig struct {
	// Type: postgres, mysql, mariadb, mongodb, redis, custom
	Type string `yaml:"type,omitempty"`

	// Container name if database is in a separate Docker container
	Container string `yaml:"container,omitempty"`

	// PreferDbContainer automatically detects and uses a database container on the same Docker network
	PreferDbContainer bool `yaml:"prefer_db_container,omitempty"`

	// Database name
	Name string `yaml:"name,omitempty"`

	// Database user
	User string `yaml:"user,omitempty"`

	// Database password (or env var name with $ prefix, e.g. "$POSTGRES_PASSWORD")
	Password string `yaml:"password,omitempty"`

	// Database host
	Host string `yaml:"host,omitempty"`

	// Database port
	Port int `yaml:"port,omitempty"`

	// Export format: sql, dump, custom
	ExportFormat string `yaml:"export_format,omitempty"`

	// Custom export command (overrides auto-generated command)
	ExportCommand string `yaml:"export_command,omitempty"`

	// Path where database export should be saved
	ExportPath string `yaml:"export_path,omitempty"`
}

// PathsConfig defines paths for backup operations
type PathsConfig struct {
	// Working directory to back up
	WorkingDir string `yaml:"working_dir,omitempty"`

	// Directory containing application files to back up (overrides WorkingDir for the tar archive if set)
	AppDir string `yaml:"app_dir,omitempty"`

	// Directory where database exports should be saved before backup
	DatabaseExportDir string `yaml:"database_export_dir,omitempty"`

	// Additional directories to include in backup
	Include []string `yaml:"include,omitempty"`
}

// RetentionRuleset defines a single retention policy rule
type RetentionRuleset struct {
	// OlderThan specifies the age threshold (e.g., "7 days", "6 months", "1 year")
	OlderThan string `yaml:"older_than"`

	// Exclude specifies days to exclude from this rule (e.g., "Sunday", "every 7 days", "last day of month")
	Exclude string `yaml:"exclude,omitempty"`

	// Action specifies what to do with matching backups: "delete" or "keep"
	Action string `yaml:"action,omitempty"`

	// TargetStorage specifies the storage backend this rule applies to: "s3", "both"
	TargetStorage string `yaml:"target_storage,omitempty"`
}

// AllTargets returns the combined list of targets from Targets and Containers fields
func (c *BackupConfig) AllTargets() []TargetConfig {
	var all []TargetConfig
	all = append(all, c.Targets...)
	all = append(all, c.Containers...)
	return all
}

// LoadConfigFromFile loads and parses a backup configuration YAML file
func LoadConfigFromFile(path string) (*BackupConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config BackupConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config YAML: %w", err)
	}

	if config.Version == "" {
		config.Version = "1"
	}

	// Normalize Containers into Targets if Targets is empty
	if len(config.Targets) == 0 && len(config.Containers) > 0 {
		config.Targets = config.Containers
		config.Containers = nil
	} else if len(config.Containers) > 0 {
		config.Targets = append(config.Targets, config.Containers...)
		config.Containers = nil
	}

	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &config, nil
}

// Validate checks if the configuration is valid
func (c *BackupConfig) Validate() error {
	if len(c.Targets) == 0 {
		return fmt.Errorf("at least one target must be defined")
	}

	for i := range c.Targets {
		target := &c.Targets[i]
		if target.Name == "" {
			return fmt.Errorf("target[%d]: name is required", i)
		}
		if target.Type == "" {
			target.Type = "filesystem"
		}
		if target.Mode == "" {
			target.Mode = "single"
		}
		mode := strings.ToLower(target.Mode)
		if mode != "single" && mode != "directory" && mode != "subdirectories" {
			return fmt.Errorf("target[%d] (%s): invalid mode %q (must be 'single' or 'subdirectories')", i, target.Name, target.Mode)
		}
		if target.Type == "postgres" || target.Type == "mysql" || target.Type == "mariadb" || target.Type == "mongodb" {
			if target.Database.Type == "" {
				target.Database.Type = target.Type
			}
		}
	}

	if len(c.Rulesets) > 0 {
		if err := ValidateRetentionRulesets(c.Rulesets); err != nil {
			return err
		}
	}

	return nil
}

// ApplyDefaults applies default settings to a target config
func (c *BackupConfig) ApplyDefaults(target *TargetConfig) {
	if target.Type == "" {
		target.Type = "filesystem"
	}
	if target.Mode == "" {
		target.Mode = "single"
	}
	if target.Database.Type == "" && c.Defaults.DatabaseType != "" {
		target.Database.Type = c.Defaults.DatabaseType
	}
	if target.Database.Name == "" && c.Defaults.DatabaseName != "" {
		target.Database.Name = c.Defaults.DatabaseName
	}
	if target.Database.User == "" && c.Defaults.DatabaseUser != "" {
		target.Database.User = c.Defaults.DatabaseUser
	}
	if target.Paths.DatabaseExportDir == "" && c.Defaults.DatabaseExportDir != "" {
		target.Paths.DatabaseExportDir = c.Defaults.DatabaseExportDir
	}

	// Resolve environment variables in password field (e.g., $PGPASSWORD or ${PGPASSWORD})
	target.Database.Password = resolveEnvVar(target.Database.Password)

	// Apply bucket path default
	if target.BucketPath == "" && c.Defaults.BucketPath != "" {
		target.BucketPath = c.Defaults.BucketPath
	}

	// Merge excludes: Defaults.Excludes + Target.Excludes (deduplicated, preserving order)
	if len(c.Defaults.Excludes) > 0 {
		target.Excludes = MergeUniqueStrings(c.Defaults.Excludes, target.Excludes)
	}

	// Apply GenerateGitRefs default (defaults to true)
	if target.GenerateGitRefs == nil {
		if c.Defaults.GenerateGitRefs != nil {
			val := *c.Defaults.GenerateGitRefs
			target.GenerateGitRefs = &val
		} else {
			val := true
			target.GenerateGitRefs = &val
		}
	}

	// Merge environment variables
	if target.Env == nil {
		target.Env = make(map[string]string)
	}
	for k, v := range c.Defaults.Env {
		if _, exists := target.Env[k]; !exists {
			target.Env[k] = v
		}
	}
}

// ShouldGenerateGitRefs returns true if git reference artifacts should be generated for this target
func (t *TargetConfig) ShouldGenerateGitRefs() bool {
	if t.GenerateGitRefs == nil {
		return true
	}
	return *t.GenerateGitRefs
}

func resolveEnvVar(val string) string {
	if val == "" || !strings.HasPrefix(val, "$") {
		return val
	}
	envVar := strings.TrimPrefix(val, "$")
	envVar = strings.TrimPrefix(envVar, "{")
	envVar = strings.TrimSuffix(envVar, "}")
	if envValue := os.Getenv(envVar); envValue != "" {
		fmt.Printf("[CONFIG] Resolved password from environment variable: %s\n", envVar)
		return envValue
	}
	fmt.Printf("[CONFIG] ⚠️  Warning: Environment variable %s referenced in config but not set\n", envVar)
	return val
}

// MergeUniqueStrings merges multiple string slices while preserving order and removing duplicates
func MergeUniqueStrings(slices ...[]string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, s := range slices {
		for _, item := range s {
			trimmed := strings.TrimSpace(item)
			if trimmed == "" {
				continue
			}
			if !seen[trimmed] {
				seen[trimmed] = true
				result = append(result, trimmed)
			}
		}
	}
	return result
}

// DefaultConfig returns a built-in default configuration for /var/www and /home/maxx
// when no external config file is found.
func DefaultConfig() *BackupConfig {
	homeDir, err := os.UserHomeDir()
	if err != nil || homeDir == "" {
		homeDir = "/home/maxx"
	}
	homeLabel := "home-" + filepath.Base(homeDir)
	generateGit := true

	defaultExcludes := []string{
		".git",
		"node_modules",
		"vendor",
		".venv",
		"venv",
		"__pycache__",
		".pytest_cache",
		".mypy_cache",
		".next",
		".nuxt",
		".svelte-kit",
		".turbo",
		".parcel-cache",
		".cache",
		".npm",
		".pnpm-store",
		".yarn",
		".bun",
		".cargo/registry",
		".cargo/git",
		".rustup",
		"*.tgz",
		"*.tar.gz",
		"*.tar.zst",
		"*.zip",
		"*.swp",
		"*.swo",
		"*.swn",
		"*.swm",
		"*~",
	}

	homeExcludes := []string{
		".Trash",
		".local/share/Trash",
		".local/share/containers",
		".local/share/Docker",
		".var",
		".config/google-chrome",
		".config/chromium",
		".mozilla",
		".vscode/extensions",
		".vscode-insiders",
		".vscode-server",
		".cursor/extensions",
		".zoom",
		".ollama/models",
		"GGUFModels",
		"VirtualBox VMs",
	}

	cfg := &BackupConfig{
		Version: "1",
		Defaults: ConfigDefaults{
			Excludes:        defaultExcludes,
			GenerateGitRefs: &generateGit,
		},
		Targets: []TargetConfig{
			{
				Name:       "var-www",
				Label:      "var-www",
				Mode:       "subdirectories",
				Type:       "filesystem",
				BucketPath: "var-www",
				Paths: PathsConfig{
					WorkingDir: "/var/www",
				},
				SkipDirs: []string{"edfx-labs"},
			},
			{
				Name:       "edfx-labs",
				Label:      "edfx-labs",
				Mode:       "subdirectories",
				Type:       "filesystem",
				BucketPath: "var-www/edfx-labs",
				Paths: PathsConfig{
					WorkingDir: "/var/www/edfx-labs",
				},
			},
			{
				Name:       homeLabel,
				Label:      homeLabel,
				Mode:       "single",
				Type:       "filesystem",
				BucketPath: homeLabel,
				Paths: PathsConfig{
					WorkingDir: homeDir,
				},
				Excludes: homeExcludes,
			},
		},
	}

	for i := range cfg.Targets {
		cfg.ApplyDefaults(&cfg.Targets[i])
	}
	return cfg
}

// LoadRetentionPolicyFromFile loads retention policy from a YAML file
func LoadRetentionPolicyFromFile(path string) (map[string]RetentionRuleset, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read retention policy file: %w", err)
	}

	var config struct {
		Rulesets map[string]RetentionRuleset `yaml:"rulesets"`
	}

	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse retention policy YAML: %w", err)
	}

	if err := ValidateRetentionRulesets(config.Rulesets); err != nil {
		return nil, fmt.Errorf("invalid retention policy: %w", err)
	}

	return config.Rulesets, nil
}

// ValidateRetentionRulesets validates retention policy rulesets
func ValidateRetentionRulesets(rulesets map[string]RetentionRuleset) error {
	for name, ruleset := range rulesets {
		if ruleset.OlderThan == "" {
			return fmt.Errorf("ruleset '%s': older_than is required", name)
		}

		if _, err := ParseHumanDuration(ruleset.OlderThan); err != nil {
			return fmt.Errorf("ruleset '%s': invalid older_than value: %w", name, err)
		}

		if ruleset.Action != "" {
			validActions := map[string]bool{"delete": true, "keep": true, "migrate_to_glacier": true}
			if !validActions[ruleset.Action] {
				return fmt.Errorf("ruleset '%s': invalid action '%s' (must be 'delete' or 'keep')", name, ruleset.Action)
			}
		}

		if ruleset.TargetStorage != "" {
			validTargets := map[string]bool{"s3": true, "minio": true, "both": true, "glacier": true}
			if !validTargets[ruleset.TargetStorage] {
				return fmt.Errorf("ruleset '%s': invalid target_storage '%s'", name, ruleset.TargetStorage)
			}
		}
	}

	return nil
}

// ParseHumanDuration parses human-readable duration strings like "7 days", "6 months", "1 year"
func ParseHumanDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))

	parts := strings.Fields(s)
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid duration format: expected 'number unit' (e.g., '7 days')")
	}

	value, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, fmt.Errorf("invalid number in duration: %w", err)
	}

	unit := parts[1]
	switch unit {
	case "day", "days":
		return time.Duration(value) * 24 * time.Hour, nil
	case "week", "weeks":
		return time.Duration(value) * 7 * 24 * time.Hour, nil
	case "month", "months":
		return time.Duration(value) * 30 * 24 * time.Hour, nil
	case "year", "years":
		return time.Duration(value) * 365 * 24 * time.Hour, nil
	case "hour", "hours":
		return time.Duration(value) * time.Hour, nil
	default:
		return 0, fmt.Errorf("unsupported time unit: %s (supported: days, weeks, months, years, hours)", unit)
	}
}

// EvaluateExclusion checks if a given time should be excluded based on the exclusion rule
// Supports patterns like "Sunday", "every 7 days", "last day of month", "day 1"
func EvaluateExclusion(t time.Time, exclude string) bool {
	if exclude == "" {
		return false
	}

	exclude = strings.TrimSpace(strings.ToLower(exclude))

	weekdays := map[string]time.Weekday{
		"sunday":    time.Sunday,
		"monday":    time.Monday,
		"tuesday":   time.Tuesday,
		"wednesday": time.Wednesday,
		"thursday":  time.Thursday,
		"friday":    time.Friday,
		"saturday":  time.Saturday,
	}

	if weekday, ok := weekdays[exclude]; ok {
		return t.Weekday() == weekday
	}

	if strings.HasPrefix(exclude, "every ") && strings.HasSuffix(exclude, " days") {
		numStr := strings.TrimPrefix(exclude, "every ")
		numStr = strings.TrimSuffix(numStr, " days")
		numStr = strings.TrimSpace(numStr)

		if num, err := strconv.Atoi(numStr); err == nil && num > 0 {
			daysSinceEpoch := int(t.Unix() / (24 * 3600))
			return daysSinceEpoch%num == 0
		}
	}

	if exclude == "last day of month" || exclude == "last day of the month" {
		tomorrow := t.AddDate(0, 0, 1)
		return tomorrow.Month() != t.Month()
	}

	if strings.HasPrefix(exclude, "day ") {
		dayStr := strings.TrimPrefix(exclude, "day ")
		if day, err := strconv.Atoi(dayStr); err == nil {
			return t.Day() == day
		}
	}

	return false
}
