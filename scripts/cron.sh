#!/bin/bash
# briefing-v3 cron entry point
# Invoked by systemd cron on the production schedule.
set -euo pipefail

# Work from the briefing-v3 repo root.
cd "$(dirname "$0")/.."

export TZ=Asia/Shanghai
export PATH="$PATH:/usr/local/go/bin"

# Load secrets if present
if [ -f config/secrets.env ]; then
    set -a
    # shellcheck disable=SC1091
    source config/secrets.env
    set +a
fi

mkdir -p logs data/images

LOG_FILE="logs/cron-$(date +%Y-%m-%d).log"
DATE="$(date +%Y-%m-%d)"

echo "[$(date +'%Y-%m-%d %H:%M:%S')] briefing-v3 run start $DATE" | tee -a "$LOG_FILE"

set +e
./bin/briefing run --date "$DATE" --domain ai --target auto 2>&1 | tee -a "$LOG_FILE"
RC=${PIPESTATUS[0]}
set -e

echo "[$(date +'%Y-%m-%d %H:%M:%S')] briefing-v3 run end rc=$RC" | tee -a "$LOG_FILE"

if [ "$RC" -ne 0 ] && [ -n "${SLACK_TEST_WEBHOOK:-}" ]; then
    curl -sS --max-time 10 -X POST "$SLACK_TEST_WEBHOOK" \
        -H "Content-Type: application/json" \
        -d "{\"text\":\"briefing-v3 ${DATE} 失败 (exit ${RC}), 请人工介入\"}" || true
fi

exit "$RC"
