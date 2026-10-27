package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// GitBackupRefFileName is written inside each discovered git repository's relative directory
	GitBackupRefFileName = ".git-backup-ref.txt"
	// GitManifestTextFileName is written at the root of the archive summarizing all discovered git repos
	GitManifestTextFileName = "GIT_REPOS_MANIFEST.txt"
	// GitManifestJSONFileName is written at the root of the archive in machine-readable JSON format
	GitManifestJSONFileName = "GIT_REPOS_MANIFEST.json"
)

// GitRepoMetadata holds extracted metadata for a single Git repository whose .git directory is excluded from backup
type GitRepoMetadata struct {
	HostPath         string    `json:"host_path"`
	RelativePath     string    `json:"relative_path"`
	PrimaryRemoteURL string    `json:"primary_remote_url,omitempty"`
	Remotes          []string  `json:"remotes,omitempty"`
	Branch           string    `json:"branch,omitempty"`
	CommitHash       string    `json:"commit_hash,omitempty"`
	ShortHash        string    `json:"short_hash,omitempty"`
	Author           string    `json:"author,omitempty"`
	CommitDate       string    `json:"commit_date,omitempty"`
	Subject          string    `json:"subject,omitempty"`
	Tags             []string  `json:"tags,omitempty"`
	ModifiedFiles    []string  `json:"modified_files,omitempty"`
	StashCount       int       `json:"stash_count"`
	GeneratedAt      time.Time `json:"generated_at"`
}

// GitManifest represents the top-level JSON manifest of all Git repositories inside a backup target
type GitManifest struct {
	TargetName      string            `json:"target_name"`
	HostRootDir     string            `json:"host_root_dir"`
	GeneratedAt     time.Time         `json:"generated_at"`
	RepositoryCount int               `json:"repository_count"`
	Repositories    []GitRepoMetadata `json:"repositories"`
}

// ScanGitRepositories walks rootDir to find all .git directories (or .git worktree files),
// skipping any directories that match the provided excludes list (other than .git itself).
func ScanGitRepositories(rootDir string, excludes []string) ([]GitRepoMetadata, error) {
	cleanRoot := filepath.Clean(rootDir)
	info, err := os.Stat(cleanRoot)
	if err != nil {
		return nil, fmt.Errorf("failed to stat %s: %w", cleanRoot, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", cleanRoot)
	}

	var repos []GitRepoMetadata
	now := time.Now()

	err = filepath.WalkDir(cleanRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			// Permission denied or vanished file during scan should not abort the backup
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		name := d.Name()

		// Detect .git directory or .git file (git worktrees/submodules)
		if name == ".git" {
			repoDir := filepath.Dir(path)
			relPath, relErr := filepath.Rel(cleanRoot, repoDir)
			if relErr != nil || relPath == "" {
				relPath = "."
			}
			meta := inspectGitRepository(repoDir, relPath, now)
			repos = append(repos, meta)
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if !d.IsDir() {
			return nil
		}

		if path == cleanRoot {
			return nil
		}

		relPath, relErr := filepath.Rel(cleanRoot, path)
		if relErr != nil {
			return nil
		}

		if shouldSkipDirDuringGitScan(relPath, name, excludes) {
			return filepath.SkipDir
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(repos, func(i, j int) bool {
		return repos[i].RelativePath < repos[j].RelativePath
	})

	return repos, nil
}

// shouldSkipDirDuringGitScan checks if a directory matches any configured exclusion pattern
// (ignoring ".git" since we are specifically looking for ".git").
func shouldSkipDirDuringGitScan(relPath, baseName string, excludes []string) bool {
	relSlash := filepath.ToSlash(relPath)
	for _, rawEx := range excludes {
		ex := strings.TrimSpace(rawEx)
		ex = strings.TrimSuffix(ex, "/")
		ex = strings.TrimPrefix(ex, "./")
		if ex == "" || ex == ".git" {
			continue
		}

		// Exact match on directory basename (e.g. "node_modules", "vendor", ".cache")
		if ex == baseName {
			return true
		}

		// Exact or prefix match on relative path (e.g. ".config/google-chrome", ".cargo/registry")
		if relSlash == ex || strings.HasPrefix(relSlash, ex+"/") {
			return true
		}

		// Glob pattern match on basename
		if matched, err := filepath.Match(ex, baseName); err == nil && matched {
			return true
		}
	}
	return false
}

// inspectGitRepository runs git commands with a timeout to extract repository metadata
func inspectGitRepository(repoDir, relPath string, generatedAt time.Time) GitRepoMetadata {
	meta := GitRepoMetadata{
		HostPath:     repoDir,
		RelativePath: filepath.ToSlash(relPath),
		GeneratedAt:  generatedAt,
	}

	// 1. Primary remote URL (origin preferred, fallback to first remote)
	if originURL := runGitCmd(repoDir, "config", "--get", "remote.origin.url"); originURL != "" {
		meta.PrimaryRemoteURL = originURL
	}

	// 2. All remotes (git remote -v)
	if remotesOut := runGitCmd(repoDir, "remote", "-v"); remotesOut != "" {
		lines := strings.Split(remotesOut, "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line != "" {
				meta.Remotes = append(meta.Remotes, line)
				if meta.PrimaryRemoteURL == "" {
					fields := strings.Fields(line)
					if len(fields) >= 2 {
						meta.PrimaryRemoteURL = fields[1]
					}
				}
			}
		}
	}

	// 3. Branch name
	if branch := runGitCmd(repoDir, "rev-parse", "--abbrev-ref", "HEAD"); branch != "" {
		meta.Branch = branch
	}

	// 4. Full and short commit hash
	if hash := runGitCmd(repoDir, "rev-parse", "HEAD"); hash != "" {
		meta.CommitHash = hash
	}
	if shortHash := runGitCmd(repoDir, "rev-parse", "--short", "HEAD"); shortHash != "" {
		meta.ShortHash = shortHash
	}

	// 5. Commit details (author, ISO date, subject)
	if logOut := runGitCmd(repoDir, "log", "-1", "--format=%an <%ae>%n%aI%n%s"); logOut != "" {
		parts := strings.SplitN(logOut, "\n", 3)
		if len(parts) >= 1 {
			meta.Author = strings.TrimSpace(parts[0])
		}
		if len(parts) >= 2 {
			meta.CommitDate = strings.TrimSpace(parts[1])
		}
		if len(parts) >= 3 {
			meta.Subject = strings.TrimSpace(parts[2])
		}
	}

	// 6. Tags pointing at HEAD
	if tagsOut := runGitCmd(repoDir, "tag", "--points-at", "HEAD"); tagsOut != "" {
		for _, t := range strings.Split(tagsOut, "\n") {
			if t = strings.TrimSpace(t); t != "" {
				meta.Tags = append(meta.Tags, t)
			}
		}
	}

	// 7. Working tree status (cap at 100 entries so huge untracked trees don't bloat the file)
	if statusOut := runGitCmd(repoDir, "status", "--porcelain"); statusOut != "" {
		lines := strings.Split(statusOut, "\n")
		for i, line := range lines {
			if strings.TrimSpace(line) == "" {
				continue
			}
			if i >= 100 {
				meta.ModifiedFiles = append(meta.ModifiedFiles, fmt.Sprintf("... (%d more uncommitted/untracked entries)", len(lines)-100))
				break
			}
			meta.ModifiedFiles = append(meta.ModifiedFiles, line)
		}
	}

	// 8. Stash count
	if stashOut := runGitCmd(repoDir, "stash", "list"); stashOut != "" {
		lines := strings.Split(stashOut, "\n")
		count := 0
		for _, l := range lines {
			if strings.TrimSpace(l) != "" {
				count++
			}
		}
		meta.StashCount = count
	}

	return meta
}

func runGitCmd(repoDir string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fullArgs := append([]string{"-C", repoDir}, args...)
	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	// Disable interactive prompts
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}

// FormatGitRefText formats a human-readable .git-backup-ref.txt content for a repository
func FormatGitRefText(meta GitRepoMetadata) string {
	var b strings.Builder

	b.WriteString("# ============================================================================\n")
	b.WriteString("# EDFX Backup - Git Repository Reference Artifact\n")
	b.WriteString("# ============================================================================\n")
	b.WriteString("# Note: The .git directory was excluded from this backup archive to save space.\n")
	b.WriteString("# Use the metadata and restore instructions below to reconnect or clone the repo.\n\n")

	b.WriteString("[Repository]\n")
	fmt.Fprintf(&b, "Host Path:             %s\n", meta.HostPath)
	fmt.Fprintf(&b, "Archive Relative Path: %s\n", meta.RelativePath)
	fmt.Fprintf(&b, "Captured At:           %s\n", meta.GeneratedAt.Format(time.RFC3339))
	if meta.PrimaryRemoteURL != "" {
		fmt.Fprintf(&b, "Primary Remote URL:    %s\n", meta.PrimaryRemoteURL)
	} else {
		b.WriteString("Primary Remote URL:    (none - local repository only)\n")
	}
	b.WriteString("\n")

	b.WriteString("[Remotes]\n")
	if len(meta.Remotes) > 0 {
		for _, r := range meta.Remotes {
			fmt.Fprintf(&b, "%s\n", r)
		}
	} else {
		b.WriteString("(no remotes configured)\n")
	}
	b.WriteString("\n")

	b.WriteString("[HEAD Commit]\n")
	fmt.Fprintf(&b, "Branch:      %s\n", defaultIfEmpty(meta.Branch, "(detached or unknown)"))
	fmt.Fprintf(&b, "Commit SHA:  %s\n", defaultIfEmpty(meta.CommitHash, "(no commits)"))
	fmt.Fprintf(&b, "Short SHA:   %s\n", defaultIfEmpty(meta.ShortHash, "(no commits)"))
	if meta.Author != "" {
		fmt.Fprintf(&b, "Author:      %s\n", meta.Author)
	}
	if meta.CommitDate != "" {
		fmt.Fprintf(&b, "Commit Date: %s\n", meta.CommitDate)
	}
	if meta.Subject != "" {
		fmt.Fprintf(&b, "Subject:     %s\n", meta.Subject)
	}
	if len(meta.Tags) > 0 {
		fmt.Fprintf(&b, "Tags:        %s\n", strings.Join(meta.Tags, ", "))
	}
	b.WriteString("\n")

	b.WriteString("[Working Tree State at Backup Time]\n")
	fmt.Fprintf(&b, "Stash Entries: %d\n", meta.StashCount)
	if len(meta.ModifiedFiles) == 0 {
		b.WriteString("Working Tree:  clean\n")
	} else {
		fmt.Fprintf(&b, "Working Tree:  %d uncommitted/untracked item(s)\n", len(meta.ModifiedFiles))
		for _, line := range meta.ModifiedFiles {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}
	b.WriteString("\n")

	b.WriteString("[Quick Restore / Re-initialization Instructions]\n")
	if meta.PrimaryRemoteURL != "" {
		b.WriteString("# Option 1: Re-attach git history in-place inside this extracted backup directory:\n")
		b.WriteString("git init\n")
		fmt.Fprintf(&b, "git remote add origin %s\n", meta.PrimaryRemoteURL)
		b.WriteString("git fetch --all --tags\n")
		if meta.CommitHash != "" {
			fmt.Fprintf(&b, "git reset --mixed %s\n", meta.CommitHash)
		}
		if meta.Branch != "" && meta.Branch != "HEAD" {
			fmt.Fprintf(&b, "git checkout -B %s\n", meta.Branch)
		}
		b.WriteString("\n# Option 2: Fresh clone at the exact backed-up commit:\n")
		fmt.Fprintf(&b, "git clone %s\n", meta.PrimaryRemoteURL)
		if meta.CommitHash != "" {
			fmt.Fprintf(&b, "git checkout %s\n", meta.CommitHash)
		}
	} else {
		b.WriteString("# Note: No remote URL was configured for this repository at backup time.\n")
		b.WriteString("# All working tree files (excluding .git) are preserved directly in this directory.\n")
		b.WriteString("git init\n")
		b.WriteString("git add -A && git commit -m \"Restored from EDFX backup\"\n")
	}

	return b.String()
}

// FormatGitManifestText formats a summary table/list of all discovered Git repositories in the backup target
func FormatGitManifestText(targetName, rootDir string, repos []GitRepoMetadata) string {
	var b strings.Builder

	b.WriteString("# ============================================================================\n")
	b.WriteString("# EDFX Backup - Git Repositories Manifest\n")
	b.WriteString("# ============================================================================\n")
	fmt.Fprintf(&b, "Target:           %s\n", targetName)
	fmt.Fprintf(&b, "Host Root Path:   %s\n", rootDir)
	fmt.Fprintf(&b, "Total Git Repos:  %d\n", len(repos))
	if len(repos) > 0 {
		fmt.Fprintf(&b, "Generated At:     %s\n", repos[0].GeneratedAt.Format(time.RFC3339))
	} else {
		fmt.Fprintf(&b, "Generated At:     %s\n", time.Now().Format(time.RFC3339))
	}
	b.WriteString("# ============================================================================\n\n")

	for i, r := range repos {
		fmt.Fprintf(&b, "[%d] %s\n", i+1, r.RelativePath)
		fmt.Fprintf(&b, "    Host Path:   %s\n", r.HostPath)
		fmt.Fprintf(&b, "    Remote URL:  %s\n", defaultIfEmpty(r.PrimaryRemoteURL, "(none - local only)"))
		fmt.Fprintf(&b, "    Branch:      %s\n", defaultIfEmpty(r.Branch, "(unknown)"))
		fmt.Fprintf(&b, "    Commit:      %s (%s)\n", defaultIfEmpty(r.ShortHash, "none"), defaultIfEmpty(r.CommitHash, "none"))
		if r.Subject != "" {
			fmt.Fprintf(&b, "    Subject:     %s\n", r.Subject)
		}
		fmt.Fprintf(&b, "    Ref File:    %s\n\n", filepath.ToSlash(filepath.Join(r.RelativePath, GitBackupRefFileName)))
	}

	return b.String()
}

// CreateGitRefsOverlay scans rootDir for Git repositories and creates a temporary overlay directory
// containing per-repo `.git-backup-ref.txt` files and root-level `GIT_REPOS_MANIFEST.txt` / `.json`.
// If no Git repositories are found, overlayDir is empty and repoCount is 0.
// The caller is responsible for removing overlayDir when done (`os.RemoveAll(overlayDir)`).
func CreateGitRefsOverlay(targetName, rootDir string, excludes []string) (overlayDir string, repos []GitRepoMetadata, err error) {
	repos, err = ScanGitRepositories(rootDir, excludes)
	if err != nil {
		return "", nil, err
	}
	if len(repos) == 0 {
		return "", nil, nil
	}

	tmpDir, err := os.MkdirTemp("", "edfx-gitrefs-*")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create git refs overlay temp dir: %w", err)
	}

	// Write per-repository .git-backup-ref.txt at the matching relative directory inside tmpDir
	for _, repo := range repos {
		targetSubDir := tmpDir
		if repo.RelativePath != "" && repo.RelativePath != "." {
			targetSubDir = filepath.Join(tmpDir, filepath.FromSlash(repo.RelativePath))
		}
		if err := os.MkdirAll(targetSubDir, 0o755); err != nil {
			_ = os.RemoveAll(tmpDir)
			return "", nil, fmt.Errorf("failed to create overlay subdir %s: %w", targetSubDir, err)
		}

		refPath := filepath.Join(targetSubDir, GitBackupRefFileName)
		refContent := FormatGitRefText(repo)
		if err := os.WriteFile(refPath, []byte(refContent), 0o644); err != nil {
			_ = os.RemoveAll(tmpDir)
			return "", nil, fmt.Errorf("failed to write %s: %w", refPath, err)
		}
	}

	// Write root-level GIT_REPOS_MANIFEST.txt
	manifestTxtPath := filepath.Join(tmpDir, GitManifestTextFileName)
	manifestTxt := FormatGitManifestText(targetName, rootDir, repos)
	if err := os.WriteFile(manifestTxtPath, []byte(manifestTxt), 0o644); err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", nil, fmt.Errorf("failed to write %s: %w", manifestTxtPath, err)
	}

	// Write root-level GIT_REPOS_MANIFEST.json
	manifestObj := GitManifest{
		TargetName:      targetName,
		HostRootDir:     rootDir,
		GeneratedAt:     repos[0].GeneratedAt,
		RepositoryCount: len(repos),
		Repositories:    repos,
	}
	if jsonBytes, jsonErr := json.MarshalIndent(manifestObj, "", "  "); jsonErr == nil {
		manifestJSONPath := filepath.Join(tmpDir, GitManifestJSONFileName)
		_ = os.WriteFile(manifestJSONPath, jsonBytes, 0o644)
	}

	return tmpDir, repos, nil
}

func defaultIfEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
