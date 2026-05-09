#!/bin/bash
# briefing-post-run.sh
#
# 在 briefing-orchestrator.sh 的 main_loop 成功返回后调用.
#
# 新时序 (2026-04-22 修复, 解决 "推送发出时 Pages 还没好链接 404" 问题):
#   (1) push ai-daily-site (Hugo 内容) → GitHub Pages
#   (2) 轮询 GitHub Pages URL 直到 200 (最多等 5 分钟 fail-soft)
#   (3) briefing promote: 用 snapshot 重放到 Slack prod + 飞书 (真正的用户推送)
#   (4) push briefing-v3 state (dedup / daily archive / slack payload) → GitHub
#
# 为什么要这么拆:
#   - orchestrator 里 briefing run 现在带 --dry-run: 只生成内容和 payload
#     snapshot, 不发 Slack/飞书.
#   - 真实推送推迟到此处, 前置条件是线上 Pages 已经有新内容, 保证用户
#     点链接就能看到.
#   - briefing promote 命令会读取 data/slack-payload-<today>.json 和
#     data/feishu-daily-card-<today>.json 重放, bytes 一致.
#
# 为什么要 push briefing-v3 state:
#   - claude-4 宿主若丢失 (VPS 到期 / 被封 / 磁盘坏), 本地只剩 GitHub 上的数据
#   - dedup 文件 (data/sent_urls.txt, data/sent_titles.txt) 是单点关键状态,
#     丢了会造成老内容被重推, 必须有线上副本
#   - 灾备恢复时 git clone 就能拉到最近一天的状态, 不依赖 Syncthing
#
# fail-soft: 任何 git / Pages poll 失败都只 log 不 exit 非零, 但 promote
# 失败会记录 WARN 并 exit 非零, 让 orchestrator 察觉 (因为此时 Slack 没推
# 出去, 是真故障). 如果 promote 已经是重试后的结果且仍失败, 就交人工介入.

set -uo pipefail

BRIEFING_ROOT=${BRIEFING_ROOT:-/root/briefing-v3}
BRIEFING_DB=${BRIEFING_DB:-$BRIEFING_ROOT/data/briefing.db}
BRIEFING_BIN=${BRIEFING_BIN:-$BRIEFING_ROOT/bin/briefing}
SITE_DIR=${SITE_DIR:-/root/ai-daily-site}

# 测试钩子:
#   GIT_PUSH_CMD        — test 会覆盖成 echo 避免真 push
#   BRIEFING_PROMOTE_CMD — test 会覆盖成 echo 避免真 promote
#   PAGES_READY_TIMEOUT  — 测试用短超时 (单位秒, 默认 300)
#   PAGES_READY_INTERVAL — 测试用短探测间隔 (单位秒, 默认 5)
GIT_PUSH_CMD=${GIT_PUSH_CMD:-"git push origin main"}
BRIEFING_PROMOTE_CMD=${BRIEFING_PROMOTE_CMD:-"$BRIEFING_BIN promote"}
PAGES_READY_TIMEOUT=${PAGES_READY_TIMEOUT:-300}
PAGES_READY_INTERVAL=${PAGES_READY_INTERVAL:-5}

TODAY=$(TZ=Asia/Shanghai date '+%Y-%m-%d')
YYYY=${TODAY:0:4}
YYYY_MM=${TODAY:0:7}
PAGES_URL="https://ylzsdafei.github.io/ai-daily-site/${YYYY}/${YYYY_MM}/${TODAY}/"

# ---------------------------------------------------------------------
# (1) ai-daily-site: push Hugo 内容到 GitHub (触发 Pages 部署)
# ---------------------------------------------------------------------
push_site() {
    if [[ ! -d "$SITE_DIR" ]]; then
        echo "[post-run] site dir $SITE_DIR not found, skipping site push"
        return 1
    fi
    cd "$SITE_DIR" || return 1
    git add content/ static/images/ 2>/dev/null || true
    if git diff --cached --quiet; then
        echo "[post-run] ai-daily-site: nothing staged, already up-to-date"
        return 0
    fi
    git commit -m "chore(content): 自动同步 $TODAY 日报" >/dev/null 2>&1 || true
    if $GIT_PUSH_CMD 2>&1; then
        echo "[post-run] ai-daily-site pushed OK ($TODAY)"
        return 0
    else
        echo "[post-run] WARN: ai-daily-site push failed"
        return 1
    fi
}

# ---------------------------------------------------------------------
# (2) 轮询 GitHub Pages 直到今日 URL 可访问
# ---------------------------------------------------------------------
# 返回 0 = ready, 1 = timeout (fail-soft 模式下调用方仍会继续 promote).
wait_for_pages() {
    local url="$1"
    local timeout="$2"
    local interval="$3"
    local elapsed=0
    local code=""

    if (( timeout <= 0 )); then
        echo "[post-run] PAGES_READY_TIMEOUT=0, skipping readiness poll"
        return 0
    fi

    echo "[post-run] waiting for Pages: $url (timeout=${timeout}s, interval=${interval}s)"
    while (( elapsed < timeout )); do
        code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 8 "$url" 2>/dev/null || echo "000")
        if [[ "$code" == "200" ]]; then
            echo "[post-run] Pages ready after ${elapsed}s (HTTP 200)"
            return 0
        fi
        sleep "$interval"
        elapsed=$(( elapsed + interval ))
    done
    echo "[post-run] WARN: Pages not ready after ${timeout}s (last HTTP=$code); proceeding with promote anyway"
    return 1
}

# ---------------------------------------------------------------------
# (3) briefing promote: 重放 snapshot 到 Slack prod + 飞书
# ---------------------------------------------------------------------
# promote 命令内部已包含飞书推送 (main.go:488-497), 无需再单独调 promote-feishu.
# 失败会 return 非 0, 上层记录 WARN 但仍尝试推 briefing-v3 state.
promote_push() {
    cd "$BRIEFING_ROOT" || return 1
    local snapshot="data/slack-payload-${TODAY}.json"
    if [[ ! -f "$snapshot" ]]; then
        echo "[post-run] ERROR: slack snapshot $snapshot not found, cannot promote"
        return 1
    fi
    echo "[post-run] promoting $TODAY to Slack prod + Feishu..."
    if $BRIEFING_PROMOTE_CMD --date "$TODAY" 2>&1; then
        echo "[post-run] promote OK ($TODAY)"
        return 0
    else
        echo "[post-run] ERROR: promote failed ($TODAY)"
        return 1
    fi
}

# ---------------------------------------------------------------------
# (4) briefing-v3: state + archive push
# ---------------------------------------------------------------------
# 只 stage 白名单文件, 避免把大/临时文件 (backups/*.db, export/, slack-payload
# JSON 历史堆积) 误提交. data/backups 已在 .gitignore 排除.
push_briefing_state() {
    cd "$BRIEFING_ROOT" || return 1

    local paths=()
    [[ -f "daily/$TODAY.md" ]] && paths+=("daily/$TODAY.md")
    [[ -f "data/sent_urls.txt" ]] && paths+=("data/sent_urls.txt")
    [[ -f "data/sent_titles.txt" ]] && paths+=("data/sent_titles.txt")
    [[ -f "data/slack-payload-$TODAY.json" ]] && paths+=("data/slack-payload-$TODAY.json")

    if [[ ${#paths[@]} -eq 0 ]]; then
        echo "[post-run] no whitelisted state files to commit"
        return 0
    fi

    git add -- "${paths[@]}" 2>/dev/null || true
    if git diff --cached --quiet; then
        echo "[post-run] briefing-v3: no state changes, skipping push"
        return 0
    fi

    if git commit -m "chore(state): auto-commit $TODAY daily state

Automated post-run commit by briefing-post-run.sh.
Includes today's archive + dedup markers so GitHub has
a disaster-recovery copy of pipeline state.
" >/dev/null 2>&1; then
        if $GIT_PUSH_CMD 2>&1; then
            echo "[post-run] briefing-v3 state pushed OK ($TODAY)"
            return 0
        else
            echo "[post-run] WARN: briefing-v3 state push failed (non-fatal)"
            return 1
        fi
    else
        echo "[post-run] briefing-v3: commit no-op (nothing staged)"
        return 0
    fi
}

# ---------------------------------------------------------------------
# 执行
# ---------------------------------------------------------------------
OVERALL_RC=0

push_site || OVERALL_RC=$?

# 即使 push_site 失败 (可能 nothing changed), 也要尝试 wait + promote, 因为
# Pages 可能已经是更早 push 上去的.
wait_for_pages "$PAGES_URL" "$PAGES_READY_TIMEOUT" "$PAGES_READY_INTERVAL" || true

if ! promote_push; then
    echo "[post-run] WARN: promote step failed — Slack/Feishu may not have received today's briefing"
    OVERALL_RC=1
fi

push_briefing_state || true

exit "$OVERALL_RC"
