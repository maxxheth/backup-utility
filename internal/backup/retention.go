package backup

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SmartRetentionPolicy defines intelligent backup retention based on backup dates.
// Allows preserving weekly and monthly backups from a single daily backup job.
type SmartRetentionPolicy struct {
	Enabled     bool
	KeepDaily   int
	KeepWeekly  int
	KeepMonthly int
	WeeklyDay   int
	MonthlyDay  int
}

// ParseNumericRange parses a numeric range string like "1-10" and returns 1-based start and end indices
func (bm *BackupManager) ParseNumericRange(rangeStr string) (int, int, error) {
	parts := strings.Split(rangeStr, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("range must be in format 'N-M' (e.g., '1-10')")
	}

	start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid start value: %w", err)
	}

	end, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid end value: %w", err)
	}

	if start < 1 {
		return 0, 0, fmt.Errorf("start must be >= 1")
	}

	if end < start {
		return 0, 0, fmt.Errorf("end must be >= start")
	}

	return start, end, nil
}

// SelectObjectsByNumericRange selects objects by numeric range (1-based, where 1 is most recent)
func (bm *BackupManager) SelectObjectsByNumericRange(objs []ObjectInfo, start, end int) ([]ObjectInfo, error) {
	if len(objs) == 0 {
		return nil, fmt.Errorf("no objects available")
	}

	sorted := make([]ObjectInfo, len(objs))
	copy(sorted, objs)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].LastModified.After(sorted[j].LastModified)
	})

	startIdx := start - 1
	endIdx := end - 1

	if startIdx >= len(sorted) {
		return nil, fmt.Errorf("start index %d exceeds number of objects (%d)", start, len(sorted))
	}

	if endIdx >= len(sorted) {
		endIdx = len(sorted) - 1
	}

	return sorted[startIdx : endIdx+1], nil
}

// ParseDateRange parses a date range string in format YYYYMMDD-YYYYMMDD or YYYYMMDD:HHMMSS-YYYYMMDD:HHMMSS
func (bm *BackupManager) ParseDateRange(rangeStr string) (time.Time, time.Time, error) {
	parts := strings.Split(rangeStr, "-")
	if len(parts) != 2 {
		return time.Time{}, time.Time{}, fmt.Errorf("range must be in format 'YYYYMMDD-YYYYMMDD' or 'YYYYMMDD:HHMMSS-YYYYMMDD:HHMMSS'")
	}

	startStr := strings.TrimSpace(parts[0])
	endStr := strings.TrimSpace(parts[1])

	var layout string
	if strings.Contains(startStr, ":") {
		layout = "20060102:150405"
	} else {
		layout = "20060102"
	}

	startTime, err := time.Parse(layout, startStr)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid start date: %w", err)
	}

	endTime, err := time.Parse(layout, endStr)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid end date: %w", err)
	}

	if layout == "20060102" {
		endTime = endTime.Add(24*time.Hour - time.Second)
	}

	if endTime.Before(startTime) {
		return time.Time{}, time.Time{}, fmt.Errorf("end date must be after start date")
	}

	return startTime, endTime, nil
}

// FilterObjectsByDateRange filters objects to only include those with LastModified between start and end times (inclusive)
func (bm *BackupManager) FilterObjectsByDateRange(objs []ObjectInfo, start, end time.Time) []ObjectInfo {
	var filtered []ObjectInfo
	for _, o := range objs {
		if (o.LastModified.Equal(start) || o.LastModified.After(start)) &&
			(o.LastModified.Equal(end) || o.LastModified.Before(end)) {
			filtered = append(filtered, o)
		}
	}
	return filtered
}

// SelectObjectsForOverwrite selects objects for deletion when keeping only the N most recent backups
func (bm *BackupManager) SelectObjectsForOverwrite(objs []ObjectInfo, remainder int) []ObjectInfo {
	if len(objs) <= remainder {
		return []ObjectInfo{}
	}

	sorted := make([]ObjectInfo, len(objs))
	copy(sorted, objs)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].LastModified.After(sorted[j].LastModified)
	})

	return sorted[remainder:]
}

// SelectObjectsWithSmartRetention selects backups to delete using date-aware retention policy
func (bm *BackupManager) SelectObjectsWithSmartRetention(objs []ObjectInfo, policy *SmartRetentionPolicy) []ObjectInfo {
	if policy == nil || !policy.Enabled {
		return []ObjectInfo{}
	}

	sorted := make([]ObjectInfo, len(objs))
	copy(sorted, objs)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].LastModified.After(sorted[j].LastModified)
	})

	type classifiedBackup struct {
		obj       ObjectInfo
		isDaily   bool
		isWeekly  bool
		isMonthly bool
	}

	var classified []classifiedBackup
	for _, obj := range sorted {
		c := classifiedBackup{obj: obj, isDaily: true}
		if obj.LastModified.Day() == policy.MonthlyDay {
			c.isMonthly = true
		}
		if int(obj.LastModified.Weekday()) == policy.WeeklyDay {
			c.isWeekly = true
		}
		classified = append(classified, c)
	}

	var toKeep []ObjectInfo
	var toDelete []ObjectInfo

	dailyCount := 0
	weeklyCount := 0
	monthlyCount := 0
	seenWeeklyDay := make(map[string]bool)
	seenMonthlyDay := make(map[string]bool)

	for _, c := range classified {
		shouldKeep := false
		dayKey := c.obj.LastModified.UTC().Format("2006-01-02")
		monthKey := c.obj.LastModified.UTC().Format("2006-01")

		if c.isMonthly && monthlyCount < policy.KeepMonthly && !seenMonthlyDay[monthKey] {
			shouldKeep = true
			monthlyCount++
			seenMonthlyDay[monthKey] = true
		} else if c.isWeekly && weeklyCount < policy.KeepWeekly && !c.isMonthly && !seenWeeklyDay[dayKey] {
			shouldKeep = true
			weeklyCount++
			seenWeeklyDay[dayKey] = true
		} else if dailyCount < policy.KeepDaily {
			shouldKeep = true
			dailyCount++
		}

		if shouldKeep {
			toKeep = append(toKeep, c.obj)
		} else {
			toDelete = append(toDelete, c.obj)
		}
	}

	bm.logVerbose("Smart retention: keeping %d backups (daily=%d, weekly=%d, monthly=%d), deleting %d",
		len(toKeep), dailyCount, weeklyCount, monthlyCount, len(toDelete))

	return toDelete
}

// ApplyRetentionPolicy evaluates retention policy rulesets and returns backups to delete
func (bm *BackupManager) ApplyRetentionPolicy(objs []ObjectInfo, rulesets map[string]RetentionRuleset) []ObjectInfo {
	now := time.Now()
	matched := make(map[string]bool)
	var deleteList []ObjectInfo

	keys := make([]string, 0, len(rulesets))
	for k := range rulesets {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		ruleset := rulesets[k]
		if ruleset.TargetStorage != "" && ruleset.TargetStorage != "both" && ruleset.TargetStorage != "s3" && ruleset.TargetStorage != "glacier" {
			continue
		}

		duration, err := ParseHumanDuration(ruleset.OlderThan)
		if err != nil {
			bm.logDebug("Invalid duration in ruleset %s: %v", k, err)
			continue
		}

		threshold := now.Add(-duration)

		for _, obj := range objs {
			if matched[obj.Key] {
				continue
			}

			if !obj.LastModified.Before(threshold) {
				continue
			}

			if ruleset.Exclude != "" && EvaluateExclusion(obj.LastModified, ruleset.Exclude) {
				matched[obj.Key] = true
				bm.logVerbose("    → %s: KEEP (excluded by: %s)", obj.Key, ruleset.Exclude)
				continue
			}

			action := ruleset.Action
			if action == "" {
				action = "delete"
			}

			matched[obj.Key] = true
			switch action {
			case "delete":
				deleteList = append(deleteList, obj)
				bm.logVerbose("    → %s: DELETE (older than %s, age: %s)", obj.Key, ruleset.OlderThan, time.Since(obj.LastModified).Round(time.Hour))
			case "keep", "migrate_to_glacier":
				bm.logVerbose("    → %s: KEEP (rule: %s)", obj.Key, k)
			}
		}
	}

	return deleteList
}
