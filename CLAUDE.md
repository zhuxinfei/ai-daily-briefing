# CLAUDE.md — AI Daily Briefing

## 原则：验证优于假设

每个自动化环节必须端到端验证，不能"部署了事"。
CF Worker cron API 返回成功但实际不触发（free tier 限制）。
launchd plist 语法正确但脚本 bug 导致静默失败。

## 项目架构

- **Pipeline**: Go 二进制 (`cmd/briefing`)，从 `config/ai.yaml` 读取配置
- **LLM**: api.gjs.ink (gpt-5.4)，env: `OPENAI_BASE_URL` / `OPENAI_API_KEY` / `OPENAI_MODEL`
- **站点**: GitHub Pages，仓库 zhuxinfei/ai-daily-site
- **内容路径**: `content/cn/{year}/{year-month}/{date}.md`

## 运行必检清单

```bash
# 1. Go 环境
export GOROOT=$HOME/go GOPATH=$HOME/gopath GOMODCACHE=$HOME/gopath/pkg/mod
export PATH=$GOROOT/bin:$GOPATH/bin:$PATH GOPROXY="https://goproxy.cn,direct" GOTOOLCHAIN=auto
mkdir -p $GOPATH $GOMODCACHE

# 2. API 环境（替换为实际 key）
export OPENAI_API_KEY="YOUR_OPENAI_API_KEY"
export OPENAI_BASE_URL="https://api.gjs.ink"
export OPENAI_MODEL="gpt-5.4"
export AI_DAILY_SITE_PUSH_TOKEN="YOUR_GITHUB_PAT"
export HEXTRA_SITE_DIR="/path/to/ai-daily-site"
export BRIEFING_REPORT_URL_BASE="https://your-site.github.io/ai-daily-site/{{YEAR}}/{{YEARMONTH}}/{{DATE}}/"
export PUBLISH_TARGET="test" BRIEFING_MODE="prod" BRIEFING_SKIP_IF_REPORT_EXISTS="1"
export SLACK_TEST_WEBHOOK="https://hooks.slack.com/services/disabled"

# 3. 构建 + 运行
cd /path/to/ai-daily-briefing
go build -o /tmp/briefing ./cmd/briefing
/tmp/briefing migrate && /tmp/briefing seed
/tmp/briefing run --date $(TZ=Asia/Shanghai date +%F) --domain ai --target test

# 4. 验证产出
ls -la $HEXTRA_SITE_DIR/content/cn/$(TZ=Asia/Shanghai date +%Y)/$(TZ=Asia/Shanghai date +%Y-%m)/$(TZ=Asia/Shanghai date +%F).md
```

## 修复记录

### P0: 扩展窗口超时（已修复）
- **根因**: `extended_hours: 48` 触发第二轮 LLM 排序，耗光时间
- **修复**: `extended_hours: 0`
- **验证**: 单轮 rank，infocard 12/12 完整产出

### P1: CF Worker cron 不触发（已确认）
- **结论**: free tier 不会触发，已用 cron-job.org 替代

### P2: launchd 脚本 bug（已修复）
- **根因**: GOMODCACHE 指向只读路径 + 未 cd 到 repo 根目录
- **修复**: 绝对路径 + 执行前 cd

### P3: 大字报字体路径（已修复）
- **修复**: `/System/Library/Fonts/STHeiti Medium.ttc`

### P4: 外部图片校验过严（已修复）
- **修复**: 超时 15s，去掉 5KB 下限，HEAD 失败回退 GET

## 常见错误

1. `config/ai.yaml: no such file or directory` → 不在 repo 根目录
2. `could not create module cache: mkdir /pkg` → GOMODCACHE 路径错误
3. Mac 睡眠丢 launchd → 没加 `caffeinate -i`
4. `context deadline exceeded` after 30min → extended_hours 打开了
