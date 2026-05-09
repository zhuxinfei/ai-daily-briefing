#!/usr/bin/env bash
# mac-install-mirror.sh
#
# 在 Mac 上一键安装 briefing-v3 + ai-daily-site 的只读镜像 + launchd 定时拉取.
# 幂等: 重复执行不会破坏已有状态.
#
# NOTE: /install.sh 是本脚本的 verbatim 拷贝 (提供短 URL 给 Mac curl-exec).
# 改本脚本后必须同步: cp scripts/mac-install-mirror.sh install.sh
# GitHub raw 不跟 symlink, 所以不能用 symlink 替代.
#
# 用法: 把本脚本粘贴/scp 到 Mac 后执行
#
#   bash mac-install-mirror.sh
#
# 或在 Mac 上直接 curl 执行:
#
#   curl -fsSL https://raw.githubusercontent.com/ylzsdafei/ai-daily-briefing/main/scripts/mac-install-mirror.sh | bash
#
# 执行完成后:
#   - ~/mirror/briefing-v3       (镜像 1)
#   - ~/mirror/ai-daily-site     (镜像 2)
#   - ~/mirror/_pull.sh          (手动拉取命令)
#   - ~/mirror/_sync.log         (拉取日志)
#   - launchd 每天 07:00 / 23:00 自动跑 + 开机自动跑一次
#
# 前置条件:
#   - Mac 已装 git (xcode-select --install 即可)
#   - 不需要任何认证: 两个 repo 都是 public, 走 HTTPS 只读 fetch
#
# 幂等细节:
#   - 若目标目录已含其它内容 (如 Syncthing 的 .stfolder), 会 mv 到 _trash_*/
#   - 若已是 git repo, fetch + reset --hard origin/main 强制对齐

set -euo pipefail

MIRROR_ROOT="$HOME/mirror"
LOG_FILE="$MIRROR_ROOT/_sync.log"
PULL_SCRIPT="$MIRROR_ROOT/_pull.sh"
PLIST="$HOME/Library/LaunchAgents/com.briefing.mirror.plist"
LABEL="com.briefing.mirror"

REPOS=(
    "https://github.com/ylzsdafei/ai-daily-briefing.git|briefing-v3"
    "https://github.com/ylzsdafei/ai-daily-site.git|ai-daily-site"
)

mkdir -p "$MIRROR_ROOT" "$HOME/Library/LaunchAgents"
touch "$LOG_FILE"

# ---------------------------------------------------------------------
# 1. 初始化 / 更新 (用 init + fetch + reset 模式, 不怕非空目录)
# ---------------------------------------------------------------------
for entry in "${REPOS[@]}"; do
    url="${entry%|*}"
    name="${entry#*|}"
    path="$MIRROR_ROOT/$name"
    mkdir -p "$path"

    if [ -d "$path/.git" ]; then
        echo "[setup] updating $name..."
        git -C "$path" remote set-url origin "$url"
    else
        echo "[setup] initializing $name..."
        if [ -n "$(ls -A "$path" 2>/dev/null)" ]; then
            trash="$MIRROR_ROOT/_trash_$(date +%Y%m%d_%H%M%S)_$name"
            mkdir -p "$trash"
            # dotglob 捕获 .stfolder 等隐藏文件, 包括 ..config 这类 (原 .[!.]* 漏)
            ( shopt -s dotglob nullglob; mv "$path"/* "$trash"/ 2>/dev/null || true )
            echo "[setup]   (non-git leftovers moved to $trash)"
        fi
        git -C "$path" init -b main -q
        git -C "$path" remote add origin "$url"
    fi

    git -C "$path" fetch origin main
    git -C "$path" reset --hard origin/main
done

# ---------------------------------------------------------------------
# 2. 写 _pull.sh (launchd 和手动执行都用它)
# ---------------------------------------------------------------------
cat > "$PULL_SCRIPT" <<'SCRIPT'
#!/usr/bin/env bash
# 被 launchd 和用户手动调用. 只读镜像: fetch + reset --hard origin/main.
export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
MIRROR_ROOT="$HOME/mirror"
LOG="$MIRROR_ROOT/_sync.log"
ts() { date '+%Y-%m-%d %H:%M:%S %Z'; }
for name in briefing-v3 ai-daily-site; do
    path="$MIRROR_ROOT/$name"
    [ -d "$path/.git" ] || continue
    before=$(git -C "$path" rev-parse --short HEAD 2>/dev/null)
    if git -C "$path" fetch --all --prune >/dev/null 2>&1 \
        && git -C "$path" reset --hard origin/main >/dev/null 2>&1; then
        after=$(git -C "$path" rev-parse --short HEAD)
        if [ "$before" = "$after" ]; then
            echo "$(ts) [$name] no change ($after)" >> "$LOG"
        else
            echo "$(ts) [$name] $before -> $after" >> "$LOG"
        fi
    else
        echo "$(ts) [$name] FETCH/RESET FAILED" >> "$LOG"
    fi
done
SCRIPT
chmod +x "$PULL_SCRIPT"

# ---------------------------------------------------------------------
# 3. 装 launchd: 每天 07:00 / 23:00 + RunAtLoad
# ---------------------------------------------------------------------
cat > "$PLIST" <<PLIST_EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>${LABEL}</string>
    <key>ProgramArguments</key>
    <array>
        <string>/bin/bash</string>
        <string>${PULL_SCRIPT}</string>
    </array>
    <key>StartCalendarInterval</key>
    <array>
        <dict>
            <key>Hour</key><integer>7</integer>
            <key>Minute</key><integer>0</integer>
        </dict>
        <dict>
            <key>Hour</key><integer>23</integer>
            <key>Minute</key><integer>0</integer>
        </dict>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>StandardOutPath</key>
    <string>${MIRROR_ROOT}/_launchd.log</string>
    <key>StandardErrorPath</key>
    <string>${MIRROR_ROOT}/_launchd.err</string>
</dict>
</plist>
PLIST_EOF

launchctl unload "$PLIST" 2>/dev/null || true
launchctl load "$PLIST"

# ---------------------------------------------------------------------
# 4. 结果报告
# ---------------------------------------------------------------------
echo ""
echo "============================================================"
echo "[setup] 安装完成"
echo "============================================================"
echo "镜像目录:"
ls -la "$MIRROR_ROOT" | grep -E '(briefing-v3|ai-daily-site|_pull|_sync)' || true
echo ""
echo "launchd 状态:"
launchctl list | grep "$LABEL" || echo "(未加载)"
echo ""
echo "下次自动拉取: 每天 07:00 / 23:00 (本机时区)"
echo "立即手动拉取: ~/mirror/_pull.sh"
echo "查看同步历史: tail -n 20 ~/mirror/_sync.log"
