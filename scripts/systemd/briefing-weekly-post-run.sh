#!/bin/bash
# briefing-weekly-post-run.sh
#
# 周报 systemd ExecStartPost 脚本. 与 briefing-post-run.sh 对齐, 时序:
#   (1) push ai-daily-site → GitHub Pages
#   (2) 轮询 weekly URL 直到 200 (最多等 5 分钟 fail-soft)
#   (3) briefing promote-weekly: 用 snapshot 重放 Slack prod
#   (4) briefing promote-weekly-feishu: 用 snapshot 重放飞书
#   (5) push briefing-v3 state 兜底 (灾备)
#
# 前置条件: briefing-weekly.service ExecStart 用 BRIEFING_WEEKLY_NO_PUBLISH=1
# 跑 weekly 命令, 让它只生成 + 写 Hugo + 存 snapshot, 不推 Slack/飞书.
# 真正的 prod 推送由本脚本在 Pages 部署完成后用 promote-weekly /
# promote-weekly-feishu 重放完成. 避免"推送发出时 Pages 还没好链接 404".
#
# fail-soft: 任何 git / Pages poll 失败都只 log; promote 失败 exit 非零
# 让 systemd journal 留痕, 便于人工介入.
#
# 测试钩子 (跟 daily post-run 一致):
#   GIT_PUSH_CMD          — 测试覆盖成 echo 避免真 push
#   BRIEFING_PROMOTE_CMD  — 测试覆盖成 echo 避免真推
#   PAGES_READY_TIMEOUT   — 测试用短超时 (默认 300s)
#   PAGES_READY_INTERVAL  — 测试用短间隔 (默认 5s)

set -uo pipefail

BRIEFING_ROOT=${BRIEFING_ROOT:-/root/briefing-v3}
BRIEFING_BIN=${BRIEFING_BIN:-$BRIEFING_ROOT/bin/briefing}
SITE_DIR=${SITE_DIR:-/root/ai-daily-site}
GIT_PUSH_CMD=${GIT_PUSH_CMD:-"git push origin main"}
BRIEFING_PROMOTE_CMD=${BRIEFING_PROMOTE_CMD:-"$BRIEFING_BIN promote-weekly"}
BRIEFING_PROMOTE_FEISHU_CMD=${BRIEFING_PROMOTE_FEISHU_CMD:-"$BRIEFING_BIN promote-weekly-feishu"}
PAGES_READY_TIMEOUT=${PAGES_READY_TIMEOUT:-300}
PAGES_READY_INTERVAL=${PAGES_READY_INTERVAL:-5}

# 周报时间锚点: systemd 在周日 14:00 UTC = 周日 22:00 北京时间触发.
# date 当下就是周日, ISO week 对应当前周 (Mon-Sun 整周).
TODAY=$(TZ=Asia/Shanghai date '+%Y-%m-%d')
ISO_YEAR_WEEK=$(TZ=Asia/Shanghai date '+%G-W%V')   # e.g. 2026-W17
WEEK_LOWER=$(echo "$ISO_YEAR_WEEK" | tr '[:upper:]' '[:lower:]')   # 2026-w17
WEEKLY_URL="https://ylzsdafei.github.io/ai-daily-site/blog/weekly/${WEEK_LOWER}/"

# ---------------------------------------------------------------------
# (1) push ai-daily-site
# ---------------------------------------------------------------------
push_site() {
    if [[ ! -d "$SITE_DIR" ]]; then
        echo "[weekly post-run] $SITE_DIR not found, skipping site push"
        return 1
    fi
    cd "$SITE_DIR" || return 1
    git add content/ static/images/ 2>/dev/null || true
    if git diff --cached --quiet; then
        echo "[weekly post-run] ai-daily-site: nothing staged, already up-to-date"
        return 0
    fi
    git commit -m "chore(content): 自动同步 $ISO_YEAR_WEEK 周报" >/dev/null 2>&1 || true
    if $GIT_PUSH_CMD 2>&1; then
        echo "[weekly post-run] ai-daily-site pushed OK ($ISO_YEAR_WEEK)"
        return 0
    else
        echo "[weekly post-run] WARN: ai-daily-site push failed"
        return 1
    fi
}

# ---------------------------------------------------------------------
# (2) wait_for_pages: poll until URL returns 200
# ---------------------------------------------------------------------
wait_for_pages() {
    local url="$1" timeout="$2" interval="$3"
    local elapsed=0 code=""
    if (( timeout <= 0 )); then
        echo "[weekly post-run] PAGES_READY_TIMEOUT=0, skipping readiness poll"
        return 0
    fi
    echo "[weekly post-run] waiting for Pages: $url (timeout=${timeout}s, interval=${interval}s)"
    while (( elapsed < timeout )); do
        code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 8 "$url" 2>/dev/null || echo "000")
        if [[ "$code" == "200" ]]; then
            echo "[weekly post-run] Pages ready after ${elapsed}s (HTTP 200)"
            return 0
        fi
        sleep "$interval"
        elapsed=$(( elapsed + interval ))
    done
    echo "[weekly post-run] WARN: Pages not ready after ${timeout}s (last HTTP=$code); proceeding anyway"
    return 1
}

# ---------------------------------------------------------------------
# (3) promote-weekly: replay Slack snapshot
# ---------------------------------------------------------------------
promote_slack() {
    cd "$BRIEFING_ROOT" || return 1
    local snapshot="data/slack-weekly-payload-${ISO_YEAR_WEEK}.json"
    if [[ ! -f "$snapshot" ]]; then
        echo "[weekly post-run] ERROR: weekly slack snapshot $snapshot not found, cannot promote"
        return 1
    fi
    echo "[weekly post-run] promoting $ISO_YEAR_WEEK to Slack prod..."
    if $BRIEFING_PROMOTE_CMD --date "$TODAY" 2>&1; then
        echo "[weekly post-run] promote-weekly OK ($ISO_YEAR_WEEK)"
        return 0
    else
        echo "[weekly post-run] ERROR: promote-weekly failed ($ISO_YEAR_WEEK)"
        return 1
    fi
}

# ---------------------------------------------------------------------
# (4) promote-weekly-feishu: replay Feishu snapshot
# ---------------------------------------------------------------------
promote_feishu() {
    cd "$BRIEFING_ROOT" || return 1
    local snapshot="data/feishu-weekly-card-${ISO_YEAR_WEEK}.json"
    if [[ ! -f "$snapshot" ]]; then
        echo "[weekly post-run] WARN: feishu snapshot $snapshot not found, skipping feishu push"
        return 0
    fi
    echo "[weekly post-run] promoting $ISO_YEAR_WEEK to Feishu..."
    if $BRIEFING_PROMOTE_FEISHU_CMD --date "$TODAY" 2>&1; then
        echo "[weekly post-run] promote-weekly-feishu OK ($ISO_YEAR_WEEK)"
        return 0
    else
        echo "[weekly post-run] WARN: promote-weekly-feishu failed (non-fatal)"
        return 1
    fi
}

# ---------------------------------------------------------------------
# (5) push briefing-v3 state for disaster recovery
# ---------------------------------------------------------------------
push_briefing_state() {
    cd "$BRIEFING_ROOT" || return 1
    local paths=()
    [[ -f "data/slack-weekly-payload-${ISO_YEAR_WEEK}.json" ]] && \
        paths+=("data/slack-weekly-payload-${ISO_YEAR_WEEK}.json")
    if [[ ${#paths[@]} -eq 0 ]]; then
        echo "[weekly post-run] no whitelisted state files to commit"
        return 0
    fi
    git add -- "${paths[@]}" 2>/dev/null || true
    if git diff --cached --quiet; then
        echo "[weekly post-run] briefing-v3: no state changes, skipping push"
        return 0
    fi
    if git commit -m "chore(state): auto-commit $ISO_YEAR_WEEK weekly snapshot

Automated post-run commit by briefing-weekly-post-run.sh.
" >/dev/null 2>&1; then
        if $GIT_PUSH_CMD 2>&1; then
            echo "[weekly post-run] briefing-v3 state pushed OK ($ISO_YEAR_WEEK)"
            return 0
        else
            echo "[weekly post-run] WARN: briefing-v3 state push failed (non-fatal)"
            return 1
        fi
    fi
    return 0
}

# ---------------------------------------------------------------------
# 执行
# ---------------------------------------------------------------------
OVERALL_RC=0

push_site || OVERALL_RC=$?
wait_for_pages "$WEEKLY_URL" "$PAGES_READY_TIMEOUT" "$PAGES_READY_INTERVAL" || true

if ! promote_slack; then
    echo "[weekly post-run] WARN: Slack promote failed — Slack prod 未收到周报"
    OVERALL_RC=1
fi

promote_feishu || true   # fail-soft: 飞书失败不阻

push_briefing_state || true

exit "$OVERALL_RC"
