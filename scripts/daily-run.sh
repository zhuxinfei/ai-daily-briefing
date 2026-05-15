#!/bin/bash
set -e
LOG="/tmp/ai-daily-briefing-$(date +%Y%m%d).log"
exec > >(tee -a "$LOG") 2>&1
echo "=== AI Daily Briefing $(date) ==="

export GOROOT="$HOME/go"
export GOPATH="$HOME/gopath"
export GOMODCACHE="$HOME/gopath/pkg/mod"
export PATH="$GOROOT/bin:$GOPATH/bin:$PATH"
export GOPROXY="https://goproxy.cn,direct"
export GOTOOLCHAIN=auto

export OPENAI_API_KEY="YOUR_OPENAI_API_KEY"
export OPENAI_BASE_URL="https://api.gjs.ink"
export OPENAI_MODEL="gpt-5.4"
export AI_DAILY_SITE_PUSH_TOKEN="YOUR_GITHUB_PAT"
export BRIEFING_REPORT_URL_BASE="https://zhuxinfei.github.io/ai-daily-site/{{YEAR}}/{{YEARMONTH}}/{{DATE}}/"
export HEXTRA_SITE_DIR="$HOME/Downloads/development/ai-daily-site-main"
export PUBLISH_TARGET="test" BRIEFING_MODE="prod" BRIEFING_SKIP_IF_REPORT_EXISTS="1"
export SLACK_TEST_WEBHOOK="https://hooks.slack.com/services/disabled"
export FEISHU_APP_ID="disabled"

BRIEFING_DIR="$HOME/Downloads/development/ai-daily-briefing-main"
SITE_DIR="$HOME/Downloads/development/ai-daily-site-main"

echo "[1] Build..."
cd "$BRIEFING_DIR"
git pull origin main
mkdir -p "$GOPATH" "$GOMODCACHE"
go build -o /tmp/briefing ./cmd/briefing
echo "  $(ls -lh /tmp/briefing | awk '{print $5}')"

echo "[2] Update site repo..."
cd "$SITE_DIR" && git pull origin main

echo "[3] Seed..."
cd "$BRIEFING_DIR"
/tmp/briefing seed

echo "[4] Run pipeline..."
RUN_DATE=$(TZ=Asia/Shanghai date +%F)
echo "  Date: $RUN_DATE"
/tmp/briefing run --date "$RUN_DATE" --domain ai --target test

echo "[5] Push..."
cd "$SITE_DIR"
git add -A
if git diff --cached --quiet; then
  echo "  No changes"
else
  git commit -m "chore: daily briefing $RUN_DATE"
  git push origin main
  echo "  ✅ Pushed"
fi

echo "Done: https://zhuxinfei.github.io/ai-daily-site/"
