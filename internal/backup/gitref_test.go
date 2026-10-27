package backup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitRefsOverlayAndTarExclusion(t *testing.T) {
	tmpDir := t.TempDir()
	projectDir := filepath.Join(tmpDir, "sample-project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Initialize a real git repository in sample-project
	runCmd := func(dir string, name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("command %s %v failed: %v (output: %s)", name, args, err, string(out))
		}
	}

	runCmd(projectDir, "git", "init", "-b", "main")
	runCmd(projectDir, "git", "config", "user.name", "Maxx Test")
	runCmd(projectDir, "git", "config", "user.email", "maxx@edfx.test")
	runCmd(projectDir, "git", "remote", "add", "origin", "git@github.com:edfx/sample-project.git")

	// Write a tracked file and commit
	appFile := filepath.Join(projectDir, "main.go")
	if err := os.WriteFile(appFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(projectDir, "git", "add", "main.go")
	runCmd(projectDir, "git", "commit", "-m", "Initial commit for sample-project")

	// Create an uncommitted file so working tree state is also tested
	dirtyFile := filepath.Join(projectDir, "uncommitted.txt")
	if err := os.WriteFile(dirtyFile, []byte("dirty state\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create an excluded directory (node_modules) with a fake .git inside it to verify it is skipped
	excludedGit := filepath.Join(projectDir, "node_modules", "some-pkg", ".git")
	if err := os.MkdirAll(excludedGit, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "node_modules", "some-pkg", "index.js"), []byte("module.exports = {};"), 0o644); err != nil {
		t.Fatal(err)
	}

	excludes := []string{".git", "node_modules", "*.tgz"}

	overlayDir, repos, err := CreateGitRefsOverlay("sample-project", projectDir, excludes)
	if err != nil {
		t.Fatalf("CreateGitRefsOverlay failed: %v", err)
	}
	defer os.RemoveAll(overlayDir)

	if len(repos) != 1 {
		t.Fatalf("expected 1 git repository (skipping node_modules), got %d", len(repos))
	}

	repo := repos[0]
	if repo.PrimaryRemoteURL != "git@github.com:edfx/sample-project.git" {
		t.Errorf("unexpected remote URL: %q", repo.PrimaryRemoteURL)
	}
	if repo.Branch != "main" {
		t.Errorf("unexpected branch: %q", repo.Branch)
	}
	if repo.CommitHash == "" || repo.ShortHash == "" {
		t.Errorf("expected non-empty commit hash")
	}
	if repo.Subject != "Initial commit for sample-project" {
		t.Errorf("unexpected commit subject: %q", repo.Subject)
	}

	// Now build and run the tar stream command and extract it into an output directory
	tarCmdStr := BuildTarStreamCommand(projectDir, overlayDir, excludes)
	extractDir := filepath.Join(tmpDir, "extracted")
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		t.Fatal(err)
	}

	pipeline := tarCmdStr + " | tar -xzf - -C " + shellEscape(extractDir)
	cmd := exec.Command("bash", "-lc", pipeline)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tar pipeline failed: %v (output: %s)", err, string(out))
	}

	// 1. Verify .git directory was excluded from extracted archive
	if _, err := os.Stat(filepath.Join(extractDir, ".git")); !os.IsNotExist(err) {
		t.Errorf("expected .git directory to be excluded from archive, but it exists")
	}

	// 2. Verify node_modules was excluded from extracted archive
	if _, err := os.Stat(filepath.Join(extractDir, "node_modules")); !os.IsNotExist(err) {
		t.Errorf("expected node_modules directory to be excluded from archive, but it exists")
	}

	// 3. Verify .git-backup-ref.txt was included and contains remote & commit info
	refBytes, err := os.ReadFile(filepath.Join(extractDir, GitBackupRefFileName))
	if err != nil {
		t.Fatalf("expected %s in extracted archive: %v", GitBackupRefFileName, err)
	}
	refStr := string(refBytes)
	if !strings.Contains(refStr, "git@github.com:edfx/sample-project.git") {
		t.Errorf("expected remote URL in %s, got:\n%s", GitBackupRefFileName, refStr)
	}
	if !strings.Contains(refStr, repo.CommitHash) {
		t.Errorf("expected commit hash %s in %s", repo.CommitHash, GitBackupRefFileName)
	}
	if !strings.Contains(refStr, "uncommitted.txt") {
		t.Errorf("expected uncommitted.txt in working tree status of %s", GitBackupRefFileName)
	}

	// 4. Verify GIT_REPOS_MANIFEST.txt and GIT_REPOS_MANIFEST.json exist
	if _, err := os.Stat(filepath.Join(extractDir, GitManifestTextFileName)); err != nil {
		t.Errorf("expected %s in extracted archive: %v", GitManifestTextFileName, err)
	}
	if _, err := os.Stat(filepath.Join(extractDir, GitManifestJSONFileName)); err != nil {
		t.Errorf("expected %s in extracted archive: %v", GitManifestJSONFileName, err)
	}
}
