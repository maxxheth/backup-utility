package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"edfx-backup/internal/backup"
)

// findEnvArg inspects argv for an explicit --env argument and returns the value if present
func findEnvArg(argv []string) string {
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if strings.HasPrefix(a, "--env=") {
			return strings.TrimPrefix(a, "--env=")
		}
		if a == "--env" && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

func mustGetStringFlag(cmd *cobra.Command, name string) string {
	val, _ := cmd.Flags().GetString(name)
	return val
}

func mustGetStringSliceFlag(cmd *cobra.Command, name string) []string {
	val, _ := cmd.Flags().GetStringSlice(name)
	return val
}

func mustGetBoolFlag(cmd *cobra.Command, name string) bool {
	val, _ := cmd.Flags().GetBool(name)
	return val
}

func mustGetFloat64Flag(cmd *cobra.Command, name string) float64 {
	val, _ := cmd.Flags().GetFloat64(name)
	return val
}

func mustGetDurationFlag(cmd *cobra.Command, name string) time.Duration {
	val, _ := cmd.Flags().GetDuration(name)
	return val
}

func mustGetIntFlag(cmd *cobra.Command, name string) int {
	val, _ := cmd.Flags().GetInt(name)
	return val
}

func mustGetInt64Flag(cmd *cobra.Command, name string) int64 {
	val, _ := cmd.Flags().GetInt64(name)
	return val
}

func mustGetCountFlag(cmd *cobra.Command, name string) int {
	val, _ := cmd.Flags().GetCount(name)
	return val
}

func getEnvWithDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getFirstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func getEnvBoolWithDefault(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		return strings.ToLower(value) == "true" || value == "1"
	}
	return defaultValue
}

func getEnvDurationWithDefault(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if duration, err := time.ParseDuration(value); err == nil {
			return duration
		}
	}
	return defaultValue
}

func getEnvFloat64WithDefault(key string, defaultValue float64) float64 {
	if value := os.Getenv(key); value != "" {
		if floatVal, err := strconv.ParseFloat(value, 64); err == nil {
			return floatVal
		}
	}
	return defaultValue
}

func getEnvIntWithDefault(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intVal, err := strconv.Atoi(value); err == nil {
			return intVal
		}
	}
	return defaultValue
}

// addS3Flags registers the standard Hetzner S3 flags on a cobra command
func addS3Flags(cmd *cobra.Command) {
	defaultEndpoint := getFirstEnv("HETZNER_S3_ENDPOINT", "S3_ENDPOINT")
	if defaultEndpoint == "" {
		defaultEndpoint = backup.DefaultHetznerEndpoint
	}
	defaultBucket := getFirstEnv("EDFX_S3_BUCKET", "HETZNER_S3_BUCKET", "S3_BUCKET")
	if defaultBucket == "" {
		defaultBucket = backup.DefaultEDFXBucket
	}
	defaultRegion := getFirstEnv("HETZNER_S3_REGION", "S3_REGION")
	if defaultRegion == "" {
		defaultRegion = backup.DefaultHetznerRegion
	}

	cmd.Flags().String("s3-endpoint", defaultEndpoint, "Hetzner S3 endpoint (env: HETZNER_S3_ENDPOINT or S3_ENDPOINT)")
	cmd.Flags().String("s3-access-key", "", "Hetzner S3 access key (env: HETZNER_S3_ACCESS_KEY or S3_ACCESS_KEY)")
	cmd.Flags().String("s3-secret-key", "", "Hetzner S3 secret key (env: HETZNER_S3_SECRET_KEY or S3_SECRET_KEY)")
	cmd.Flags().String("s3-bucket", defaultBucket, "Hetzner S3 bucket name (default: edfx, env: EDFX_S3_BUCKET, HETZNER_S3_BUCKET, or S3_BUCKET)")
	cmd.Flags().String("s3-region", defaultRegion, "Hetzner S3 region (default: hel1, env: HETZNER_S3_REGION or S3_REGION)")
	cmd.Flags().Bool("s3-ssl", getEnvBoolWithDefault("S3_SSL", true), "Use SSL for S3 connection (env: S3_SSL, default: true)")
	cmd.Flags().Duration("s3-http-timeout", getEnvDurationWithDefault("S3_HTTP_TIMEOUT", 0), "S3 HTTP client timeout (env: S3_HTTP_TIMEOUT)")
	cmd.Flags().String("s3-bucket-path", getEnvWithDefault("S3_BUCKET_PATH", ""), "Optional path prefix inside the S3 bucket (env: S3_BUCKET_PATH)")
}

// getS3Config builds an S3Config from flags and environment variables
func getS3Config(cmd *cobra.Command, allowUnauthenticatedDryRun bool) (*backup.S3Config, error) {
	bucket := mustGetStringFlag(cmd, "s3-bucket")
	if bucket == "" {
		bucket = getFirstEnv("EDFX_S3_BUCKET", "HETZNER_S3_BUCKET", "S3_BUCKET")
	}
	if bucket == "" {
		bucket = backup.DefaultEDFXBucket
	}

	endpoint := mustGetStringFlag(cmd, "s3-endpoint")
	if endpoint == "" {
		endpoint = getFirstEnv("HETZNER_S3_ENDPOINT", "S3_ENDPOINT")
	}
	if endpoint == "" {
		endpoint = backup.DefaultHetznerEndpoint
	}

	region := mustGetStringFlag(cmd, "s3-region")
	if region == "" {
		region = getFirstEnv("HETZNER_S3_REGION", "S3_REGION")
	}
	if region == "" {
		region = backup.DefaultHetznerRegion
	}

	accessKey := mustGetStringFlag(cmd, "s3-access-key")
	if accessKey == "" {
		accessKey = getFirstEnv("EDFX_S3_ACCESS_KEY", "HETZNER_S3_ACCESS_KEY", "S3_ACCESS_KEY", "AWS_ACCESS_KEY_ID", "AWS_ACCESS_KEY")
	}

	secretKey := mustGetStringFlag(cmd, "s3-secret-key")
	if secretKey == "" {
		secretKey = getFirstEnv("EDFX_S3_SECRET_KEY", "HETZNER_S3_SECRET_KEY", "S3_SECRET_KEY", "AWS_SECRET_ACCESS_KEY")
	}

	if !allowUnauthenticatedDryRun {
		if accessKey == "" {
			return nil, fmt.Errorf("S3 access key is required (set HETZNER_S3_ACCESS_KEY / S3_ACCESS_KEY in .env or use --s3-access-key)")
		}
		if secretKey == "" {
			return nil, fmt.Errorf("S3 secret key is required (set HETZNER_S3_SECRET_KEY / S3_SECRET_KEY in .env or use --s3-secret-key)")
		}
	}

	useSSL := mustGetBoolFlag(cmd, "s3-ssl")
	httpTimeout := mustGetDurationFlag(cmd, "s3-http-timeout")
	bucketPath := mustGetStringFlag(cmd, "s3-bucket-path")

	return &backup.S3Config{
		Endpoint:    endpoint,
		AccessKey:   accessKey,
		SecretKey:   secretKey,
		Bucket:      bucket,
		Region:      region,
		UseSSL:      useSSL,
		BucketPath:  bucketPath,
		HTTPTimeout: httpTimeout,
	}, nil
}

// resolveDefaultConfigPath returns the path to a default backup-config.yml if one exists on disk
func resolveDefaultConfigPath(explicitFlag string) string {
	if explicitFlag != "" {
		return explicitFlag
	}
	if envCfg := os.Getenv("EDFX_BACKUP_CONFIG"); envCfg != "" {
		return envCfg
	}
	candidates := []string{
		"configs/backup-config.yml",
		"backup-config.yml",
		"/var/www/edfx-labs/backup-utility/configs/backup-config.yml",
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".config", "edfx-backup", "backup-config.yml"))
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	return ""
}

// parseSize parses size strings like "125MB", "1.5GB" into bytes
func parseSize(sizeStr string) (int64, error) {
	sizeStr = strings.TrimSpace(strings.ToUpper(sizeStr))

	multiplier := int64(1)
	switch {
	case strings.HasSuffix(sizeStr, "TB"):
		multiplier = 1024 * 1024 * 1024 * 1024
		sizeStr = strings.TrimSuffix(sizeStr, "TB")
	case strings.HasSuffix(sizeStr, "GB"):
		multiplier = 1024 * 1024 * 1024
		sizeStr = strings.TrimSuffix(sizeStr, "GB")
	case strings.HasSuffix(sizeStr, "MB"):
		multiplier = 1024 * 1024
		sizeStr = strings.TrimSuffix(sizeStr, "MB")
	case strings.HasSuffix(sizeStr, "KB"):
		multiplier = 1024
		sizeStr = strings.TrimSuffix(sizeStr, "KB")
	case strings.HasSuffix(sizeStr, "B"):
		sizeStr = strings.TrimSuffix(sizeStr, "B")
	}

	value, err := strconv.ParseFloat(strings.TrimSpace(sizeStr), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size value: %s", sizeStr)
	}

	return int64(value * float64(multiplier)), nil
}

func formatBytes(b int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
		TB = 1024 * GB
	)
	switch {
	case b >= TB:
		return fmt.Sprintf("%.2f TB", float64(b)/float64(TB))
	case b >= GB:
		return fmt.Sprintf("%.2f GB", float64(b)/float64(GB))
	case b >= MB:
		return fmt.Sprintf("%.2f MB", float64(b)/float64(MB))
	case b >= KB:
		return fmt.Sprintf("%.2f KB", float64(b)/float64(KB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
