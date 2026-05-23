# briefing-v3 · 任务交接文档

**版本**：v1.0.0-rc5 · feature/infocard-l1-l2 branch
**日期**：2026-04-10
**目的**：防止当前会话意外退出后上下文丢失，新会话可以沿着这份文档把 v1.0.0 推到完成状态。

---

## 1. 需求一句话

> 自主生成一个**不逊色于 https://ai.hubtoday.app/** 的 AI 每日早报网站 + Slack 推送系统，完全摆脱对上游 `justlovemaki/CloudFlare-AI-Insight-Daily` 仓库的依赖。

## 2. 硬需求清单（v1.0.0 不可妥协）

| # | 需求 | 状态 |
|---|---|---|
| 1 | **5 个 section**：产品与功能更新 / 前沿研究 / 行业展望与社会影响 / 开源 TOP / 社媒分享 | ✅ |
| 2 | **20+ items / 天**，密度对齐上游 | ✅（当天 20 条） |
| 3 | **Slack 双通道**：test 无条件，prod 仅在 gate pass 时 | ✅ |
| 4 | **不允许静默降级**：任一强制环节失败 → 硬停 + 告警到 test 频道 | ✅ |
| 5 | **测试期默认只推 test 频道** | ✅（`--target test`） |
| 6 | **零成本**：只用已有 OpenAI-compatible 网关 | ✅ |
| 7 | **24h 时间窗**，条目不够再扩到 48h | ✅ |
| 8 | **/api/chat 真 AI 对话**，marked.js 渲染 markdown | ✅（serve.go） |
| 9 | **浅色 Hextra 风 UI + 左侧年月日归档** | ✅（render/html.go） |
| 10 | **专业术语为非技术用户加注释** | ✅（classify + compose prompts） |
| 11 | **所有链接按钮化**，无裸露 URL | ✅（`src-btn`） |
| 12 | **图片必须与内容相关**：优先 og:image → fallback PIL 信息卡 | ✅（mediaextract + infocard） |
| 13 | **两层图片架构**：L1 大字报（每期 1 张）+ L2 信息卡（每条 1 张） | ✅（本次做完） |
| 14 | **稳中求进**，不允许为赶进度放弃质量 | ✅ |
| 15 | **单个 item 渲染失败不能拖垮整条 pipeline** | ✅（recover + continue） |

## 3. 总体技术方案

### 3.1 进程拓扑

```
┌───────────────────────────────┐     ┌─────────────────────────┐
│ briefing-serve.service        │     │ cloudflared-tunnel.svc  │
│ ./bin/briefing serve          │◄────┤ cloudflared --url ...   │
│ :8080  /docs /api/chat        │     │ Quick Tunnel (HTTPS CDN)│
└───────────────────────────────┘     └─────────────────────────┘
          ▲                                       ▲
          │ 读                                    │ 对外访问
          │                                       │
┌─────────┴─────────┐                    ┌────────┴──────────┐
│ docs/*.html       │◄───────────────────┤ 用户浏览器         │
│ data/images/...   │                    │ Slack 按钮跳转     │
│ data/briefing.db  │                    └───────────────────┘
└───────────────────┘
          ▲
          │ 写入
          │
┌─────────┴─────────────────┐
│ briefing run (cron 每早)  │
│ → ingest → rank →         │
│   classify → compose →    │
│   insight → summary →     │
│   infocard (LLM+PIL) →    │
│   render HTML + MD →      │
│   gate → publish Slack    │
└───────────────────────────┘
```

### 3.2 Pipeline 13 步

```
0. open store + migrate
1. upsert issue row
2. concurrent ingest (21 sources, 20s 超时 / 源)
3. persist raw_items (sequential in-memory IDs to fix rank bug)
4. filter by 24h window
5. rank (LLM quality scoring)
6. classify (LLM section assignment)
7. compose (LLM per-section summarization)
7b. mediaextract (fallback og:image/video)
8. persist issue_items
9. insight (LLM industry + takeaways)
10. summary (LLM 3-line 头条大字报)
10b. ★ infocard (LLM 出 20+1 张卡 JSON → PIL 渲染 PNG → 注入 BodyMD)
11. render markdown + HTML
12. gen_headline.py fallback PNG
12b. HTML + index.html refresh
13. gate check
14. Slack test publish (强制)
15. Slack prod publish (gate pass + target=prod)
16. mark issue published
```

### 3.3 数据流关键

- **源**：21 条 RSS/API 适配器，详见 `internal/ingest/`
- **LLM**：OpenAI-compatible 网关 `http://64.186.239.99:8080`，model `gpt-5.4`
- **DB**：SQLite `data/briefing.db`，schema 由 `internal/store/migrations/`
- **HTML 产物**：`docs/YYYY-MM-DD.html` + `docs/index.html`
- **图片产物**：
  - `data/images/cards/YYYY-MM-DD/header.png`（L1 大字报）
  - `data/images/cards/YYYY-MM-DD/item-1.png ... item-N.png`（L2 信息卡）
  - `data/images/YYYY-MM-DD.png`（老的 gen_headline fallback）
- **Slack webhook**：读 `config/secrets.env` 里的 `SLACK_TEST_WEBHOOK` / `SLACK_PROD_WEBHOOK`
- **对外 URL**：Cloudflare Quick Tunnel，每次重启都换 URL，需要手动写 `BRIEFING_REPORT_URL_BASE`

## 4. 本次（feature/infocard-l1-l2）做了什么

### 4.1 新增文件

| 路径 | 作用 |
|---|---|
| `internal/infocard/infocard.go` | LLM 批量出卡片 JSON 的 Go 包，自己的 OpenAI client + 严格 JSON 解析 |
| `scripts/gen_info_card.py` | PIL 报纸风渲染器，`--mode item\|header`，stdin 读 JSON |
| `docs/specs/HANDOVER.md` | 本文档 |

### 4.2 修改文件

| 路径 | 改动 |
|---|---|
| `cmd/briefing/run.go` | 1) 新增 imports: `os/exec`, `briefing-v3/internal/infocard` 2) 10b 段：调 `infocard.Generate` + 渲染 N+1 张 PNG + 把 `![alt](../data/images/cards/...)` 塞进 IssueItem.BodyMD 最前面 3) 12b 段：HTML hero 图优先用 `header.png` 4) `renderInfoCardPNG` 辅助函数：JSON marshal → exec python3 → 30s 超时 5) 单条失败 recover+continue，绝不拖垮 pipeline |
| `cmd/briefing/serve.go` | 早前加了 `/api/chat` endpoint，注入 SQLite issue context 后调 OpenAI-compatible |
| `/etc/systemd/system/briefing-serve.service` | 加 `EnvironmentFile=config/secrets.env`，不然 /api/chat 返回 503 |
| `internal/render/html.go` | 早前重写过：浅色 Hextra 主题 + 左侧年月日归档 + 浮动 chat widget + marked.js |
| `internal/mediaextract/fetch.go` | 黑名单加强（logo/banner/avatar/sprite/icon/thumbnails/1x1），多候选图扫描 |
| `config/secrets.env` | 更新 `BRIEFING_REPORT_URL_BASE` 为当前 Quick Tunnel URL |

### 4.3 关键 bug + fix 记录（避免重复踩坑）

1. **rank 967→1**：`InsertRawItems` 不回填 AUTOINCREMENT id，导致 rank 的 byID map collapse。**Fix**：在 insert 之后给 rawItems 分配内存态的顺序 id `for i, it := range rawItems { it.ID = int64(i+1) }`。
2. **summarize 502**：OpenAI 网关偶发 502。**Fix**：retry 3→5，指数回退 1/2/4/8s，maxItems 10→8，maxExcerpt 400→250。
3. **item_seq collision**（本次发现）：compose.Seq 按 section 重启，跨 section 有 1..N 冲突，导致多个新闻点写到同一张 PNG。**Fix**：run.go 里做 shadow slice + uidToItem 映射，LLM 只看全局唯一 uid。
4. **Cloudflared URL 漂移**：`systemctl restart briefing-serve` 因为 `Requires=briefing-serve.service` 级联重启 tunnel 导致 URL 换掉。**教训**：更新 secrets.env 后**不要** restart serve，运行期 env 从 systemd EnvironmentFile 加载，下一次 briefing run 自然读到新值。
5. **mediaextract 重复 logo**：og:image 常返回站 logo。**Fix**：扩 tracker 黑名单，多候选图扫描 top 20。

## 5. 当前状态

### 5.1 已完成（可以 demo）

- ✅ 21 源并发采集 → 2668 raw items / 955 filtered / 30 ranked / 20 composed
- ✅ Gate pass=true（items≥20, 5 sections, insightChars>700, 7 domains）
- ✅ Slack test 推送成功（有 Quick Tunnel URL 按钮）
- ✅ L1 大字报 PNG + L2 信息卡 PNG 全部渲染（之前一版有 seq 冲突只出 8 张，已修复，等 rerun 验证）
- ✅ /api/chat 真 AI 对话 + marked.js 渲染
- ✅ 浅色 Hextra UI + 左侧归档 sidebar

### 5.2 待做（v1.0.0 收尾）

1. **Rerun pipeline**（修了 seq collision 的版本）→ 验证 20/20 PNG 全部独立 + 推 Slack test 让用户看新效果
2. **Feature branch commit + push**：本次所有改动备份（本文档是这一步的一部分）
3. **cron 定时任务**：`scripts/cron.sh` 装到 crontab，每早 08:00 UTC 跑 `briefing run --date $(date -I) --target auto`
4. **v1.0.0 upgrade doc**（task #11）：在 `docs/specs/UPGRADE_LOG.md` 写清 v1.0.0 相对 AI-Insight-Daily 的差异、迁移路径、运维入口

### 5.3 延后到 v1.0.1

1. **Named Tunnel**：让 tunnel URL 稳定（需要用户到 CF Dashboard 创建 token，我这边没网络不可达）
2. **History dedup**：同一 URL 跨天不重复（需要 `raw_items.url` 全局 unique index + 跨日查询）
3. **ReactFlow / AntV G6 交互式知识图谱**（可选）
4. **Pollinations AI 插图 fallback**（已有 `internal/illustration/`，没接线）

## 6. 运行方式（完整手册）

### 6.1 一次手动 run

```bash
cd /root/briefing-v3
set -a; source config/secrets.env; set +a
./bin/briefing run --date 2026-04-10 --domain ai --target test
```

`--target` 可选：
- `test`：只推 test 频道
- `prod`：gate pass 才推 prod 频道
- `auto`：test 一定推，prod 看 gate

### 6.2 Serve + tunnel（已是 systemd 托管）

```bash
systemctl status briefing-serve         # HTTP 服务器 :8080
systemctl status cloudflared-tunnel     # Quick Tunnel
tail -f logs/serve.log                  # 请求日志
tail -f logs/cf-tunnel.log              # 抓当前 trycloudflare URL
```

### 6.3 拿当前 tunnel URL

```bash
grep -oE 'https://[a-z0-9-]+\.trycloudflare\.com' logs/cf-tunnel.log | tail -1
```

拿到新 URL 后更新 `config/secrets.env` 的 `BRIEFING_REPORT_URL_BASE`，**不要** restart briefing-serve（否则 tunnel 又漂），下一次 briefing run 自动读新值。

### 6.4 Rebuild

```bash
cd /root/briefing-v3
/usr/local/go/bin/go build -o bin/briefing ./cmd/briefing
```

### 6.5 重新推一条消息

```bash
set -a; source config/secrets.env; set +a
./bin/briefing run --date 2026-04-10 --domain ai --target test
```

## 7. 目录速查

```
/root/briefing-v3/
├── bin/briefing              # 编译产物
├── cmd/briefing/
│   ├── main.go               # CLI entrypoint
│   ├── run.go                # ★ 核心 pipeline 接线（含本次 infocard 改动）
│   ├── serve.go              # HTTP server + /api/chat
│   └── seed.go               # 往 SQLite 写 source 配置
├── config/
│   ├── ai.yaml               # pipeline 配置（21 源 + gate 参数）
│   └── secrets.env           # OPENAI_API_KEY + Slack webhook + tunnel URL
├── data/
│   ├── briefing.db           # SQLite 库
│   └── images/cards/YYYY-MM-DD/  # ★ 本次新增：L1 header.png + L2 item-N.png
├── docs/
│   ├── YYYY-MM-DD.html       # 每日早报页
│   ├── index.html            # 归档首页
│   └── specs/
│       ├── HANDOVER.md       # ★ 本文档
│       ├── UPGRADE_LOG.md    # v1.0.0 升级记录
│       └── 2026-04-10-briefing-v3-design.md
├── internal/
│   ├── classify/             # LLM 分类器
│   ├── compose/              # LLM 分段撰稿
│   ├── config/               # ai.yaml loader
│   ├── gate/                 # 硬质量门
│   ├── generate/             # LLM 网关
│   ├── illustration/         # （未接线）Pollinations
│   ├── image/                # 老版 gen_headline.py wrapper
│   ├── infocard/             # ★ 本次新增：LLM 批量出卡片 JSON
│   ├── ingest/               # 21 源适配器
│   ├── mediaextract/         # og:image/video 抓取
│   ├── publish/              # Slack webhook 封装
│   ├── rank/                 # LLM 质量打分
│   ├── render/               # markdown + HTML + Slack Block Kit
│   └── store/                # SQLite schema + queries
├── scripts/
│   ├── cron.sh               # 定时运行脚手架
│   ├── gen_headline.py       # 老版头图
│   └── gen_info_card.py      # ★ 本次新增：PIL 报纸风信息卡
├── logs/                     # runtime 日志
│   ├── cf-tunnel.log         # cloudflared
│   ├── serve.log             # HTTP server
│   └── run-YYYYMMDD-HHMMSS.log # 每次手动 run
└── daily/                    # 每天的 flat markdown 备份
    └── YYYY-MM-DD.md
```

## 8. 关键设计决策与"为什么这样做"

1. **两层图片架构（L1/L2）**：上游 ai.hubtoday.app 每期只有一张大字报式头图 + 每条新闻一张解释图。我们 L1 走 `--mode header`，L2 走 `--mode item`，都是同一张 PIL 模板。
2. **mediaextract 只做 fallback**：og:image 太容易撞 logo/banner，LLM 生出的结构化卡片永远比抓图稳。
3. **infocard 单次 LLM call**：所有卡片一次出完，token 成本线性不是 N 倍。
4. **shadow UID 映射**：不改 compose 的 Seq 语义（HTML 渲染依赖它），只在 run.go 的 infocard 段做本地映射。
5. **Quick Tunnel vs Named Tunnel**：Quick Tunnel 零配置但 URL 漂移；Named Tunnel 稳定但需要 cert.pem（当前环境网络不可达 CF origin）。v1.0.1 再做。
6. **硬失败 vs 软失败**：
   - 硬失败（退出 1）：ingest 为空 / filter 为空 / rank 为空 / compose 报错 / insight 报错 / summary 报错 / gate fail（仅在 --target prod）/ Slack test 推送失败
   - 软失败（WARN 继续）：mediaextract 0 命中 / infocard 整段失败 / 单张 PNG 渲染失败 / ReplaceIssueItems 重写失败 / gen_headline.py 失败 / HTML 写入失败 / index 刷新失败

## 9. 继续工作的"第一动作"

下一个会话进来第一件事：

```bash
cd /root/briefing-v3
git status
git log --oneline -10
tail -50 logs/run-*.log | tail -50
```

看 git 状态 + 最后一次 run 结果，如果 infocard 那步有 20/20 PNG 独立，就直接进 v1.0.0 收尾（cron + upgrade log）。否则先修 run.go 的 infocard 段。

---

**最后一次更新**：2026-04-10 12:15 UTC，feature/infocard-l1-l2 分支，刚修完 seq collision 待 rerun 验证。
