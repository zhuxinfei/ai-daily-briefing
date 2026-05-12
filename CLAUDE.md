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

# 2. API 环境
export OPENAI_API_KEY="YOUR_OPENAI_API_KEY"
export OPENAI_BASE_URL="https://api.gjs.ink"
export OPENAI_MODEL="gpt-5.4"
export AI_DAILY_SITE_PUSH_TOKEN="YOUR_GITHUB_PAT"

# 3. 工作目录必须是 repo 根目录
cd /path/to/ai-daily-briefing

# 4. 验证：config 文件存在
test -f config/ai.yaml || { echo "MISSING config/ai.yaml"; exit 1; }

# 5. 运行
./briefing migrate && ./briefing seed
./briefing run --date $(TZ=Asia/Shanghai date +%F) --domain ai --target test

# 6. 验证产出
ls -la $HEXTRA_SITE_DIR/content/cn/$(date +%Y)/$(date +%Y-%m)/$(date +%F).md
```

## 修复清单

### ✅ P0: 扩展窗口超时
- **根因**: `extended_hours: 48` 触发第二轮 60 批 LLM 排序（+14 分钟），耗光 parent context，导致 infocard/Python/Slack 全超时
- **修复**: `extended_hours: 0`
- **验证**: 运行 pipeline，只看到一轮 rank，infocard 完整产出

### ✅ P1: CF Worker cron 不触发【已验证：free tier 就是不会触发】
- **根因**: CF free tier cron trigger API 接受但不实际执行（* * * * * 每分 cron 等了 2 分钟 0 events）
- **主力方案**: launchd + caffeinate 每天 06:00 自动运行
- **备用方案**: CF Worker 手动触发 `GET /run` 或在 cron-job.org 注册免费账号

### ✅ P2: launchd 脚本 bug
- **根因**: GOMODCACHE 写成 `/pkg`（只读），未 cd 到 repo 根目录
- **修复**: 脚本用绝对路径 `$HOME/gopath/pkg/mod`，执行前 cd
- **验证**: 手动 `launchctl start` 测试

### ✅ P3: 大字报字体路径
- **根因**: 硬编码 Linux 路径 `/usr/share/fonts/...`
- **修复**: 改为 macOS 字体 `/System/Library/Fonts/STHeiti Medium.ttc`
- **验证**: `data/images/20xx-xx-xx.png` 生成成功

### ✅ P4: 外部图片 HEAD 校验过严
- **根因**: 从国内网络 HEAD 请求海外图片源 5s 超时
- **修复**: 超时 15s，去掉 5KB 下限，HEAD 失败回退 GET
- **验证**: 日报产出 >10 张图片

## 常见错误

1. `config/ai.yaml: no such file or directory` → 不在 repo 根目录
2. `could not create module cache: mkdir /pkg` → GOMODCACHE 路径错误
3. Mac 睡眠丢 launchd → 没加 `caffeinate -i`
4. `context deadline exceeded` after 30min → extended_hours 打开了
