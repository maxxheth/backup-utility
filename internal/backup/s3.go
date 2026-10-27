package backup

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	mathrand "math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const (
	// DefaultHetznerEndpoint is the default Hetzner Object Storage endpoint
	DefaultHetznerEndpoint = "hel1.your-objectstorage.com"
	// DefaultEDFXBucket is the default Hetzner S3 bucket name for EDFX backups
	DefaultEDFXBucket = "edfx"
	// DefaultHetznerRegion is the default Hetzner S3 region
	DefaultHetznerRegion = "hel1"
)

// S3Config represents Hetzner S3 configuration for object storage
type S3Config struct {
	Endpoint    string
	AccessKey   string
	SecretKey   string
	Bucket      string
	Region      string
	UseSSL      bool
	BucketPath  string
	HTTPTimeout time.Duration
}

// ObjectInfo is a lightweight representation of an object in S3 storage
type ObjectInfo struct {
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"last_modified"`
}

// ProgressReader wraps an io.Reader and reports upload/download progress periodically
type ProgressReader struct {
	reader      io.Reader
	total       int64
	read        int64
	lastReport  time.Time
	reportEvery time.Duration
	label       string
}

// NewProgressReader creates a progress tracking reader
func NewProgressReader(r io.Reader, total int64, label string) *ProgressReader {
	return &ProgressReader{
		reader:      r,
		total:       total,
		lastReport:  time.Now(),
		reportEvery: 2 * time.Second,
		label:       label,
	}
}

func (pr *ProgressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	pr.read += int64(n)

	now := time.Now()
	if now.Sub(pr.lastReport) >= pr.reportEvery || err == io.EOF || err != nil {
		pr.report()
		pr.lastReport = now
	}

	return n, err
}

func (pr *ProgressReader) report() {
	if pr.total > 0 {
		percent := float64(pr.read) / float64(pr.total) * 100
		mbRead := float64(pr.read) / (1024 * 1024)
		mbTotal := float64(pr.total) / (1024 * 1024)
		fmt.Printf("   %s: %.2f%% (%.2f / %.2f MB)\n", pr.label, percent, mbRead, mbTotal)
	} else {
		mbRead := float64(pr.read) / (1024 * 1024)
		fmt.Printf("   %s: %.2f MB transferred\n", pr.label, mbRead)
	}
}

// extractS3RegionFromError attempts to pull the correct bucket region from an S3 error
func extractS3RegionFromError(err error) string {
	errResp := minio.ToErrorResponse(err)
	if errResp.Region != "" {
		return errResp.Region
	}
	if errResp.StatusCode == http.StatusMovedPermanently {
		return errResp.Region
	}
	if strings.EqualFold(errResp.Code, "PermanentRedirect") {
		return errResp.Region
	}
	if strings.EqualFold(errResp.Code, "AuthorizationHeaderMalformed") && errResp.Region != "" {
		return errResp.Region
	}
	return ""
}

// normalizeEndpoint strips http:// or https:// scheme prefixes if present
func normalizeEndpoint(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	endpoint = strings.TrimPrefix(endpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "http://")
	endpoint = strings.TrimSuffix(endpoint, "/")
	return endpoint
}

// initS3Client initializes the Hetzner S3 client if not already initialized
func (bm *BackupManager) initS3Client() error {
	if bm.s3Client != nil {
		return nil
	}

	if bm.s3Config == nil {
		return fmt.Errorf("S3 configuration is not set")
	}

	dialer := &net.Dialer{
		Timeout:   60 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   5 * time.Minute,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       5 * time.Minute,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
	}
	if bm.s3Config.HTTPTimeout > 0 {
		tr.ResponseHeaderTimeout = bm.s3Config.HTTPTimeout
	}

	endpoint := normalizeEndpoint(bm.s3Config.Endpoint)
	if endpoint == "" {
		endpoint = DefaultHetznerEndpoint
	}
	bm.s3Config.Endpoint = endpoint

	if bm.s3Config.Bucket == "" {
		bm.s3Config.Bucket = DefaultEDFXBucket
	}

	bm.logDebug("S3 config endpoint: %q, region: %q, bucket: %q, useSSL: %v",
		endpoint, bm.s3Config.Region, bm.s3Config.Bucket, bm.s3Config.UseSSL)

	if bm.verbosity >= 1 {
		fmt.Printf("[S3] Using endpoint: %s (bucket: %s, region: %s, ssl: %v)\n",
			endpoint, bm.s3Config.Bucket, bm.s3Config.Region, bm.s3Config.UseSSL)
	}

	client, err := minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(bm.s3Config.AccessKey, bm.s3Config.SecretKey, ""),
		Secure:       bm.s3Config.UseSSL,
		Transport:    tr,
		Region:       bm.s3Config.Region,
		BucketLookup: minio.BucketLookupAuto,
	})
	if err != nil {
		return fmt.Errorf("failed to create S3 client: %w", err)
	}

	bm.s3Client = client

	// Ensure bucket exists (or create it)
	ctx := context.Background()
	exists, err := bm.s3Client.BucketExists(ctx, bm.s3Config.Bucket)
	if err != nil {
		if region := extractS3RegionFromError(err); region != "" && region != bm.s3Config.Region {
			bm.logVerbose("S3 bucket region mismatch detected: bucket requires %s but configured %s; retrying", region, bm.s3Config.Region)
			bm.s3Config.Region = region
			bm.s3Client = nil
			return bm.initS3Client()
		}
		return fmt.Errorf("failed to check if S3 bucket %q exists: %w", bm.s3Config.Bucket, err)
	}

	if !exists {
		bm.logVerbose("S3 bucket %s missing; attempting to create", bm.s3Config.Bucket)
		makeOpts := minio.MakeBucketOptions{}
		if bm.s3Config.Region != "" {
			makeOpts.Region = bm.s3Config.Region
		}
		if err := bm.s3Client.MakeBucket(ctx, bm.s3Config.Bucket, makeOpts); err != nil {
			return fmt.Errorf("failed to create S3 bucket %s: %w", bm.s3Config.Bucket, err)
		}
	}

	return nil
}

// TestS3Connection tests connectivity to the Hetzner S3 bucket with write, read, verify, and delete steps
func (bm *BackupManager) TestS3Connection() error {
	if err := bm.initS3Client(); err != nil {
		return err
	}

	ctx := context.Background()

	// Step 1: Test bucket existence
	fmt.Printf("1. Testing S3 bucket existence...\n")
	exists, err := bm.s3Client.BucketExists(ctx, bm.s3Config.Bucket)
	if err != nil {
		return fmt.Errorf("failed to check S3 bucket existence: %w", err)
	}
	if !exists {
		return fmt.Errorf("S3 bucket '%s' does not exist", bm.s3Config.Bucket)
	}
	fmt.Printf("   ✓ S3 bucket '%s' exists\n\n", bm.s3Config.Bucket)

	// Step 2: Test write operation
	fmt.Printf("2. Testing S3 write operation...\n")
	testObjectName := fmt.Sprintf(".connection-test-%d.txt", time.Now().Unix())
	if bm.s3Config.BucketPath != "" {
		testObjectName = filepath.ToSlash(filepath.Join(bm.s3Config.BucketPath, testObjectName))
	}

	testContent := []byte("This is a connection test file created by edfx-backup")

	info, err := bm.s3Client.PutObject(ctx, bm.s3Config.Bucket, testObjectName,
		strings.NewReader(string(testContent)), int64(len(testContent)), minio.PutObjectOptions{
			ContentType: "text/plain",
		})
	if err != nil {
		return fmt.Errorf("failed to write test object to S3: %w", err)
	}
	fmt.Printf("   ✓ Successfully wrote test object '%s' to S3 (%d bytes)\n\n", testObjectName, info.Size)

	// Step 3: Test read operation
	fmt.Printf("3. Testing S3 read operation...\n")
	object, err := bm.s3Client.GetObject(ctx, bm.s3Config.Bucket, testObjectName, minio.GetObjectOptions{})
	if err != nil {
		return fmt.Errorf("failed to read test object from S3: %w", err)
	}
	defer object.Close()

	readContent, err := io.ReadAll(object)
	if err != nil {
		return fmt.Errorf("failed to read test object content from S3: %w", err)
	}
	if len(readContent) != len(testContent) {
		return fmt.Errorf("read size mismatch: expected %d, got %d", len(testContent), len(readContent))
	}
	if string(readContent) != string(testContent) {
		return fmt.Errorf("content mismatch: read content doesn't match written content")
	}
	fmt.Printf("   ✓ Successfully read test object from S3 and verified content\n\n")

	// Step 4: Test delete operation
	fmt.Printf("4. Testing S3 delete operation...\n")
	err = bm.s3Client.RemoveObject(ctx, bm.s3Config.Bucket, testObjectName, minio.RemoveObjectOptions{})
	if err != nil {
		return fmt.Errorf("failed to delete test object from S3: %w", err)
	}
	fmt.Printf("   ✓ Successfully deleted test object from S3\n")

	return nil
}

// SimulateS3RoundTrip performs a full S3 round trip with a random payload, SHA256 checksum verification, and optional cleanup
func (bm *BackupManager) SimulateS3RoundTrip(ctx context.Context, keep bool, maxSizeMB int) error {
	if err := bm.initS3Client(); err != nil {
		return err
	}

	if maxSizeMB < 1 {
		maxSizeMB = 1
	}
	if maxSizeMB > 100 {
		maxSizeMB = 100
	}

	fmt.Printf("1. Checking S3 bucket existence...\n")
	exists, err := bm.s3Client.BucketExists(ctx, bm.s3Config.Bucket)
	if err != nil {
		return fmt.Errorf("failed to check S3 bucket existence: %w", err)
	}
	if !exists {
		return fmt.Errorf("S3 bucket '%s' does not exist", bm.s3Config.Bucket)
	}
	fmt.Printf("   ✓ Bucket '%s' exists\n\n", bm.s3Config.Bucket)

	rng := mathrand.New(mathrand.NewSource(time.Now().UnixNano()))
	sizeMB := rng.Intn(maxSizeMB) + 1
	sizeBytes := int64(sizeMB) * 1024 * 1024
	fmt.Printf("2. Generating random payload (%d MB)...\n", sizeMB)
	data := make([]byte, sizeBytes)
	if _, err := cryptorand.Read(data); err != nil {
		return fmt.Errorf("failed to generate random payload: %w", err)
	}

	checksum := sha256.Sum256(data)
	checksumHex := hex.EncodeToString(checksum[:])

	objectName := fmt.Sprintf(".simulate-%d.bin", time.Now().Unix())
	if bm.s3Config.BucketPath != "" {
		objectName = filepath.ToSlash(filepath.Join(bm.s3Config.BucketPath, objectName))
	}

	fmt.Printf("   Uploading object '%s' (%d bytes)\n", objectName, sizeBytes)
	info, err := bm.s3Client.PutObject(ctx, bm.s3Config.Bucket, objectName,
		bytes.NewReader(data), sizeBytes, minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return fmt.Errorf("failed to upload simulate object to S3: %w", err)
	}
	fmt.Printf("   ✓ Uploaded (%d bytes written)\n", info.Size)
	fmt.Printf("   SHA256 (hex): %s\n\n", checksumHex)

	fmt.Printf("3. Listing S3 objects (first 5)...\n")
	listOpts := minio.ListObjectsOptions{Prefix: bm.s3Config.BucketPath, Recursive: true}
	count := 0
	for object := range bm.s3Client.ListObjects(ctx, bm.s3Config.Bucket, listOpts) {
		if object.Err != nil {
			return fmt.Errorf("failed during S3 list: %w", object.Err)
		}
		if count < 5 {
			fmt.Printf("   - %s (%d bytes)\n", object.Key, object.Size)
		}
		count++
	}
	if count == 0 {
		fmt.Printf("   (no objects in bucket)\n")
	}
	fmt.Println()

	fmt.Printf("4. Downloading object and verifying checksum...\n")
	obj, err := bm.s3Client.GetObject(ctx, bm.s3Config.Bucket, objectName, minio.GetObjectOptions{})
	if err != nil {
		return fmt.Errorf("failed to download simulate object from S3: %w", err)
	}
	defer obj.Close()

	downData, err := io.ReadAll(obj)
	if err != nil {
		return fmt.Errorf("failed to read downloaded object: %w", err)
	}
	downChecksum := sha256.Sum256(downData)
	if hex.EncodeToString(downChecksum[:]) != checksumHex {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", checksumHex, hex.EncodeToString(downChecksum[:]))
	}
	fmt.Printf("   ✓ Checksum verified (%s)\n\n", checksumHex)

	if keep {
		fmt.Println("5. Cleanup skipped (--simulate-keep enabled)")
		return nil
	}

	fmt.Printf("5. Cleaning up simulate object...\n")
	if err := bm.s3Client.RemoveObject(ctx, bm.s3Config.Bucket, objectName, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("failed to delete simulate object: %w", err)
	}
	fmt.Println("   ✓ Deleted simulate object")

	return nil
}

// ReadBackup downloads or streams an object from S3. If outputPath is empty it writes to stdout.
func (bm *BackupManager) ReadBackup(objectName, outputPath string) error {
	if err := bm.initS3Client(); err != nil {
		return err
	}

	ctx := context.Background()
	obj, err := bm.s3Client.GetObject(ctx, bm.s3Config.Bucket, objectName, minio.GetObjectOptions{})
	if err != nil {
		return fmt.Errorf("failed to get object '%s': %w", objectName, err)
	}
	defer obj.Close()

	if outputPath == "" {
		if _, err := io.Copy(os.Stdout, obj); err != nil {
			return fmt.Errorf("failed to stream object to stdout: %w", err)
		}
		return nil
	}

	if dir := filepath.Dir(outputPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("failed to create output directory: %w", err)
		}
	}

	f, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, obj); err != nil {
		return fmt.Errorf("failed to write object to file: %w", err)
	}

	fmt.Printf("Successfully downloaded %s to %s\n", objectName, outputPath)
	return nil
}

// DownloadBackup opens a stream to an object in S3 storage
func (bm *BackupManager) DownloadBackup(objectName string) (io.ReadCloser, error) {
	if err := bm.initS3Client(); err != nil {
		return nil, err
	}

	ctx := context.Background()
	obj, err := bm.s3Client.GetObject(ctx, bm.s3Config.Bucket, objectName, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get object '%s': %w", objectName, err)
	}
	return obj, nil
}

// ListBackups lists objects in the configured S3 bucket filtered by prefix
func (bm *BackupManager) ListBackups(prefix string, limit int) ([]ObjectInfo, error) {
	if err := bm.initS3Client(); err != nil {
		return nil, err
	}

	ctx := context.Background()
	opts := minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}

	var results []ObjectInfo
	ch := bm.s3Client.ListObjects(ctx, bm.s3Config.Bucket, opts)
	for obj := range ch {
		if obj.Err != nil {
			return nil, fmt.Errorf("error listing object: %w", obj.Err)
		}
		results = append(results, ObjectInfo{
			Key:          obj.Key,
			Size:         obj.Size,
			LastModified: obj.LastModified,
		})
		if limit > 0 && len(results) >= limit {
			break
		}
	}

	return results, nil
}

// GetLatestObject returns the key of the most recently modified object with the given prefix
func (bm *BackupManager) GetLatestObject(prefix string) (string, error) {
	objs, err := bm.ListBackups(prefix, 0)
	if err != nil {
		return "", err
	}
	if len(objs) == 0 {
		return "", fmt.Errorf("no objects found for prefix '%s'", prefix)
	}

	latest := objs[0]
	for _, o := range objs[1:] {
		if o.LastModified.After(latest.LastModified) {
			latest = o
		}
	}

	return latest.Key, nil
}

// DeleteObject removes a single object from the configured S3 bucket
func (bm *BackupManager) DeleteObject(objectName string) error {
	if err := bm.initS3Client(); err != nil {
		return err
	}

	ctx := context.Background()
	if err := bm.s3Client.RemoveObject(ctx, bm.s3Config.Bucket, objectName, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("failed to delete object '%s': %w", objectName, err)
	}
	return nil
}

// DeleteObjects removes multiple objects from the configured S3 bucket using batch RemoveObjects
func (bm *BackupManager) DeleteObjects(objectNames []string) error {
	if len(objectNames) == 0 {
		return nil
	}
	if err := bm.initS3Client(); err != nil {
		return err
	}

	ctx := context.Background()
	objectsCh := make(chan minio.ObjectInfo, len(objectNames))
	go func() {
		defer close(objectsCh)
		for _, k := range objectNames {
			objectsCh <- minio.ObjectInfo{Key: k}
		}
	}()

	errCh := bm.s3Client.RemoveObjects(ctx, bm.s3Config.Bucket, objectsCh, minio.RemoveObjectsOptions{})

	var errs []string
	for e := range errCh {
		errs = append(errs, fmt.Sprintf("%s: %v", e.ObjectName, e.Err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("errors deleting objects: %s", strings.Join(errs, "; "))
	}

	return nil
}
