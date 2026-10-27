package backup

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfigFromFileAndValidate(t *testing.T) {
	cfgPath := filepath.Join("..", "..", "configs", "backup-config.yml")
	cfg, err := LoadConfigFromFile(cfgPath)
	if err != nil {
		t.Fatalf("failed to load configs/backup-config.yml: %v", err)
	}

	if len(cfg.Targets) != 3 {
		t.Fatalf("expected 3 targets in backup-config.yml (var-www, edfx-labs, home-maxx), got %d", len(cfg.Targets))
	}

	varWWW := cfg.Targets[0]
	cfg.ApplyDefaults(&varWWW)
	if varWWW.Mode != "subdirectories" {
		t.Errorf("expected var-www mode 'subdirectories', got %q", varWWW.Mode)
	}
	if !varWWW.ShouldGenerateGitRefs() {
		t.Errorf("expected ShouldGenerateGitRefs() to be true")
	}
	if !containsExclude(varWWW.Excludes, ".git") || !containsExclude(varWWW.Excludes, "node_modules") {
		t.Errorf("expected default excludes (.git, node_modules) to be merged into var-www target")
	}

	edfxLabs := cfg.Targets[1]
	cfg.ApplyDefaults(&edfxLabs)
	if edfxLabs.Name != "edfx-labs" || edfxLabs.Mode != "subdirectories" {
		t.Errorf("expected edfx-labs mode 'subdirectories', got name=%q mode=%q", edfxLabs.Name, edfxLabs.Mode)
	}
	if edfxLabs.BucketPath != "var-www/edfx-labs" {
		t.Errorf("expected edfx-labs bucket_path 'var-www/edfx-labs', got %q", edfxLabs.BucketPath)
	}

	homeMaxx := cfg.Targets[2]
	cfg.ApplyDefaults(&homeMaxx)
	if homeMaxx.Mode != "single" {
		t.Errorf("expected home-maxx mode 'single', got %q", homeMaxx.Mode)
	}
	if !containsExclude(homeMaxx.Excludes, ".config/google-chrome") || !containsExclude(homeMaxx.Excludes, ".git") {
		t.Errorf("expected both default and home-specific excludes in home-maxx target")
	}
}

func TestExpandSubdirectoryTargetWithNestedGroups(t *testing.T) {
	tmpRoot := t.TempDir()
	// Create /var/www-like structure:
	//   tmpRoot/proj-a
	//   tmpRoot/proj-b
	//   tmpRoot/edfx-labs/sub-1
	//   tmpRoot/edfx-labs/sub-2
	//   tmpRoot/skip-me
	for _, rel := range []string{
		"proj-a",
		"proj-b",
		"edfx-labs/sub-1",
		"edfx-labs/sub-2",
		"skip-me",
	} {
		if err := os.MkdirAll(filepath.Join(tmpRoot, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	bm := NewBackupManagerWithS3(&S3Config{Bucket: "edfx"})
	tCfg := TargetConfig{
		Name:         "var-www",
		Mode:         "subdirectories",
		Type:         "filesystem",
		BucketPath:   "var-www",
		Paths:        PathsConfig{WorkingDir: tmpRoot},
		NestedGroups: []string{"edfx-labs"},
		SkipDirs:     []string{"skip-me"},
	}

	expanded, err := bm.expandTargetConfig(tCfg)
	if err != nil {
		t.Fatalf("expandTargetConfig failed: %v", err)
	}

	if len(expanded) != 4 {
		t.Fatalf("expected 4 expanded targets (proj-a, proj-b, edfx-labs-sub-1, edfx-labs-sub-2), got %d", len(expanded))
	}

	labels := make(map[string]string)
	for _, item := range expanded {
		labels[item.Label] = item.BucketPath
	}

	if labels["proj-a"] != "var-www/proj-a" {
		t.Errorf("unexpected bucket path for proj-a: %q", labels["proj-a"])
	}
	if labels["sub-1"] != "var-www/edfx-labs/sub-1" {
		t.Errorf("unexpected bucket path for sub-1: %q", labels["sub-1"])
	}
}

func TestParseHumanDurationAndEvaluateExclusion(t *testing.T) {
	d, err := ParseHumanDuration("7 days")
	if err != nil || d != 7*24*time.Hour {
		t.Fatalf("expected 7 days duration, got %v (err: %v)", d, err)
	}

	// Sunday check: 2026-10-25 is a Sunday
	sun := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	if !EvaluateExclusion(sun, "Sunday") {
		t.Errorf("expected 2026-10-25 to match Sunday exclusion")
	}

	// Last day of month check: 2026-10-31
	octEnd := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	if !EvaluateExclusion(octEnd, "last day of month") {
		t.Errorf("expected 2026-10-31 to match last day of month exclusion")
	}
}
