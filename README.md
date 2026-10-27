# EDFX Local S3 Backup Utility (`edfx-backup`)

A local backup CLI utility in Go derived from `/var/www/ciwg-cli`, designed specifically for streaming compressed `.tgz` backups of **`/var/www`** and **`/home/maxx`** to the **`edfx`** S3 bucket on **Hetzner Object Storage**.

## Key Features

- **Hybrid Packaging**:
  - **`/var/www` (`mode: subdirectories`)**: Backs up each project directory in `/var/www/*` (and expands `/var/www/edfx-labs/*` via `nested_groups`) into its own archive under `s3://edfx/var-www/<project>/<project>-YYYYMMDD-HHMMSS.tgz`.
  - **`/home/maxx` (`mode: single`)**: Backs up `/home/maxx` into `s3://edfx/home-maxx/home-maxx-YYYYMMDD-HHMMSS.tgz`.
- **Zero-Disk Streaming to Hetzner S3**: Pipes `tar -czf -` directly into S3 `PutObject` with no local intermediate archive files.
- **Git History Exclusion + Reference Artifact Generation**:
  - Excludes `.git` directories to save gigabytes of packfiles while automatically generating:
    - `<repo-path>/.git-backup-ref.txt`: Placed directly inside each repository's directory in the tarball with remotes, branch, commit SHA, author/date/subject, tags, working tree status, and copy-paste restore/clone instructions.
    - `GIT_REPOS_MANIFEST.txt` & `GIT_REPOS_MANIFEST.json`: Placed at the root of the archive summarizing all Git repositories inside that backup target.
  - Uses a temporary overlay directory merged into `tar` via `-C <overlayDir> .` so your live working tree is never dirtied.
- **Pre/Post Hooks & Optional Docker Database Exports**: Supports `pre_backup_commands`, `post_backup_commands`, and Docker container database dumps (`postgres`, `mysql`, `mariadb`, `mongodb`, `wordpress`).
- **Smart Retention & Pruning**: Supports date-aware daily/weekly/monthly retention (`--smart-retention`), YAML retention policy rulesets (`--retention-policy`), and simple remainder pruning (`--prune --remainder N`).

## Quick Start

```bash
# Build the binary
make build

# Test Hetzner EDFX S3 connectivity (bucket check, write, read, delete)
./dist/edfx-backup conn

# Preview a backup (dry-run) with instant heuristic compression estimate & Git ref discovery
./dist/edfx-backup create --dry-run --estimate-method heuristic --target backup-utility

# Inspect Git reference artifacts that will be generated for any directory
./dist/edfx-backup git-refs /var/www/ciwg-cli --show-ref

# Back up a specific target to s3://edfx
./dist/edfx-backup create --target backup-utility

# Back up only /home/maxx
./dist/edfx-backup create --target home-maxx

# Back up all /var/www projects
./dist/edfx-backup create --group var-www

# Back up everything (/var/www/* + /home/maxx) with smart retention pruning
./dist/edfx-backup create --prune --smart-retention

# List backups in s3://edfx
./dist/edfx-backup list

# Download the latest backup for a project
./dist/edfx-backup read --latest --prefix var-www/ciwg-cli/ --save
```
