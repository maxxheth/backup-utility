package backup

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// exportWordPressDatabase handles WordPress-specific Docker database export (ported from ciwg-cli)
func (bm *BackupManager) exportWordPressDatabase(target TargetInfo) error {
	fmt.Printf("Cleaning all SQL files in %s...\n", target.Name)
	cleanCmd := fmt.Sprintf(`docker exec -u 0 "%s" find /var/www/html -name "*.sql" -type f -exec rm -f {} \;`, target.Name)
	if _, stderr, err := bm.executeCommand(cleanCmd); err != nil {
		fmt.Printf("Warning: failed to clean old SQL files: %v (stderr: %s)\n", err, stderr)
	}

	fmt.Printf("Removing existing SQL files in %s/www/wp-content...\n", target.WorkingDir)
	hostWPContent := filepath.Join(target.WorkingDir, "www", "wp-content")
	cleanHostCmd := fmt.Sprintf(`if [ -d "%s" ]; then find "%s" -name "*.sql" -type f -exec rm -f {} +; fi`, hostWPContent, hostWPContent)
	if _, stderr, err := bm.executeCommand(cleanHostCmd); err != nil {
		fmt.Printf("Warning: failed to remove existing SQL files from host wp-content: %v (stderr: %s)\n", err, stderr)
	}

	fmt.Printf("Exporting DB in %s...\n", target.Name)
	exportCmd := fmt.Sprintf(`docker exec -u 0 "%s" sh -c 'wp --allow-root db export --skip-ssl /var/www/html/web/app/%s.sql'`, target.Name, target.Name)
	if _, stderr, err := bm.executeCommand(exportCmd); err != nil {
		return fmt.Errorf("failed to export database: %w (stderr: %s)", err, stderr)
	}

	return nil
}

// exportDatabase exports a database based on the target configuration (ported from ciwg-cli)
func (bm *BackupManager) exportDatabase(target TargetInfo, options *BackupOptions) error {
	if target.Config == nil {
		return fmt.Errorf("no configuration provided for target")
	}

	dbConfig := target.Config.Database
	if dbConfig.Type == "" {
		return fmt.Errorf("no database type specified")
	}

	bm.logDebug("exportDatabase called for target: %s, db type: %s", target.Name, dbConfig.Type)

	// Use custom export command if provided
	if dbConfig.ExportCommand != "" {
		bm.logVerbose("Running custom database export command...")
		fmt.Printf("Running custom database export command...\n")
		stdout, stderr, err := bm.executeCommand(dbConfig.ExportCommand)
		if err != nil {
			bm.logDebug("Custom export command stdout: %s", stdout)
			bm.logDebug("Custom export command stderr: %s", stderr)
			return fmt.Errorf("custom export command failed: %w (stderr: %s)", err, stderr)
		}
		bm.logVerbose("✓ Custom export command completed successfully")
		return nil
	}

	// Determine export path
	var exportPath string
	if dbConfig.ExportPath != "" {
		exportPath = dbConfig.ExportPath
	} else if target.Config.Paths.DatabaseExportDir != "" {
		exportPath = filepath.Join(target.Config.Paths.DatabaseExportDir, fmt.Sprintf("%s-export.sql", dbConfig.Name))
	} else {
		exportPath = filepath.Join(target.WorkingDir, fmt.Sprintf("%s-export.sql", dbConfig.Name))
	}

	fmt.Printf("Exporting %s database %s...\n", dbConfig.Type, dbConfig.Name)

	if options.DryRun {
		fmt.Printf("[DRY RUN] Would export database to %s\n", exportPath)
		return nil
	}

	exportDir := filepath.Dir(exportPath)
	if exportDir != "" && exportDir != "." {
		mkdirCmd := fmt.Sprintf(`mkdir -p "%s"`, exportDir)
		if _, stderr, err := bm.executeCommand(mkdirCmd); err != nil {
			return fmt.Errorf("failed to create export directory on host: %w (stderr: %s)", err, stderr)
		}
	}

	var attemptedContainers []string
	var lastErr error
	var lastStderr string

	tryExport := func(targetContainer string, reason string) (bool, error) {
		attemptedContainers = append(attemptedContainers, fmt.Sprintf("%s (%s)", targetContainer, reason))
		bm.logDebug("Attempting export using container: %s (%s)", targetContainer, reason)

		originalContainer := dbConfig.Container
		dbConfig.Container = targetContainer

		var cmd string
		switch strings.ToLower(dbConfig.Type) {
		case "postgres", "postgresql":
			cmd = bm.buildPostgresExportCommand(target, dbConfig, exportPath)
		case "mysql", "mariadb":
			cmd = bm.buildMySQLExportCommand(target, dbConfig, exportPath)
		case "mongodb", "mongo":
			cmd = bm.buildMongoExportCommand(target, dbConfig, exportPath)
		default:
			dbConfig.Container = originalContainer
			return false, fmt.Errorf("unsupported database type: %s", dbConfig.Type)
		}

		dbConfig.Container = originalContainer

		bm.logDebug("Export command: %s", maskPassword(cmd))
		_, stderr, err := bm.executeCommand(cmd)
		if err != nil {
			lastErr = err
			lastStderr = stderr

			if exitErr, ok := err.(*exec.ExitError); ok {
				if exitErr.ExitCode() == 127 {
					bm.logVerbose("⚠️  Database client tool not found in %s", targetContainer)
					return false, fmt.Errorf("exit code 127: command not found")
				}
			}
			return false, err
		}

		bm.logVerbose("✓ Database export completed successfully using %s", targetContainer)
		fmt.Printf("Database exported to %s\n", exportPath)
		return true, nil
	}

	// Priority 1: Explicit container
	if dbConfig.Container != "" {
		success, err := tryExport(dbConfig.Container, "explicit config")
		if success {
			return nil
		}
		if err != nil && !strings.Contains(err.Error(), "exit code 127") {
			return bm.formatExportError(lastErr, lastStderr, attemptedContainers)
		}
	}

	// Priority 2: Auto-detect database container on same Docker network
	if dbConfig.PreferDbContainer {
		detectedContainer := bm.findDatabaseContainer(target)
		if detectedContainer != "" {
			fmt.Printf("  → Using detected database container: %s\n", detectedContainer)
			success, err := tryExport(detectedContainer, "auto-detected")
			if success {
				return nil
			}
			if err != nil && !strings.Contains(err.Error(), "exit code 127") {
				return bm.formatExportError(lastErr, lastStderr, attemptedContainers)
			}
		}
	}

	// Priority 3: App container
	targetContainer := target.Name
	if dbConfig.Container != "" {
		targetContainer = dbConfig.Container
	}

	success, err := tryExport(targetContainer, "app container")
	if success {
		return nil
	}
	if err != nil && !strings.Contains(err.Error(), "exit code 127") {
		return bm.formatExportError(lastErr, lastStderr, attemptedContainers)
	}

	// Priority 4: Auto-fix database tools if enabled
	shouldAutoFix := options.AutoFixDatabaseTools &&
		lastErr != nil &&
		strings.Contains(lastErr.Error(), "exit code 127")

	if !shouldAutoFix {
		return bm.formatExportError(lastErr, lastStderr, attemptedContainers)
	}

	fmt.Printf("\n🔧 Auto-fix enabled: Attempting to install database client tools in %s...\n", targetContainer)
	if err := bm.installDatabaseTools(targetContainer, dbConfig.Type); err != nil {
		return bm.formatExportError(lastErr, lastStderr, attemptedContainers)
	}

	success, retryErr := tryExport(targetContainer, "after auto-fix")
	if success {
		fmt.Printf("\n✓ Auto-fix successful! Database exported.\n")
		return nil
	}

	return bm.formatExportError(retryErr, lastStderr, attemptedContainers)
}

func (bm *BackupManager) buildPostgresExportCommand(target TargetInfo, dbConfig DatabaseConfig, exportPath string) string {
	targetContainer := target.Name
	if dbConfig.Container != "" {
		targetContainer = dbConfig.Container
	}

	var baseCmd string
	if dbConfig.Password != "" {
		baseCmd = fmt.Sprintf(`docker exec -e PGPASSWORD=%s %s pg_dump -U %s -d %s`,
			shellEscape(dbConfig.Password), targetContainer, dbConfig.User, dbConfig.Name)
	} else {
		baseCmd = fmt.Sprintf(`docker exec %s pg_dump -U %s -d %s`,
			targetContainer, dbConfig.User, dbConfig.Name)
	}

	if dbConfig.Host != "" {
		baseCmd += fmt.Sprintf(` -h %s`, dbConfig.Host)
	}
	if dbConfig.Port > 0 {
		baseCmd += fmt.Sprintf(` -p %d`, dbConfig.Port)
	}

	return fmt.Sprintf(`%s > %s`, baseCmd, shellEscape(exportPath))
}

func (bm *BackupManager) buildMySQLExportCommand(target TargetInfo, dbConfig DatabaseConfig, exportPath string) string {
	targetContainer := target.Name
	if dbConfig.Container != "" {
		targetContainer = dbConfig.Container
	}

	cmd := fmt.Sprintf(`docker exec %s mysqldump -u %s`, targetContainer, dbConfig.User)
	if dbConfig.Password != "" {
		cmd += fmt.Sprintf(` -p%s`, shellEscape(dbConfig.Password))
	}
	if dbConfig.Host != "" {
		cmd += fmt.Sprintf(` -h %s`, dbConfig.Host)
	}
	if dbConfig.Port > 0 {
		cmd += fmt.Sprintf(` -P %d`, dbConfig.Port)
	}
	cmd += fmt.Sprintf(` %s > %s`, dbConfig.Name, shellEscape(exportPath))
	return cmd
}

func (bm *BackupManager) buildMongoExportCommand(target TargetInfo, dbConfig DatabaseConfig, exportPath string) string {
	targetContainer := target.Name
	if dbConfig.Container != "" {
		targetContainer = dbConfig.Container
	}

	cmd := fmt.Sprintf(`docker exec %s mongodump --db %s --out %s`, targetContainer, dbConfig.Name, shellEscape(exportPath))
	if dbConfig.User != "" {
		cmd += fmt.Sprintf(` --username %s`, dbConfig.User)
	}
	if dbConfig.Password != "" {
		cmd += fmt.Sprintf(` --password %s`, shellEscape(dbConfig.Password))
	}
	return cmd
}

func (bm *BackupManager) findDatabaseContainer(target TargetInfo) string {
	if target.Config == nil || target.Config.Database.Type == "" {
		return ""
	}

	dbType := strings.ToLower(target.Config.Database.Type)
	cmd := fmt.Sprintf(`docker inspect %s --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}'`, target.Name)
	stdout, _, err := bm.executeCommand(cmd)
	if err != nil {
		return ""
	}

	networks := strings.Fields(strings.TrimSpace(stdout))
	if len(networks) == 0 {
		return ""
	}

	var searchPattern, toolName string
	switch dbType {
	case "postgres", "postgresql":
		searchPattern = `(postgres|pg|db)`
		toolName = "pg_dump"
	case "mysql", "mariadb":
		searchPattern = `(mysql|mariadb|db)`
		toolName = "mysqldump"
	case "mongodb", "mongo":
		searchPattern = `(mongo|db)`
		toolName = "mongodump"
	default:
		return ""
	}

	cmd = fmt.Sprintf(`docker ps --format '{{.Names}}' | grep -iE '%s'`, searchPattern)
	stdout, _, err = bm.executeCommand(cmd)
	if err != nil {
		return ""
	}

	candidates := strings.Split(strings.TrimSpace(stdout), "\n")
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || candidate == target.Name {
			continue
		}

		cmd = fmt.Sprintf(`docker exec %s which %s 2>/dev/null`, candidate, toolName)
		if out, _, err := bm.executeCommand(cmd); err != nil || strings.TrimSpace(out) == "" {
			continue
		}

		cmd = fmt.Sprintf(`docker inspect %s --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}'`, candidate)
		out, _, err := bm.executeCommand(cmd)
		if err != nil {
			continue
		}

		candidateNetworks := strings.Fields(strings.TrimSpace(out))
		for _, netName := range networks {
			for _, candNet := range candidateNetworks {
				if netName == candNet {
					return candidate
				}
			}
		}
	}

	return ""
}

func (bm *BackupManager) installDatabaseTools(containerName string, dbType string) error {
	var pkgManager, installCmd string

	cmd := fmt.Sprintf(`docker exec %s which apt-get 2>/dev/null`, containerName)
	if stdout, _, err := bm.executeCommand(cmd); err == nil && strings.TrimSpace(stdout) != "" {
		pkgManager = "apt-get"
		switch strings.ToLower(dbType) {
		case "postgres", "postgresql":
			installCmd = "apt-get update && apt-get install -y postgresql-client"
		case "mysql", "mariadb":
			installCmd = "apt-get update && apt-get install -y mysql-client"
		case "mongodb", "mongo":
			installCmd = "apt-get update && apt-get install -y mongodb-database-tools"
		}
	}

	if pkgManager == "" {
		cmd = fmt.Sprintf(`docker exec %s which apk 2>/dev/null`, containerName)
		if stdout, _, err := bm.executeCommand(cmd); err == nil && strings.TrimSpace(stdout) != "" {
			pkgManager = "apk"
			switch strings.ToLower(dbType) {
			case "postgres", "postgresql":
				installCmd = "apk add --no-cache postgresql-client"
			case "mysql", "mariadb":
				installCmd = "apk add --no-cache mysql-client"
			case "mongodb", "mongo":
				installCmd = "apk add --no-cache mongodb-tools"
			}
		}
	}

	if pkgManager == "" || installCmd == "" {
		return fmt.Errorf("no supported package manager or database tool found for %s", dbType)
	}

	cmd = fmt.Sprintf(`docker exec %s sh -c '%s'`, containerName, installCmd)
	_, stderr, err := bm.executeCommand(cmd)
	if err != nil {
		return fmt.Errorf("installation failed: %w (stderr: %s)", err, stderr)
	}

	return nil
}

func (bm *BackupManager) formatExportError(err error, stderr string, attemptedContainers []string) error {
	if len(attemptedContainers) > 0 {
		return fmt.Errorf("database export failed (attempted: %s): %w (stderr: %s)", strings.Join(attemptedContainers, ", "), err, stderr)
	}
	return fmt.Errorf("database export failed: %w (stderr: %s)", err, stderr)
}

func shellEscape(s string) string {
	escaped := strings.ReplaceAll(s, "'", "'\\''")
	return fmt.Sprintf("'%s'", escaped)
}

func maskPassword(cmd string) string {
	masked := regexp.MustCompile(`(-e\s+PGPASSWORD=)([^\s]+)`).ReplaceAllString(cmd, `$1***`)
	masked = regexp.MustCompile(`(-p)([^\s]+)`).ReplaceAllString(masked, `${1}***`)
	masked = regexp.MustCompile(`(--password[=\s]+)([^\s]+)`).ReplaceAllString(masked, `${1}***`)
	return masked
}
