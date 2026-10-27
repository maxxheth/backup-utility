package backup

import (
	"testing"
	"time"
)

func TestSelectObjectsForOverwrite(t *testing.T) {
	bm := NewBackupManagerWithS3(&S3Config{Bucket: "edfx"})
	now := time.Now()
	objs := []ObjectInfo{
		{Key: "b1.tgz", LastModified: now.Add(-4 * time.Hour)},
		{Key: "b2.tgz", LastModified: now.Add(-3 * time.Hour)},
		{Key: "b3.tgz", LastModified: now.Add(-2 * time.Hour)},
		{Key: "b4.tgz", LastModified: now.Add(-1 * time.Hour)},
	}

	toDelete := bm.SelectObjectsForOverwrite(objs, 2)
	if len(toDelete) != 2 {
		t.Fatalf("expected 2 objects to delete, got %d", len(toDelete))
	}
	if toDelete[0].Key != "b2.tgz" || toDelete[1].Key != "b1.tgz" {
		t.Errorf("unexpected objects selected for deletion: %+v", toDelete)
	}
}

func TestSmartRetention(t *testing.T) {
	bm := NewBackupManagerWithS3(&S3Config{Bucket: "edfx"})
	// Create 5 daily backups on non-Sunday, non-1st days
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) // Tuesday Oct 20
	objs := []ObjectInfo{
		{Key: "day1.tgz", LastModified: base},
		{Key: "day2.tgz", LastModified: base.AddDate(0, 0, 1)}, // Wed Oct 21
		{Key: "day3.tgz", LastModified: base.AddDate(0, 0, 2)}, // Thu Oct 22
		{Key: "day4.tgz", LastModified: base.AddDate(0, 0, 3)}, // Fri Oct 23
		{Key: "sun.tgz", LastModified: base.AddDate(0, 0, 5)},  // Sun Oct 25 (Weekly)
	}

	policy := &SmartRetentionPolicy{
		Enabled:     true,
		KeepDaily:   2,
		KeepWeekly:  1,
		KeepMonthly: 1,
		WeeklyDay:   0, // Sunday
		MonthlyDay:  1,
	}

	// sun.tgz is kept as weekly (1/1), day4.tgz and day3.tgz are kept as daily (2/2), day2.tgz and day1.tgz should be deleted
	toDelete := bm.SelectObjectsWithSmartRetention(objs, policy)
	if len(toDelete) != 2 {
		t.Fatalf("expected 2 backups to delete, got %d: %+v", len(toDelete), toDelete)
	}
}
