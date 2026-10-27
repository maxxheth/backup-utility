#!/usr/bin/env bash
# ============================================================================
# EDFX Scheduled Backup Runner (Cron Wrapper)
# Runs 4x daily (every 6 hours) to back up /var/www/*, /var/www/edfx-labs/*,
# and /home/maxx to the Hetzner EDFX S3 bucket (s3://edfx).
# ============================================================================

set -euo pipefail

export PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/home/maxx/.local/bin"

PROJECT_DIR="/var/www/edfx-labs/backup-utility"
BINARY="${PROJECT_DIR}/dist/edfx-backup"
CONFIG_FILE="${PROJECT_DIR}/configs/backup-config.yml"
ENV_FILE="${PROJECT_DIR}/.env"
LOG_DIR="${PROJECT_DIR}/logs"
LOG_FILE="${LOG_DIR}/cron-backup.log"
LOCK_FILE="/tmp/edfx-backup-cron.lock"

mkdir -p "${LOG_DIR}"

# Rotate log file if it exceeds 25 MB
if [[ -f "${LOG_FILE}" ]]; then
  LOG_SIZE=$(stat -c%s "${LOG_FILE}" 2>/dev/null || echo 0)
  if (( LOG_SIZE > 26214400 )); then
    mv -f "${LOG_FILE}" "${LOG_FILE}.1"
  fi
fi

# Acquire exclusive non-blocking lock so overlapping runs never collide
exec 9>"${LOCK_FILE}"
if ! flock -n 9; then
  echo "[$(date -Is)] Skipping backup run: another edfx-backup process is currently holding ${LOCK_FILE}" >> "${LOG_FILE}"
  exit 0
fi

cd "${PROJECT_DIR}"

echo "============================================================================" >> "${LOG_FILE}"
echo "[$(date -Is)] Starting scheduled EDFX backup (/var/www + /home/maxx -> s3://edfx)" >> "${LOG_FILE}"
echo "============================================================================" >> "${LOG_FILE}"

"${BINARY}" create \
  --env "${ENV_FILE}" \
  --config-file "${CONFIG_FILE}" \
  --prune \
  --smart-retention \
  --keep-daily "${BACKUP_KEEP_DAILY:-28}" \
  --keep-weekly "${BACKUP_KEEP_WEEKLY:-26}" \
  --keep-monthly "${BACKUP_KEEP_MONTHLY:-6}" \
  --respect-capacity-limit \
  --capacity-threshold 95.0 >> "${LOG_FILE}" 2>&1

EXIT_CODE=$?
echo "[$(date -Is)] Finished scheduled EDFX backup (exit code: ${EXIT_CODE})" >> "${LOG_FILE}"
exit "${EXIT_CODE}"
