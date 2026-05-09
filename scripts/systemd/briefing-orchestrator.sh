#!/bin/bash
# briefing-orchestrator.sh
#
# v1.0.1 critical fix (2026-04-14).
# 取代 briefing-retry-{1,2,3}.timer 的 systemd OnFailure 链 —— 那条链
# 因 Bug B (gate `_ = failedSections`) 让 pipeline 永远 exit 0 从未触发过。
#
# 本脚本被 briefing-daily.service 的 ExecStart 调用, 以固定时刻 schedule 的
# 方式驱动整条 briefing run. D4 重试公式 (06:00/06:25/06:45/07:05 四次)
# 内置在本脚本中, 失败后的告警也由本脚本完成 —— systemd 只负责"按时拉起".
#
# 用户部署检查清单 (等 approve 后手工执行, 本脚本自身不做):
#   1. 确认 /tmp/briefing-daily.service.new 内容正确, 然后:
#      cp /tmp/briefing-daily.service.new /etc/systemd/system/briefing-daily.service
#   2. 删除废弃单元:
#      rm /etc/systemd/system/briefing-retry-{1,2,3}.{service,timer}
#      rm /etc/systemd/system/briefing-alert.service  # 告警改走本脚本
#   3. sudo systemctl daemon-reload
#   4. sudo systemctl reset-failed
#   5. sudo systemctl restart briefing-daily.timer
#
# Exit codes (传递 pipeline 的 exit code, 参见 plan D1):
#   0 success
#   2 transient (LLM 5xx / timeout) —— 本脚本会重试
#   3 content (gate hard fail) —— 本脚本会重试, 最终失败 exit 6
#   4 config/auth —— 立即 abort + 告警
#   5 infra (DB lock/disk full) —— 立即 abort + 告警
#   6 needs_human (4 次重试后仍缺 section, 已发告警) —— systemd 不要 restart

set -uo pipefail

# ======================================================================
# 0. 常量和 DRY_RUN mode
# ======================================================================

: "${BRIEFING_ROOT:=/root/briefing-v3}"
: "${BRIEFING_DB:=$BRIEFING_ROOT/data/briefing.db}"
: "${BRIEFING_BIN:=$BRIEFING_ROOT/bin/briefing}"
: "${BRIEFING_SECRETS:=$BRIEFING_ROOT/config/secrets.env}"
: "${BRIEFING_LOCK:=/var/lock/briefing-orchestrator.lock}"
: "${BRIEFING_JSONL:=/var/log/briefing-orchestrator.jsonl}"
: "${BRIEFING_DRY_RUN:=0}"
# BRIEFING_NOW_OVERRIDE: 测试时 mock 当前时间, 格式 "HH:MM" (Asia/Shanghai).
# 生产不设. 测试脚本设成 "08:30" 等.
: "${BRIEFING_NOW_OVERRIDE:=}"
# BRIEFING_DEADLINE_MINUTES: 总 deadline, 默认 195 (06:00->09:15). 测试时可调小.
: "${BRIEFING_DEADLINE_MINUTES:=195}"
# BRIEFING_PREFLIGHT_ONLY: 只跑 pre-flight 然后 exit 0, 不进入 attempt 循环.
: "${BRIEFING_PREFLIGHT_ONLY:=0}"

TAG="[orchestrator]"

# ======================================================================
# 1. JSON log helper
# ======================================================================

# log_jsonl <attempt> <action> <exit_code> <duration_s> <error_msg>
log_jsonl() {
    local attempt="$1" action="$2" exit_code="$3" duration_s="$4" error="${5:-}"
    local ts
    ts=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
    # 用 jq 保证 error 里的特殊字符不破坏 JSON
    if command -v jq >/dev/null 2>&1; then
        jq -nc \
            --arg ts "$ts" \
            --argjson attempt "$attempt" \
            --arg action "$action" \
            --argjson exit_code "$exit_code" \
            --argjson duration_s "$duration_s" \
            --arg error "$error" \
            '{timestamp:$ts, attempt:$attempt, action:$action, exit_code:$exit_code, duration_s:$duration_s, error:$error}' \
            >> "$BRIEFING_JSONL" 2>/dev/null
    else
        # fallback: 粗糙字符串拼接 (理论上 jq 总是装的, 这里只是兜底)
        printf '{"timestamp":"%s","attempt":%s,"action":"%s","exit_code":%s,"duration_s":%s,"error":"%s"}\n' \
            "$ts" "$attempt" "$action" "$exit_code" "$duration_s" "$error" \
            >> "$BRIEFING_JSONL" 2>/dev/null
    fi
    # 同时打印到 stdout (systemd journal 会收)
    echo "$TAG [$action] attempt=$attempt exit=$exit_code dur=${duration_s}s ${error:+error=$error}"
}

# ======================================================================
# 2. SIGTERM trap
# ======================================================================

CURRENT_ATTEMPT=0
CURRENT_ACTION="starting"
STARTED_AT=$(date +%s)

on_sigterm() {
    local now
    now=$(date +%s)
    local dur=$((now - STARTED_AT))
    log_jsonl "$CURRENT_ATTEMPT" "terminated_by_signal" 143 "$dur" "received SIGTERM, orchestrator aborting during $CURRENT_ACTION"
    exit 143
}
trap on_sigterm TERM INT

# ======================================================================
# 3. flock 互斥
# ======================================================================

# 打开 fd 200 对 lock 文件, 用 flock 拿锁; 拿不到等 10 秒, 仍失败才 exit 0
# (说明已经有实例在跑, 比如今天已经在 06:00 拉起, 09:00 的手动再跑就让它静默退出).
mkdir -p "$(dirname "$BRIEFING_LOCK")"
exec 200>"$BRIEFING_LOCK"
if ! flock -w 10 200; then
    echo "$TAG another instance is running (lock held after 10s wait), exiting"
    log_jsonl 0 "lock_held" 0 0 "another instance holds $BRIEFING_LOCK after 10s wait"
    exit 0
fi

echo "$TAG started pid=$$ dry_run=$BRIEFING_DRY_RUN"
log_jsonl 0 "started" 0 0 "pid=$$ dry_run=$BRIEFING_DRY_RUN now_override=${BRIEFING_NOW_OVERRIDE:-none}"

# ======================================================================
# 4. 时间 helper: 以 Asia/Shanghai HH:MM 形式读当前或 override 时间
# ======================================================================

# now_minutes: 返回从 00:00 开始的累积分钟数 (Asia/Shanghai).
now_minutes() {
    local hhmm
    if [[ -n "$BRIEFING_NOW_OVERRIDE" ]]; then
        hhmm="$BRIEFING_NOW_OVERRIDE"
    else
        hhmm=$(TZ=Asia/Shanghai date '+%H:%M')
    fi
    local h="${hhmm%%:*}"
    local m="${hhmm##*:}"
    # strip leading zeros (08 -> 8) 防 bash 把 08 解析成八进制
    h=$((10#$h))
    m=$((10#$m))
    echo $((h * 60 + m))
}

# ======================================================================
# 5. Pre-flight 检查
#    返回值: 0=通过, 2=LLM 暂时不可达(可重试), 4=config/infra 错误(不可重试)
# ======================================================================

# LAST_PREFLIGHT_ERROR: 记录最近一次 preflight 失败原因, 供告警引用
LAST_PREFLIGHT_ERROR=""

preflight_abort() {
    local reason="$1"
    echo "$TAG PRE-FLIGHT FAIL: $reason" >&2
    log_jsonl "$CURRENT_ATTEMPT" "preflight_fail" 4 0 "$reason"
    LAST_PREFLIGHT_ERROR="$reason"
    # 不再 exit, 由调用方根据 preflight() 返回值决定行为
}

preflight() {
    echo "$TAG running pre-flight checks..."
    local t0
    t0=$(date +%s)

    # ---- 5.1 secrets.env 存在 + 含必需 key (不可重试) ----
    if [[ ! -f "$BRIEFING_SECRETS" ]]; then
        preflight_abort "secrets.env missing: $BRIEFING_SECRETS"
        return 4
    fi

    # 读 secrets.env 到当前 shell (设置 set -a 让 export 自动)
    set -a
    # shellcheck disable=SC1090
    source "$BRIEFING_SECRETS"
    set +a

    for key in BRIEFING_MODE SLACK_TEST_WEBHOOK OPENAI_BASE_URL OPENAI_API_KEY; do
        if [[ -z "${!key:-}" ]]; then
            preflight_abort "$key missing from secrets.env"
            return 4
        fi
    done
    echo "$TAG secrets.env OK (mode=$BRIEFING_MODE)"

    # ---- 5.2 git SSH key 有效 (不可重试) ----
    if ! git -C "$BRIEFING_ROOT" status >/dev/null 2>&1; then
        preflight_abort "git -C $BRIEFING_ROOT status failed"
        return 4
    fi
    echo "$TAG git repo OK"

    # ---- 5.3 DB 可写 (不可重试) ----
    if ! sqlite3 "$BRIEFING_DB" "SELECT 1" >/dev/null 2>&1; then
        preflight_abort "DB not accessible: $BRIEFING_DB"
        return 4
    fi
    echo "$TAG DB OK"

    # ---- 5.4 磁盘 >1GB (不可重试) ----
    local avail_g
    avail_g=$(df -BG "$BRIEFING_ROOT" | awk 'NR==2 {gsub(/G/,"",$4); print $4}')
    if [[ -z "$avail_g" || "$avail_g" -lt 1 ]]; then
        preflight_abort "disk too low: ${avail_g}G available in $BRIEFING_ROOT"
        return 4
    fi
    echo "$TAG disk OK (${avail_g}G available)"

    # ---- 5.5 LLM ping: 最小 prompt 探测, 失败降级为 WARN 不阻塞 pipeline ----
    # 历史教训 (20260418-20): orchestrator ping 写死 gpt-4o-mini, 被 gateway 400,
    # 4 次 attempt 全被 preflight 提前跳过, 自动运行从未成功. 实际 pipeline 用的是
    # OPENAI_MODEL=gpt-5.4, ping 跟 pipeline 走的模型/路径都不是同一个, ping 失败
    # 不代表 pipeline 跑不起来. 所以这里只记 warning, 让 pipeline 自己去试.
    if ! llm_ping; then
        LAST_PREFLIGHT_ERROR="LLM ping failed (http=${LAST_LLM_PING_CODE:-?} body_head=${LAST_LLM_PING_BODY_HEAD:0:120}); proceeding anyway"
        echo "$TAG PRE-FLIGHT WARN: $LAST_PREFLIGHT_ERROR" >&2
        log_jsonl "$CURRENT_ATTEMPT" "preflight_llm_warn" 0 0 "$LAST_PREFLIGHT_ERROR"
    else
        echo "$TAG LLM ping OK"
    fi

    local dur
    dur=$(( $(date +%s) - t0 ))
    log_jsonl 0 "preflight_pass" 0 "$dur" ""
    return 0
}

# llm_ping: 3 次尝试 POST 到 $OPENAI_BASE_URL/v1/chat/completions;
# 至少 1 次 2xx 就返回 0. DRY_RUN mode 跳过但仍返回 0.
# 副作用: 把最近一次 http code 写到 LAST_LLM_PING_CODE, 响应 body 头 200 字节
# 写到 /tmp/claude_ping_body.txt, 供 preflight 告警引用 (不影响返回值逻辑).
LAST_LLM_PING_CODE=""
LAST_LLM_PING_BODY_HEAD=""
llm_ping() {
    LAST_LLM_PING_CODE=""
    LAST_LLM_PING_BODY_HEAD=""
    if [[ "$BRIEFING_DRY_RUN" == "1" ]]; then
        echo "$TAG [dry-run] skipping LLM ping"
        return 0
    fi

    local url="${OPENAI_BASE_URL%/}/v1/chat/completions"
    local model="${OPENAI_MODEL:-gpt-5.4}"
    local body_file="/tmp/claude_ping_body.txt"
    local body
    body='{"model":"'"$model"'","messages":[{"role":"user","content":"ping"}],"max_tokens":1}'
    local i code body_head
    for i in 1 2 3; do
        : > "$body_file"  # truncate; curl --fail-or-empty 才会在失败时清空, 这里显式清
        code=$(curl -sS -o "$body_file" -w '%{http_code}' \
            --max-time 10 \
            -X POST "$url" \
            -H "Authorization: Bearer $OPENAI_API_KEY" \
            -H "Content-Type: application/json" \
            -d "$body" 2>/dev/null || echo "000")
        LAST_LLM_PING_CODE="$code"
        body_head=$(head -c 200 "$body_file" 2>/dev/null | tr -d '\n' || true)
        LAST_LLM_PING_BODY_HEAD="$body_head"
        if [[ "$code" =~ ^2 ]]; then
            return 0
        fi
        echo "$TAG LLM ping attempt $i: http=$code body_head=${body_head:0:120}"
        sleep 2
    done
    return 1
}

# ======================================================================
# 6. 自愈 SQL (D3): 若今天 issue 已推 prod 但 status='generated', 补写
# ======================================================================

selfheal_publish_status() {
    local result
    # 注意用 DATE('now','localtime') (依赖 sqlite 所在机器的时区);
    # 服务器时区是 UTC, 但 issue_date 是 Asia/Shanghai 当天, 所以要用
    # date 命令拿北京 today 再传进去.
    # 2026-04-27 加固: 原版只查 today, 早 6 点启动时今天 issue 还没生成,
    # 当天 promote 推完后再无 selfheal 机会; 第二天又只看新一天的 today,
    # 永远漏修昨天. 改成扫近 7 天兜底, 包含今天和过去一周.
    local today cutoff
    today=$(TZ=Asia/Shanghai date '+%Y-%m-%d')
    cutoff=$(TZ=Asia/Shanghai date -d '7 days ago' '+%Y-%m-%d')
    result=$(sqlite3 "$BRIEFING_DB" <<EOF 2>&1
UPDATE issues SET status='published',
       published_at = COALESCE(published_at, CURRENT_TIMESTAMP)
WHERE issue_date >= '$cutoff'
  AND issue_date <= '$today'
  AND status='generated'
  AND EXISTS(SELECT 1 FROM deliveries
             WHERE issue_id=issues.id
               AND channel='slack_prod' AND status='sent');
SELECT changes();
EOF
)
    local changes="${result##*$'\n'}"
    if [[ "$changes" =~ ^[0-9]+$ ]] && [[ "$changes" -gt 0 ]]; then
        echo "$TAG selfheal: corrected $changes issue(s) generated -> published in [$cutoff..$today]"
        log_jsonl 0 "selfheal" 0 0 "corrected $changes rows"
    else
        echo "$TAG selfheal: no inconsistency found in [$cutoff..$today]"
    fi
}

# ======================================================================
# 7. 短路: 若今天 status='published' 直接 exit 0
# ======================================================================

already_published_today() {
    local today status
    today=$(TZ=Asia/Shanghai date '+%Y-%m-%d')
    status=$(sqlite3 "$BRIEFING_DB" \
        "SELECT status FROM issues WHERE issue_date='$today' ORDER BY id DESC LIMIT 1;" 2>/dev/null)
    if [[ "$status" == "published" ]]; then
        echo "$TAG already published for $today (short-circuit)"
        log_jsonl 0 "short_circuit" 0 0 "today=$today already published"
        return 0
    fi
    return 1
}

# ======================================================================
# 8. D4 时刻表: decide_next_action
#   input: current minutes-since-midnight
#   output: global CURRENT_ATTEMPT (1-4) + CURRENT_ACTION (run|repair|alert|wait|done)
# ======================================================================

# 固定窗口 (Asia/Shanghai):
#   T1 = 06:00 (360)
#   T2 = 06:20 (380)
#   T3 = 06:45 (405)
#   T4 = 07:15 (435)
#   T5 = 08:00 (480)  -> UTC 00:00
#   T6 = 09:00 (540)  -> UTC 01:00
#   DEADLINE = 09:15 (555)
T1=360
T2=380
T3=405
T4=435
T5=480
T6=540
T_DEADLINE=555

# decide_attempt <now_minutes>
# 返回下一个该跑的 attempt 编号 (1-6) 或特殊值 (7=emergency_alert, 8=deadline_passed).
# 规则: 返回 "当前时间 >= Tn 且 < T(n+1)" 的 n.
# 注意: now_minutes 使用 Asia/Shanghai 时间, 范围 0-1439, T5(480)/T6(540) 不跨午夜.
decide_attempt() {
    local now="$1"
    if   (( now < T1 )); then echo 1           # 06:00 前, 立即就跑 attempt 1
    elif (( now < T2 )); then echo 1
    elif (( now < T3 )); then echo 2
    elif (( now < T4 )); then echo 3
    elif (( now < T5 )); then echo 4
    elif (( now < T6 )); then echo 5
    elif (( now < T_DEADLINE )); then echo 6
    elif (( now < T_DEADLINE + 15 )); then echo 7   # 09:15-09:30 告警窗口
    else                       echo 8               # 09:30+ 硬 deadline
    fi
}

# ======================================================================
# 9. 执行单次 pipeline 或 repair
# ======================================================================

# run_pipeline <attempt_num> <mode> : mode = "full" | "repair"
# 返回 pipeline 的 exit code.
run_pipeline() {
    local attempt="$1" mode="$2"
    CURRENT_ATTEMPT="$attempt"
    CURRENT_ACTION="$mode"

    local t0 t1 dur exit_code
    t0=$(date +%s)

    if [[ "$BRIEFING_DRY_RUN" == "1" ]]; then
        echo "$TAG [dry-run] skipping real pipeline for attempt=$attempt mode=$mode"
        t1=$(date +%s); dur=$((t1 - t0))
        log_jsonl "$attempt" "$mode" 0 "$dur" "dry-run skipped"
        return 0
    fi

    if [[ "$mode" == "repair" ]]; then
        local missing
        missing=$(list_missing_sections)
        if [[ -z "$missing" ]]; then
            echo "$TAG repair requested but no missing sections found, skipping"
            t1=$(date +%s); dur=$((t1 - t0))
            log_jsonl "$attempt" "repair_noop" 0 "$dur" "no missing sections"
            return 0
        fi
        echo "$TAG attempt $attempt: repairing sections: $missing"
        # 如果现有 binary 没实现 repair 子命令 (Batch 2.18 由 Team B 做),
        # fallback 到完整 pipeline.
        if ! "$BRIEFING_BIN" --help 2>&1 | grep -q '^    repair'; then
            echo "$TAG repair subcommand not implemented, fallback to full pipeline"
            # --dry-run: 生成所有内容 + payload snapshot, 不发 Slack/飞书.
            # 真正的 Slack/飞书 prod 推送由 post-run.sh 在 GitHub Pages 部署
            # 完成后用 briefing promote 重放 snapshot 完成. 避免 "推送发出时
            # Pages 还没好导致链接 404" (2026-04-22 用户反馈).
            "$BRIEFING_BIN" run --target auto --dry-run
            exit_code=$?
        else
            # repair 本质是 run 的别名, 同样用 --dry-run 避免过早推 Slack.
            "$BRIEFING_BIN" repair --section "$missing" --dry-run
            exit_code=$?
        fi
    else
        # 先跑 daily-reset-dedup (之前是 ExecStartPre; orchestrator 接管后内嵌)
        /usr/local/bin/briefing-daily-reset-dedup.sh || true
        echo "$TAG attempt $attempt: full pipeline (briefing run --target auto --dry-run)"
        # --dry-run: 生成所有内容 + payload snapshot, 不发 Slack/飞书.
        # 真正的 Slack/飞书 prod 推送由 post-run.sh 在 GitHub Pages 部署
        # 完成后用 briefing promote 重放 snapshot 完成. 避免 "推送发出时
        # Pages 还没好导致链接 404" (2026-04-22 用户反馈).
        "$BRIEFING_BIN" run --target auto --dry-run
        exit_code=$?
    fi

    t1=$(date +%s); dur=$((t1 - t0))
    log_jsonl "$attempt" "$mode" "$exit_code" "$dur" ""
    return "$exit_code"
}

# list_missing_sections: 查 DB 今天哪些 section 未 validated, 返回逗号分隔列表.
# 如果 issue_items.status 列不存在 (Team A 还没 migrate), 返回空.
list_missing_sections() {
    local today sections missing
    today=$(TZ=Asia/Shanghai date '+%Y-%m-%d')
    # 判断 status 列是否存在
    if ! sqlite3 "$BRIEFING_DB" "PRAGMA table_info(issue_items);" 2>/dev/null | grep -q '|status|'; then
        echo ""
        return
    fi
    # 期望的 5 个 section
    local expected="product_update research industry opensource social"
    # 今天有哪些 section 是 validated
    sections=$(sqlite3 "$BRIEFING_DB" <<EOF 2>/dev/null
SELECT DISTINCT section FROM issue_items
WHERE issue_id IN (SELECT id FROM issues WHERE issue_date='$today')
  AND status='validated';
EOF
)
    missing=""
    for want in $expected; do
        if ! echo "$sections" | grep -qx "$want"; then
            if [[ -z "$missing" ]]; then
                missing="$want"
            else
                missing="$missing,$want"
            fi
        fi
    done
    echo "$missing"
}

# ======================================================================
# 10. 紧急告警 (07:25 或 pre-flight fail)
# ======================================================================

# send_emergency_alert <reason> <missing> <last_error> <completed> [actual_attempts] [total_attempts] [fail_type]
send_emergency_alert() {
    local reason="$1" missing="$2" last_error="$3" completed="$4"
    local actual_attempts="${5:-$CURRENT_ATTEMPT}" total_attempts="${6:-6}" fail_type="${7:-pipeline_error}"
    if [[ "$BRIEFING_DRY_RUN" == "1" ]]; then
        echo "$TAG [dry-run] skipping emergency alert POST"
        log_jsonl "$CURRENT_ATTEMPT" "emergency_alert_dry_run" 0 0 "$reason"
        return
    fi
    if [[ -z "${SLACK_TEST_WEBHOOK:-}" ]]; then
        echo "$TAG WARN: SLACK_TEST_WEBHOOK not set, cannot alert"
        return
    fi
    local today
    today=$(TZ=Asia/Shanghai date '+%Y-%m-%d')
    local text
    text=$(cat <<EOF
:rotating_light: briefing-daily 经 ${actual_attempts}/${total_attempts} 次尝试仍无法完成
- 原因: $reason
- 失败类型: $fail_type
- 缺失 section: ${missing:-unknown}
- 最后 LLM error: ${last_error:-n/a}
- 当前已完成 sections: ${completed:-unknown}
- 建议命令: briefing repair --date $today --section ${missing:-<填入缺失 section>}
- DB: $BRIEFING_DB
- 日志: journalctl -u briefing-daily.service; cat $BRIEFING_JSONL
EOF
)
    local payload
    payload=$(jq -nc --arg t "$text" '{text:$t}')
    curl -sS --max-time 10 -X POST -H 'Content-Type: application/json' \
        --data "$payload" "$SLACK_TEST_WEBHOOK" >/dev/null 2>&1 || true
    log_jsonl "$CURRENT_ATTEMPT" "emergency_alert" 0 0 "$reason"
    echo "$TAG emergency alert sent: $reason"
}

# ======================================================================
# 11. 主循环: 执行 attempts
# ======================================================================

main_loop() {
    local wait_seconds
    local last_exit=0
    local actual_attempts=0          # 实际已执行的 attempt 计数
    local total_attempts=6           # 总窗口数

    while : ; do
        local now
        now=$(now_minutes)
        local attempt
        attempt=$(decide_attempt "$now")

        case "$attempt" in
            1|2|3|4|5|6)
                # 先查是否已被（别的进程/上次 attempt）推 prod 导致 status=published
                if already_published_today; then return 0; fi

                # 若还没到窗口时间, 等到窗口开始
                local window_start window_label
                case "$attempt" in
                    1) window_start=$T1; window_label="06:00" ;;
                    2) window_start=$T2; window_label="06:20" ;;
                    3) window_start=$T3; window_label="06:45" ;;
                    4) window_start=$T4; window_label="07:15" ;;
                    5) window_start=$T5; window_label="08:00" ;;
                    6) window_start=$T6; window_label="09:00" ;;
                esac
                if (( now < window_start )); then
                    wait_seconds=$(( (window_start - now) * 60 ))
                    echo "$TAG waiting ${wait_seconds}s until window $attempt ($window_label) starts"
                    sleep "$wait_seconds" || return 143
                fi

                # 每次 attempt 前重跑 preflight (LLM ping 失败只 warn, 不阻塞)
                preflight
                local pf_rc=$?
                if (( pf_rc == 4 )); then
                    # config/infra 不可重试, 立即 abort
                    send_emergency_alert "preflight config/infra fail" "" \
                        "${LAST_PREFLIGHT_ERROR:-see journalctl}" "" \
                        "$actual_attempts" "$total_attempts" "preflight_fail"
                    return 4
                fi

                # preflight 通过, 决定 mode
                local mode="full"
                local missing=""
                if (( attempt >= 4 )); then
                    # attempt 4+ 若有 section 跟踪, 走 repair
                    missing=$(list_missing_sections)
                    if [[ -n "$missing" ]]; then
                        mode="repair"
                        echo "$TAG attempt $attempt: missing sections = $missing, using repair mode"
                    else
                        echo "$TAG attempt $attempt: no section tracking / no missing, using full pipeline"
                    fi
                fi

                actual_attempts=$((actual_attempts + 1))
                run_pipeline "$attempt" "$mode"
                last_exit=$?
                case "$last_exit" in
                    0)
                        echo "$TAG attempt $attempt succeeded"
                        return 0
                        ;;
                    2|3)
                        echo "$TAG attempt $attempt transient/content fail (exit=$last_exit), retry next window"
                        ;;
                    4)
                        send_emergency_alert "config/auth fail (exit 4)" "$missing" "see journalctl" "" \
                            "$actual_attempts" "$total_attempts" "pipeline_error"
                        return 4
                        ;;
                    5)
                        send_emergency_alert "infra fail (exit 5)" "$missing" "see journalctl" "" \
                            "$actual_attempts" "$total_attempts" "pipeline_error"
                        return 5
                        ;;
                    *)
                        echo "$TAG attempt $attempt unexpected exit=$last_exit, treating as transient"
                        ;;
                esac
                ;;
            7)
                # 09:15-09:30: 发紧急告警然后 exit 6
                if already_published_today; then return 0; fi
                local missing2 completed
                missing2=$(list_missing_sections)
                local today
                today=$(TZ=Asia/Shanghai date '+%Y-%m-%d')
                completed=$(sqlite3 "$BRIEFING_DB" <<EOF 2>/dev/null
SELECT GROUP_CONCAT(DISTINCT section) FROM issue_items
WHERE issue_id IN (SELECT id FROM issues WHERE issue_date='$today');
EOF
)
                # 截取 journal 最后几条 LLM 错误给用户看
                local last_error
                last_error=$(journalctl -u briefing-daily.service -n 2000 --no-pager 2>/dev/null \
                    | grep -Eio 'http 5[0-9][0-9]|timeout|connection reset|502 bad gateway' \
                    | tail -1 || true)
                send_emergency_alert "重试仍缺 section, 进入 needs_human 状态" \
                    "${missing2:-unknown}" \
                    "${last_error:-n/a}" \
                    "${completed:-n/a}" \
                    "$actual_attempts" "$total_attempts" "pipeline_error"
                return 6
                ;;
            8)
                # 09:30+: 硬 deadline, 不再等
                echo "$TAG past hard deadline (09:15), exiting with 6"
                log_jsonl "$CURRENT_ATTEMPT" "deadline_passed" 6 0 "past T_DEADLINE (09:15) without success"
                return 6
                ;;
        esac

        # 如果 BRIEFING_DEADLINE_MINUTES 是测试模式 (很小), 防止死循环
        local elapsed
        elapsed=$(( ($(date +%s) - STARTED_AT) / 60 ))
        if (( elapsed >= BRIEFING_DEADLINE_MINUTES )); then
            echo "$TAG hit BRIEFING_DEADLINE_MINUTES=$BRIEFING_DEADLINE_MINUTES, aborting"
            log_jsonl "$CURRENT_ATTEMPT" "test_deadline" 6 0 "hit deadline_minutes=$BRIEFING_DEADLINE_MINUTES"
            return 6
        fi
    done
}

# ======================================================================
# 12. 执行顺序
# ======================================================================

# 12.1 首次 pre-flight (启动时检查一次, 不可重试的问题立即 abort)
preflight
PF_RC=$?
if (( PF_RC == 4 )); then
    send_emergency_alert "startup preflight config/infra fail" "" \
        "${LAST_PREFLIGHT_ERROR:-see journalctl}" "" \
        "0" "6" "preflight_fail"
    exit 4
fi
# PF_RC==2 (LLM 暂时不可达) 不 exit, 交给 main_loop 重试

# 12.2 D3 自愈 SQL
selfheal_publish_status

# 12.3 短路
if already_published_today; then
    exit 0
fi

# 12.4 只做 pre-flight 就退出 (smoke test 用)
if [[ "$BRIEFING_PREFLIGHT_ONLY" == "1" ]]; then
    echo "$TAG BRIEFING_PREFLIGHT_ONLY=1, exiting after pre-flight"
    exit 0
fi

# 12.5 主循环
main_loop
FINAL_EXIT=$?

# 12.6 post-run: 仅在 main_loop 成功时执行 (替代 systemd ExecStartPost)
if (( FINAL_EXIT == 0 )); then
    echo "$TAG running post-run tasks..."
    if [[ -x /usr/local/bin/briefing-post-run.sh ]]; then
        /usr/local/bin/briefing-post-run.sh || {
            echo "$TAG WARN: post-run.sh failed (exit=$?), non-fatal"
            log_jsonl "$CURRENT_ATTEMPT" "post_run_fail" "$?" 0 "post-run.sh failed"
        }
    else
        echo "$TAG WARN: /usr/local/bin/briefing-post-run.sh not found or not executable"
    fi
fi

log_jsonl "$CURRENT_ATTEMPT" "orchestrator_exit" "$FINAL_EXIT" "$(( $(date +%s) - STARTED_AT ))" ""
exit "$FINAL_EXIT"
