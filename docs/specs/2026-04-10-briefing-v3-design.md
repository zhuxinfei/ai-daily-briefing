# Briefing v3: AI 领域早报系统设计

**版本**: 0.1 Draft
**日期**: 2026-04-10
**架构师**: Claude Opus 4.6 via Claude Code
**状态**: 待用户 review

---

## 1. 背景

- 当前中转版本 `CloudFlare-AI-Insight-Daily` 是基于 upstream `justlovemaki/CloudFlare-AI-Insight-Daily` 二开的 Slack 推送链路
- 主要问题：数据源完全依赖 upstream（别人的 markdown），我们没有对内容质量和节奏的控制权
- 长期目标：沉淀一套**可复用的通用领域资讯方案**，AI 早报是第一个 case

## 2. 目标

**Day 1 MVP**:
- 用自己的 Go 程序跑通"采集 → 生成 → 推送 Slack"的 E2E 链路，不依赖 upstream raw
- 明早由新系统推送（或由中转版本兜底）

**Day 1-7 完整目标**:
- 完整复制 upstream `ai.hubtoday.app` 的内容深度（7 section / 20+ 条目 / 50+ 源链接）
- 多渠道分发：Slack + 飞书云文档 + 飞书群机器人
- 结构化数据长期存储，支持业务分析和主业务 DB 联动
- 整个过程零成本（不用 Docker / Redis / Postgres / 付费服务）

## 3. 非目标（今天不做）

- 原创数据源发现（继续用 smol.ai / GitHub Trending / Folo 等已知源）
- 多领域切换的实际运行（代码留扩展点，今天只跑 `ai` 一个 domain）
- 静态 HTML 站点（Day 4 再做）
- 视频 / 音频内容生成
- 用户反馈闭环
- 向量检索 / 语义去重

## 4. 核心决策

| 决策项 | 选择 | 关键理由 |
|---|---|---|
| 语言 | **Go 1.22** | 跟主业务栈一致 |
| 存储 | **SQLite (modernc.org/sqlite 纯 Go)** | 零运维，后端同事认可的整合路径 |
| 飞书 SDK | **larksuite/oapi-sdk-go/v3** 官方 | 云文档/多维表/群消息统一支持 |
| LLM | 现有 OpenAI 兼容 API (gpt-5.4) | 免费复用 |
| 调度 | 服务器 cron | 避开 GHA schedule 丢失风险 |
| 部署 | 单二进制 + 单 SQLite 文件 | 零成本零运维 |
| 隔离 | 新目录 `/root/briefing-v3/` + 新 repo | 中转版本保持兜底 |
| 多领域 | `domain_id` 贯穿 schema，代码参数化 | 为未来留空，今天不实现切换 |

## 5. 技术栈

```
Go 1.22+
├── DB        modernc.org/sqlite          (纯 Go，无 cgo)
├── Feishu    github.com/larksuite/oapi-sdk-go/v3
├── OpenAI    github.com/sashabaranov/go-openai
├── HTTP      net/http (标准库)
├── HTML 抓取 github.com/PuerkitoBio/goquery
├── Markdown  github.com/yuin/goldmark
├── YAML      gopkg.in/yaml.v3
├── RSS       github.com/mmcdole/gofeed
└── 日志      log/slog (标准库)
```

零 Docker 依赖，单 `go build` 出二进制。

## 6. 目录结构

```
/root/briefing-v3/
├── cmd/briefing/main.go           # 单二进制入口 + 子命令 CLI
├── internal/
│   ├── config/                    # YAML 配置加载 + env
│   ├── store/                     # SQLite 访问层 + migrations
│   ├── ingest/                    # 数据采集
│   │   ├── source.go              # Source interface
│   │   ├── github_trending.go     # Day 1
│   │   ├── smolai_rss.go          # Day 1
│   │   ├── folo.go                # Day 2
│   │   └── rss.go                 # 通用 RSS
│   ├── extract/                   # 源链接正文抓取（Day 2+）
│   ├── compose/                   # raw_items → issue_items（规则 + LLM 分类）
│   ├── generate/                  # LLM 洞察 + 启发生成
│   │   ├── prompt.go              # 复用 slack-notify.js 已 hardened prompt
│   │   └── validate.go            # banned pattern + 重写
│   ├── render/                    # Slack blocks / Feishu doc / HTML
│   └── publish/                   # Slack / 飞书云文档 / 飞书群机器人
├── config/
│   ├── ai.yaml                    # AI 领域配置
│   └── secrets.env                # 环境变量（不入 git）
├── data/
│   └── briefing.db                # SQLite 主存
├── migrations/                    # SQL 升级脚本
├── scripts/
│   ├── backfill.go                # 历史数据导入
│   └── cron.sh                    # 服务器 cron 入口
├── docs/specs/                    # 设计文档
├── go.mod
└── README.md
```

## 7. 数据模型（SQLite Schema）

```sql
-- 领域（一等公民，为多 domain 留空）
CREATE TABLE domains (
    id          TEXT PRIMARY KEY,          -- 'ai', 'web3', ...
    name        TEXT NOT NULL,
    config_path TEXT,                      -- config/{id}.yaml
    created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- 数据源
CREATE TABLE sources (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    domain_id   TEXT NOT NULL REFERENCES domains(id),
    type        TEXT NOT NULL,             -- 'github_trending' | 'rss' | 'folo' | ...
    name        TEXT NOT NULL,
    config_json TEXT NOT NULL,             -- type 特定配置
    enabled     INTEGER DEFAULT 1,
    created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- 原始采集条目
CREATE TABLE raw_items (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    domain_id     TEXT NOT NULL REFERENCES domains(id),
    source_id     INTEGER NOT NULL REFERENCES sources(id),
    external_id   TEXT,                    -- 源侧唯一 ID
    url           TEXT NOT NULL,
    title         TEXT,
    author        TEXT,
    published_at  TIMESTAMP,
    fetched_at    TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    content       TEXT,                    -- extract 后的正文
    metadata_json TEXT,
    UNIQUE(source_id, external_id)
);

CREATE INDEX idx_raw_items_domain_fetched ON raw_items(domain_id, fetched_at);
CREATE INDEX idx_raw_items_url ON raw_items(url);

-- 日报期次（一天一期）
CREATE TABLE issues (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    domain_id     TEXT NOT NULL REFERENCES domains(id),
    issue_date    DATE NOT NULL,
    issue_number  INTEGER,                 -- 第几期
    title         TEXT,                    -- '2026年4月11日 AI洞察日报'
    summary       TEXT,                    -- 今日摘要
    status        TEXT DEFAULT 'draft',    -- 'draft' | 'generated' | 'published'
    source_count  INTEGER,                 -- 本期采集源数
    item_count    INTEGER,                 -- 本期条目数
    generated_at  TIMESTAMP,
    published_at  TIMESTAMP,
    UNIQUE(domain_id, issue_date)
);

-- 日报条目（按 section 分组）
CREATE TABLE issue_items (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id          INTEGER NOT NULL REFERENCES issues(id),
    section           TEXT NOT NULL,       -- 'product_update' | 'research' | 'industry' | 'opensource' | 'social'
    seq               INTEGER NOT NULL,    -- 在该 section 内的序号
    title             TEXT NOT NULL,
    body_md           TEXT NOT NULL,
    source_urls_json  TEXT,                -- 关联的源 URL（JSON 数组）
    raw_item_ids_json TEXT,                -- 关联的 raw_items.id
    created_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_issue_items_issue ON issue_items(issue_id, section, seq);

-- 行业洞察 + 对我们的启发
CREATE TABLE issue_insights (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id     INTEGER NOT NULL REFERENCES issues(id),
    industry_md  TEXT,                     -- 行业洞察（3-4 条）
    our_md       TEXT,                     -- 对我们的启发（2-3 条）
    model        TEXT,
    temperature  REAL,
    retry_count  INTEGER DEFAULT 0,
    generated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- 分发记录
CREATE TABLE deliveries (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id      INTEGER NOT NULL REFERENCES issues(id),
    channel       TEXT NOT NULL,           -- 'slack_test' | 'slack_prod' | 'feishu_doc' | 'feishu_bot'
    target        TEXT,                    -- webhook URL / doc token / chat_id
    status        TEXT NOT NULL,           -- 'sent' | 'failed' | 'skipped'
    response_json TEXT,
    sent_at       TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_deliveries_issue ON deliveries(issue_id, channel);
```

**Schema 设计原则**:
- `domain_id` 贯穿核心表，为多领域抽象留空
- 所有 type-specific 数据用 `*_json` 字段承载
- 标准 SQL，可一键平移 PG / MySQL
- 只加基础索引，不预先过度优化

## 8. Pipeline 流程

`briefing run --date 2026-04-11 --domain ai` 依次执行：

```
Step 1: ingest
  并发拉取所有 enabled sources
  规范化为 raw_items 结构
  INSERT INTO raw_items ... ON CONFLICT DO NOTHING

Step 2: dedup & filter
  URL 去重
  时间窗口过滤（默认 24h 内）
  low-quality 过滤（无标题 / 无作者 / 长度异常）

Step 3: extract (Day 2+)
  对每个 raw_item 抓源 URL 正文
  goquery 提取 title / body
  UPDATE raw_items SET content = ?

Step 4: compose
  规则分类到 7 section（关键词匹配 + LLM 兜底）
  每 section 按 published_at desc 排序
  生成 issue_items

Step 5: generate insights
  调 OpenAI 生成行业洞察 + 对我们的启发
  banned pattern 校验
  不通过则 repair，最多 3 次
  INSERT INTO issue_insights

Step 6: render
  生成 Slack Block Kit JSON
  生成飞书云文档 markdown（Day 2+）
  生成飞书群消息卡片（Day 3+）

Step 7: publish
  按 channel 顺序推送
  记录 deliveries
  失败不阻塞其他 channel

Step 8: finalize
  UPDATE issues SET status = 'published', published_at = NOW()
  exit 0
```

所有网络调用带 timeout，任何 P0 步骤失败 → `exit 1` → cron alert。

## 9. 数据源

**Day 1 必须**:

| 源 | 类型 | 实现 | 预期条目数 |
|---|---|---|---|
| GitHub Trending (daily) | API | `https://git-trending.justlikemaki.vip/topone/?since=daily` | 5-10 |
| smol.ai news RSS | RSS | `https://news.smol.ai/rss.xml` | 3-5 |

**Day 2-3 补齐**（借 upstream wrangler.toml 配置）:
- Folo list: `NEWS_AGGREGATOR_LIST_ID = 158437828119024640`
- Folo list: `HGPAPERS_LIST_ID = 158437917409783808`
- Folo list: `TWITTER_LIST_ID = 153028784690326528`
- Folo list: `REDDIT_LIST_ID = 167576006499975168`
- arxiv sanity feed
- HuggingFace daily papers
- 量子位 / 机器之心 RSS（如果有公开 feed）

**Source 接口抽象** (`internal/ingest/source.go`):
```go
type Source interface {
    Fetch(ctx context.Context) ([]*RawItem, error)
    ID() int64
    Type() string
    Name() string
}
```

新增一个源 = 实现 Source interface + 在 `sources` 表插一行配置。

## 10. LLM 生成与质量护栏

**复用 `slack-notify.js` 已 hardened 的 prompt**（port 到 Go）:
- 系统角色：资深 AI 行业分析师 + 对普通同事讲清楚
- 洞察结构：事实 → 判断 → 影响
- 条数硬约束：行业洞察 3-4 条 / 启发 2-3 条
- 每条 40-70 字
- 非大众名词必须加括号注释
- 3 次重写循环 + banned pattern 校验

**banned pattern**（继承 + 扩展市场调研吐槽点）:
- `webhook` / `cron` / `schedule` / 缓存 / 轮询（运维词）
- `"In today's rapidly evolving"` / clickbait 悬念式（LLM 套话）
- 具体时间戳 / 频道名 / 告警 / 补发（内部运维信息）

**新增 guardrails**（来自市场调研吐槽点直接落地）:
- **源头透明化**: 每期生成 "今天检查了 N 个源（GitHub Trending, smol.ai, ...），筛出 M 条"
- **Low-signal day 合法化**: raw_items < 阈值时，生成"今天没什么大事，这里是一些次要更新"的降级版本，而非硬凑
- **Fallback 降级**: 3 次 retry 全败 → 降级到纯条目列表（不带洞察），而非 exit 1

## 11. 分发

| 通道 | Day | 实现要点 |
|---|---|---|
| Slack 测试频道 | **1** | 复用 slack-notify.js 的 Block Kit，改写 Go；webhook POST with timeout |
| 飞书云文档 | 2 | 年/月/期三层 wiki 节点；markdown → docx blocks；参考 `ai-daily-news/src/feishu_docs.py` 业务逻辑改写 Go |
| 飞书群机器人 | 3 | `im/v1/messages` POST interactive 卡片；`@all` post；等待用户应用发版 |
| Slack 正式频道 | 3+ | 稳定一周后切换 |
| 静态 HTML + GH Pages | 4 | templates → 静态页 → Git push → GH Pages 免费托管 |

**分发策略**: 每个 channel 独立 Publisher，失败不阻塞其他。

## 12. 隔离与版本策略

- **中转版本 `CloudFlare-AI-Insight-Daily` 一行代码不动**
- 新目录 `/root/briefing-v3/`
- 新 GitHub repo（建议名 `briefing-v3` 或 `ai-briefing`，待用户确认）
- 服务器 cron 并行跑:
  - `15 2 * * * /root/bin/trigger-slack-notify.sh` （中转，保持）
  - `25 1 * * * /root/briefing-v3/bin/briefing run --domain ai` （新，北京 09:25）
- **切换策略**:
  - Day 1-7: 中转 + 新系统并行推送（测试频道）
  - Day 7 review: 如果新系统稳定无事故，关闭中转 cron（保留代码）
  - Day 14 review: 关闭中转 GHA workflow

## 13. 可靠性与运维

- **Timeout 全覆盖**: 所有 fetch 有 timeout（HTTP 5-10s, OpenAI 60s, webhook 15s），无 hang
- **指数退避**: 数据源 fetch 失败 retry 3 次（1s / 3s / 9s）
- **失败告警**: cron exit 非 0 → 服务器 cron 写 alert log，同时 POST Slack webhook 告警
- **日志**: `log/slog` JSON 格式 → `/root/briefing-v3/logs/briefing.log`
- **监控**: 每日最后一步 INSERT INTO deliveries 作为 heartbeat；连续 2 天无成功记录 → 手动介入
- **备份**: 每日 `cp data/briefing.db data/briefing.db.bak.YYYY-MM-DD`；保留最近 14 天

## 14. 存储/导出/联动

**主存**: `/root/briefing-v3/data/briefing.db`

**导出方式**:
- 全量 SQL: `sqlite3 briefing.db .dump > dump.sql`
- CSV: `sqlite3 briefing.db -header -csv "SELECT * FROM issues;" > issues.csv`
- JSON: `briefing export --format json --table issues`
- 物理备份: `cp briefing.db elsewhere/`（单文件）

**主业务联动**:
- Schema 全部标准 SQL，与 PG / MySQL 兼容
- 一键导入 PG: `sqlite3 briefing.db .dump | psql your_db`
- 定期 ETL: `briefing sync --target postgres --dsn ...`
- 按需查询: 主业务可读取 backup 或 attach SQLite 文件

## 15. 多领域抽象（今天只留空，不实现切换）

- `domains` 表存多领域元数据
- `config/{domain}.yaml` 每领域一份配置
- 代码层 `--domain` 参数贯穿所有子命令
- 今天只实现 `--domain ai`，但所有 schema 都带 `domain_id`

**未来新增领域**:
```bash
# 1. 新建 config/web3.yaml
# 2. INSERT INTO domains, sources
# 3. ./briefing run --domain web3
# 不改代码
```

## 16. Day 1 MVP Scope

**必须完成**:
- [ ] Go 项目骨架 + go.mod + 子命令 CLI
- [ ] SQLite schema 建立 + migration 工具
- [ ] `ingest/github_trending.go` + `ingest/smolai_rss.go`
- [ ] `compose/` 规则分类（关键词匹配到 5 section）
- [ ] `generate/prompt.go` OpenAI 洞察生成（port slack-notify.js prompt）
- [ ] `render/slack.go` Block Kit 构造
- [ ] `publish/slack.go` Slack webhook POST
- [ ] `cmd/briefing/main.go` 整合 run 命令
- [ ] 服务器 cron 条目设好（明早 09:25）
- [ ] E2E 跑通: `./briefing run --date 2026-04-11 --domain ai` 推送到测试频道

**Day 1 不做**:
- Folo API（需要 cookie，Day 2）
- 飞书任何分发
- 源链接正文抓取
- 静态 HTML 归档
- 历史数据 backfill
- 多领域切换实现

## 17. Day 2-7 增量路线

- **Day 2**: Folo 接入 + 历史 backfill + 源链接正文抓取 + 飞书云文档发布
- **Day 3**: 飞书群机器人 @所有人（等用户飞书应用发版）
- **Day 4**: 静态 HTML 归档 + GitHub Pages 部署（复制 ai.hubtoday.app 风格）
- **Day 5**: 质量优化（compose 分类准确度 + LLM prompt 微调）
- **Day 6**: Slack 正式频道切换
- **Day 7**: 全量稳定，准备下线中转版本

## 18. 成本

**零**。
- 无 Docker / Redis / Postgres / K8s
- 无云服务（除现有服务器本身）
- 无新 API key（复用 OpenAI 账号）
- 无付费 SDK
- 单二进制 + 单 .db 文件

## 19. 风险与缓解

| 风险 | 影响 | 缓解 |
|---|---|---|
| Day 1 来不及做完 | 明早仍由中转推送 | 中转不动，是兜底 |
| Folo 需要 cookie 认证 | Day 2 阻塞 | 先用 GH Trending + smol.ai，Day 2 解决 |
| 飞书应用机器人没发版 | Day 3 阻塞 | 用户侧，不影响其他通道 |
| OpenAI API 异常 | 无洞察 | 降级到纯条目列表，Slack 还能推 |
| 内容质量不如 upstream | 用户失望 | 复用已验证的 prompt + 市场调研 guardrails |
| SQLite 文件损坏 | 数据丢失 | 每日备份 + git data 分支 |

## 20. 成功标准

**Day 1 结束时**:
- `/root/briefing-v3/data/briefing.db` 存在，含 1 条 issue + N 条 issue_items + 1 条 issue_insight
- Slack 测试频道收到推送，内容质量不低于中转版本
- 服务器 cron 设好，明早 09:25 自动触发
- 整个 pipeline 3 分钟内跑完

**Day 7 结束时**:
- 多 channel 正常（Slack + 飞书云文档 + 飞书群机器人）
- 至少 5 个数据源接入
- 静态 HTML 归档可访问
- 数据库累积 7 期 issue + 完整历史
- 中转版本可下线（但保留代码）
