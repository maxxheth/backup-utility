package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"edfx-backup/internal/backup"
)

var gitRefsCmd = &cobra.Command{
	Use:   "git-refs [directory]",
	Short: "Scan a directory for Git repositories and preview or export .git-backup-ref.txt artifacts",
	Long: `Scan a target directory (default: current directory) for Git repositories (.git),
skipping configured exclude patterns (like node_modules, vendor, .cache), and output
the Git Reference Artifact (.git-backup-ref.txt) and manifest (GIT_REPOS_MANIFEST.txt)
that edfx-backup embeds into backup archives when .git histories are excluded.

Examples:
  # Preview Git repositories and reference metadata in /var/www/ciwg-cli
  edfx-backup git-refs /var/www/ciwg-cli --show-ref

  # Scan all of /var/www/edfx-labs and view the manifest
  edfx-backup git-refs /var/www/edfx-labs

  # Write .git-backup-ref.txt and GIT_REPOS_MANIFEST.txt to an output directory
  edfx-backup git-refs /var/www/edfx-labs --output-dir /tmp/edfx-git-refs`,
	Args: cobra.MaximumNArgs(1),
	RunE: runGitRefs,
}

func init() {
	gitRefsCmd.Flags().StringSlice("exclude", nil, "Additional directory exclusion patterns")
	gitRefsCmd.Flags().Bool("show-ref", false, "Print full .git-backup-ref.txt contents for each discovered repository")
	gitRefsCmd.Flags().Bool("json", false, "Output discovered repositories as JSON")
	gitRefsCmd.Flags().String("output-dir", "", "Write the generated .git-backup-ref.txt and GIT_REPOS_MANIFEST.txt files to this directory")
}

func runGitRefs(cmd *cobra.Command, args []string) error {
	targetDir := "."
	if len(args) > 0 {
		targetDir = args[0]
	}
	absDir, err := filepath.Abs(targetDir)
	if err != nil {
		return fmt.Errorf("invalid directory %q: %w", targetDir, err)
	}

	defaultCfg := backup.DefaultConfig()
	excludes := backup.MergeUniqueStrings(defaultCfg.Defaults.Excludes, mustGetStringSliceFlag(cmd, "exclude"))

	outputDir := mustGetStringFlag(cmd, "output-dir")
	if outputDir != "" {
		overlayDir, repos, err := backup.CreateGitRefsOverlay(filepath.Base(absDir), absDir, excludes)
		if err != nil {
			return err
		}
		if len(repos) == 0 {
			fmt.Printf("No Git repositories found in %s\n", absDir)
			return nil
		}
		defer os.RemoveAll(overlayDir)

		if err := os.MkdirAll(outputDir, 0o755); err != nil {
			return fmt.Errorf("failed to create output directory %s: %w", outputDir, err)
		}

		// Copy files from overlayDir to outputDir
		err = filepath.WalkDir(overlayDir, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() {
				return walkErr
			}
			rel, relErr := filepath.Rel(overlayDir, path)
			if relErr != nil {
				return relErr
			}
			dst := filepath.Join(outputDir, rel)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			return os.WriteFile(dst, data, 0o644)
		})
		if err != nil {
			return fmt.Errorf("failed to write git reference files to %s: %w", outputDir, err)
		}

		fmt.Printf("✓ Exported %d Git repository reference artifact(s) and manifest to %s\n", len(repos), outputDir)
		return nil
	}

	repos, err := backup.ScanGitRepositories(absDir, excludes)
	if err != nil {
		return err
	}

	if mustGetBoolFlag(cmd, "json") {
		manifest := backup.GitManifest{
			TargetName:      filepath.Base(absDir),
			HostRootDir:     absDir,
			GeneratedAt:     time.Now(),
			RepositoryCount: len(repos),
			Repositories:    repos,
		}
		b, err := json.MarshalIndent(manifest, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}

	if len(repos) == 0 {
		fmt.Printf("No Git repositories found in %s\n", absDir)
		return nil
	}

	fmt.Print(backup.FormatGitManifestText(filepath.Base(absDir), absDir, repos))

	if mustGetBoolFlag(cmd, "show-ref") {
		for _, repo := range repos {
			fmt.Println("----------------------------------------------------------------------------")
			fmt.Print(backup.FormatGitRefText(repo))
		}
	}

	return nil
}
