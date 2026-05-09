# briefing-v3 升级日志

> 本文档记录 briefing-v3 项目从 0 到 v1.0.0 的完整升级过程，以及后续 v1.1+ 的路线。
> 作用：防止多会话上下文丢失，任何新会话读完本文档即可接上进度。

---

## 0. 项目背景

### 起源
- **中转版本**：`/root/AI-Insight-Daily`（基于 upstream `justlovemaki/CloudFlare-AI-Insight-Daily` 二开）
  - 从 upstream 的 `book` 分支 raw markdown 拉日报
  - 用 OpenAI 兼容 API (gpt-5.4) 生成洞察 + 启发
  - 推 Slack 测试频道
  - **依赖 upstream，不自治**
- **升级目标**：做一个**完全自治**的 Go 系统 `briefing-v3`，自己采集、自己生成、自己分发、自己有网页载体，**复刻 https://ai.hubtoday.app/ 的效果**
- **明确要求**：让中转版本 v1.0.0 之后开始并行观察、逐步退休

### 产品定位
- **目标站点**：https://ai.hubtoday.app/ (Hugo 0.147.9 生成的静态站)
- **upstream 项目**：https://github.com/justlovemaki/CloudFlare-AI-Insight-Daily (CF Workers + Gemini + mdbook)
- **我们 fork**：https://github.com/ylzsdafei/AI-Insight-Daily (只加了 slack-notify.js)
- **briefing-v3**：`/root/briefing-v3/`（全新 Go 项目，本地 git，暂未 push）

---

## 1. v1.0.0 需求（最终冻结版）

### 硬底线（必做 + 不允许失败）
1. **自动采集 21 个数据源**（并发，失败容错）
2. **4 步 LLM Pipeline**：Rank（质量评分）→ Classify（5 section 分类）→ Compose（复刻 upstream StepOne 文案化）→ Insight（行业洞察 + 启发，我们的增量）
3. **Summary LLM**：额外 1 次调用生成 3 行今日摘要（复刻 upstream StepThree）
4. **复刻 5 section 结构**：产品与功能更新 / 前沿研究 / 行业展望与社会影响 / 开源TOP项目 / 社媒分享
5. **硬 Gate 质量门控**：条目数 / 独立源数 / 洞察字数 / banned pattern / 分类完整度
6. **Slack 双频道推送**：默认推测试频道；Gate 通过后推正式频道；Gate 失败不推正式 + 推告警到测试
7. **图文并茂**：Python PIL + Noto Sans CJK 生成 1200×630 报纸头版风格封面大图
8. **网页版载体**：本地 `docs/YYYY-MM-DD.html` + `docs/index.html` 历史列表（我方案前期遗漏，用户强调后补上）
9. **SQLite 存储**：7 张表（domains / sources / raw_items / issues / issue_items / issue_insights / deliveries）
10. **服务器 cron 自动触发**：每天北京 09:25
11. **失败告警**：exit 1 + Slack 告警到测试频道
12. **零降级**：任何阶段失败硬退，绝不静默简化
13. **中转版本一行不动**：隔离目录 + git tag + crontab backup

### v1.0.0 不做（推迟）
- 飞书三件套（云文档 / 群机器人 / 多维表格）→ v1.1
- GitHub Pages 公网部署 → v1.1
- 国内站点直连（机器之心 / 量子位）→ 服务器物理不通，需 CF Worker 代理 → v1.1
- X/Twitter 原生短推文 → 用 AI 大 V 博客代替（深度更好）
- 静态 Hugo 站点迁移 → v1.2
- 播客音频（TTS）→ v1.3
- 视频生成 → v1.4
- 多领域切换 → 代码留接口 / 不实现 → v2.0
- 每条目配图 → 只有 1 张封面图 → v1.2+

---

## 2. 架构决策（已锁定）

### 技术栈
| 维度 | 选择 | 理由 |
|---|---|---|
| 语言 | Go 1.25.0 | 跟主业务栈一致，单二进制部署 |
| 存储 | SQLite (modernc.org/sqlite 纯 Go) | 零运维，无需 Docker |
| 配置 | YAML + env var | `config/ai.yaml` + `config/secrets.env` |
| LLM | OpenAI 兼容 API (gpt-5.4) | 免费复用，上游同款模型 |
| RSS 解析 | mmcdole/gofeed | 成熟稳定 |
| HTML 抓取 | PuerkitoBio/goquery | jQuery 风格 |
| 图片生成 | Python PIL + Noto Sans CJK | 服务器已装，Go 用 exec 调用 |
| HTML 模板 | stdlib html/template | 零依赖 |
| 调度 | 服务器 crontab | 避开 GHA schedule 丢失问题 |

### 数据流（锁定）
```
21 源并发采集 (20s 超时/源)
  ↓
InsertRawItems (SQLite ON CONFLICT)
  ↓
分配临时 ID (in-memory, 因 InsertRawItems 不回写 AUTOINCREMENT id)
  ↓
时间窗口过滤 (48h, 不足扩展到 72h)
  ↓
Step 1 Rank LLM (批量评分, MinScore 6.0, TopN 30)
  ↓
Step 2 Classify LLM (分到 5 section, 规则 fallback)
  ↓
Step 3 Compose LLM (per-section Summarize 复刻 upstream StepOne prompt)
  ↓
Step 4 Generate Insight LLM (industry + takeaways, 5 次阶梯 retry)
  ↓
Summary LLM (100 字 3 行, 复刻 upstream StepThree)
  ↓
Render (markdown + Slack Block Kit + HTML 页面)
  ↓
Python PIL 生成封面图
  ↓
硬 Gate (条目数 / 洞察字数 / banned pattern / 源多样性)
  ↓
Publish 测试频道 (无条件)
  ↓
Gate pass → Publish 正式频道 (target=auto)
Gate fail → 告警到测试频道
```

---

## 3. 数据源清单（21 个，全部验证 200）

### 英文新闻 (5)
- smol.ai news — `https://news.smol.ai/rss.xml`
- The Decoder — `https://the-decoder.com/feed/`
- TechCrunch AI — `https://techcrunch.com/category/artificial-intelligence/feed/`
- OpenAI News — `https://openai.com/news/rss.xml`
- Anthropic News — `https://www.anthropic.com/news` (HTML 抓)

### 中文内容 (4)
- 宝玉的分享 — `https://baoyu.io/feed.xml`
- 阮一峰的网络日志 — `https://www.ruanyifeng.com/blog/atom.xml`
- Google News RSS: 人工智能 — 搜索 query URL-encoded
- Google News RSS: 大语言模型 — 同上

### AI 大 V 博客 (5)
- Ethan Mollick (oneusefulthing) — `https://www.oneusefulthing.org/feed`
- Simon Willison — `https://simonwillison.net/atom/everything/`
- Jack Clark (Import AI) — `https://jack-clark.net/feed/`
- Sebastian Raschka — `https://magazine.sebastianraschka.com/feed`
- Lilian Weng (OpenAI) — `https://lilianweng.github.io/index.xml`

### 论文 (3)
- arXiv cs.AI — `https://rss.arxiv.org/rss/cs.AI`
- arXiv cs.LG — `https://rss.arxiv.org/rss/cs.LG`
- HuggingFace Daily Papers — `https://huggingface.co/papers` (HTML 抓)

### 开源项目 (1)
- GitHub Trending (via ossinsight.io) — `https://api.ossinsight.io/v1/trends/repos`

### 社区 (3)
- HackerNews Top — `https://hacker-news.firebaseio.com/v0/topstories.json`
- Reddit r/MachineLearning — JSON API
- Reddit r/LocalLLaMA — JSON API (+UA)

---

## 4. 代码结构

```
/root/briefing-v3/
├── bin/briefing                           # 编译产物
├── cmd/briefing/
│   ├── main.go                            # CLI 入口（migrate/seed/run/promote/status）
│   └── run.go                             # 主 pipeline 逻辑
├── internal/
│   ├── config/config.go                   # YAML + env loader
│   ├── store/
│   │   ├── types.go                       # Domain/Source/RawItem/Issue/IssueItem/IssueInsight/Delivery
│   │   ├── store.go                       # Store interface
│   │   ├── sqlite.go                      # modernc.org/sqlite 实现
│   │   └── migrations/001_initial.sql     # embed schema
│   ├── ingest/
│   │   ├── source.go                      # Source interface + registry
│   │   ├── rss.go                         # 通用 RSS (Wave 1)
│   │   ├── github_trending.go             # Wave 1 (旧代理，已替换)
│   │   ├── folo.go                        # Wave 1 stub（不用）
│   │   ├── ossinsight.go                  # ← 新：GitHub Trending API
│   │   ├── hn.go                          # ← 新：HackerNews API
│   │   ├── reddit.go                      # ← 新：Reddit JSON
│   │   ├── hf_papers.go                   # ← 新：HuggingFace HTML
│   │   ├── anthropic.go                   # ← 新：Anthropic HTML
│   │   └── gnews.go                       # ← 新：Google News RSS
│   ├── rank/rank.go                       # Step 1 LLM 质量评分
│   ├── classify/classify.go               # Step 2 LLM 5-section 分类
│   ├── compose/compose.go                 # 按 section 组装 IssueItem
│   ├── generate/
│   │   ├── generator.go                   # Generator interface
│   │   ├── openai.go                      # OpenAI 客户端
│   │   ├── prompts.go                     # 复刻 slack-notify.js 的洞察 prompt
│   │   ├── validator.go                   # bannedPatterns + ValidateInsight
│   │   └── summarize.go                   # Summarizer interface + Summarize 方法
│   ├── render/
│   │   ├── markdown.go                    # 完整 markdown 渲染
│   │   ├── slack.go                       # Slack Block Kit
│   │   └── html.go                        # HTML 页面 + index.html
│   ├── gate/quality.go                    # 硬质量 Gate
│   ├── image/headline.go                  # Python exec wrapper
│   └── publish/
│       ├── publisher.go                   # Publisher interface + RenderedIssue
│       ├── slack.go                       # Wave 1 slack publisher（未用，直接 post）
│       ├── feishu_doc.go                  # v1.1 预留
│       └── feishu_bot.go                  # v1.1 预留
├── scripts/
│   ├── gen_headline.py                    # Python PIL 报纸头版风格封面
│   └── cron.sh                            # cron 入口 + 失败告警
├── config/
│   ├── ai.yaml                            # 领域配置 + 21 sources
│   ├── secrets.env                        # (不入 git) 真实 secrets
│   └── secrets.env.example                # 模板
├── data/
│   ├── briefing.db                        # SQLite 主存
│   └── images/                            # 封面 PNG
├── daily/                                 # 每日 markdown 存档
│   └── YYYY-MM-DD.md
├── docs/                                  # 网页载体
│   ├── index.html                         # 历史列表
│   ├── YYYY-MM-DD.html                    # 单期完整页
│   └── specs/
│       ├── 2026-04-10-briefing-v3-design.md
│       └── UPGRADE_LOG.md                 # ← 本文档
├── logs/
├── go.mod / go.sum
└── README.md
```

---

## 5. 里程碑进度（截至 2026-04-10）

| 里程碑 | 状态 | 时间 |
|---|---|---|
| M0: Wave 0 骨架 + git tag + crontab backup | ✅ | 2026-04-10 早 |
| M1: Wave 1 4 个 teammate 代码 | ✅ | Teammate 并行完成 |
| M2: generate/openai.go 补齐 | ✅ | 主线补 |
| M3a: Wave 2 Agent Teams 并行写 4 份代码 | ✅ | ingest/rank/classify/compose/render/gate/image/config/CLI |
| M3b: 主线 run.go wire pipeline | ✅ | 515 行 |
| M3c: HTML 页面载体补救 | ✅ | render/html.go 530 行 |
| M3d: rank bug 修复（临时 ID 方案）| ✅ | 补 run.go |
| M4: dry-run #1 | ✅ 暴露 rank bug | 14:24-14:33 |
| M4: dry-run #2 | 🔄 进行中 | 验证 rank fix |
| M5: 真实 run --target test | ⏳ | 等 M4 |
| M6: 完整全链路（测试频道 + 浏览器打开 HTML） | ⏳ | |
| M7: crontab 部署 + 首个 commit | ⏳ | |
| M8: 明早 09:25 自动触发验证 | ⏳ | 明早 |
| M9: v1.0.0 稳定 3+ 天 → 考虑中转退休 | ⏳ | Day 3+ |

---

## 6. v1.0.0 硬约束（已锁定，任何后续 PR 必须遵守）

1. **零降级**：pipeline 任何阶段失败 → exit 1 + 告警 + 等人工
2. **不允许推送失败的早报**：Gate 不通过绝不触碰正式频道
3. **洞察 + 启发非空**：两个必须有内容，否则硬失败
4. **Slack 测试频道优先**：默认 `--target test`，正式频道只在 `--target auto/prod` 且 Gate pass 时推送
5. **中转版本不动**：git tag `stable-2026-04-10-pre-briefing-v3` 已推 remote，一键回滚
6. **测试期间推送只到测试频道**：手动 run 默认 test，cron 才用 auto
7. **图文并茂**：每期一张报纸头版风格封面大图
8. **网页版载体**：每期一个 HTML 页面 + 首页历史列表（本地 docs/）

---

## 7. 已知 bug + workaround

### Bug 1: InsertRawItems 不回写 AUTOINCREMENT id
- **症状**: ingest 后 rawItems[].ID 全为 0，rank 的 byID map 退化
- **Workaround**: 在 pipeline 里 InsertRawItems 之后手动分配临时 int 序号 ID
- **永久修复**: v1.0.1 给 store.Store.InsertRawItems 加 RETURNING id 回写

### Bug 2: ossinsight 无 pushed_at
- **症状**: trending 项目都用 fetch 时间作为 PublishedAt，时间窗过滤失效
- **Workaround**: 接受（trending 本身就是实时快照，48h 窗口内都保留）

### Bug 3: HF Papers HTML 结构依赖 text-walk
- **症状**: DOM 变动 upvotes 抓不到
- **Workaround**: 降级时只保留 title+url

### Bug 4: Anthropic 抓不到 publishedAt
- **症状**: 索引页 HTML 无日期信息
- **Workaround**: fetch 时间作为 PublishedAt（48h 内都保留）

### Bug 5: Google News 中文搜索 URL 必须 encode
- **已修**: gnews adapter 用 url.QueryEscape

### Bug 6: baoyu.io feed 路径必须是 `/feed.xml`
- **已修**: config 里写正确路径（之前试 `/atom.xml` / `/rss.xml` / `/index.xml` 都 404）

---

## 8. 后续升级路线（v1.1 ~ v2.0）

### v1.0.1 — 质量调优（Day 2-3）
- rank prompt 精调（解决"全部打低分"问题）
- compose summary 质量提升
- HTML 视觉微调（更接近 ai.hubtoday.app Hugo 站点风格）
- 永久修 InsertRawItems 回写 ID

### v1.1 — 飞书三件套（Day 4-7）
- 飞书云文档自动发布（wiki 三层节点：年 / 月 / 单期）
- 飞书群机器人 @all 推送
- 飞书多维表格同步（日报期次 / 条目 / 洞察 / 反馈 / 通知）
- 复用 ai-daily-news 老项目的 `user_access_token` 方案

### v1.2 — 静态站 + GitHub Pages（Day 8-14）
- 配置 GitHub Pages 部署 `docs/`
- Slack 按钮 URL 切公网 URL
- 或者：迁移到 Hugo 完全复刻 ai.hubtoday.app 的主题
- 复刻 avif 配图（每条目一张 AI 生成图）

### v1.3 — 播客（Day 15-21）
- 复刻 upstream 的 `summarizationPromptPodcastFormatting.js` + `getSystemPromptShortPodcastFormatting`
- 接 TTS API 生成音频
- 推送到飞书云文档或音频托管平台

### v1.4 — 视频（Day 22+）
- 文字 + 封面图 → 简单短视频
- 接字幕 + BGM

### v2.0 — 多领域（长期）
- 激活 `domain_id` 切换能力
- 新建 `config/finance.yaml` `config/web3.yaml` 等
- 一套 pipeline 跑多个领域

### 中转退休路线
- v1.0.0 稳定跑 3 天 → 开始并行对比质量
- 连续 7 天 ≥ upstream 质量 → 下线中转 cron
- 保留中转代码 14 天 → 最终删除

---

## 9. 运维命令 cheatsheet

```bash
# 初始化
cd /root/briefing-v3
./bin/briefing migrate
./bin/briefing seed

# 手动运行（仅测试频道）
./bin/briefing run --date 2026-04-10 --dry-run           # 不推送
./bin/briefing run --date 2026-04-10 --target test       # 推测试

# cron 模式（测试 + gate + 正式）
./bin/briefing run --date 2026-04-11 --target auto

# 补推正式频道
./bin/briefing promote --date 2026-04-11

# 查看状态
./bin/briefing status --date 2026-04-11
```

### 回滚中转
```bash
cd /root/CloudFlare-AI-Insight-Daily
git reset --hard stable-2026-04-10-pre-briefing-v3
crontab /root/crontab.backup.2026-04-10-pre-briefing-v3
```

### 服务器环境
- Go: `/usr/local/go/bin/go` (1.25.0)
- Python: `/usr/bin/python3` + PIL 10.4.0
- 字体: `/usr/share/fonts/opentype/noto/NotoSansCJK-Bold.ttc`
- OpenAI: `http://64.186.239.99:8080` (gpt-5.4)
- Slack 测试: `hooks.slack.com/services/REPLACE/WITH/YOUR_TEST_WEBHOOK` (实际值在 `config/secrets.env`)
- Slack 正式: `hooks.slack.com/services/REPLACE/WITH/YOUR_PROD_WEBHOOK` (实际值在 `config/secrets.env`)

---

## 10. 本次升级的反思（给未来的自己）

### 架构师的教训
1. **"复刻" 的完整定义必须包括载体**。ai.hubtoday.app 是网站 = 载体，不只是数据流。我前期只想数据流，漏了网页版。用户提醒 3 次才补。**下次接到"复刻"需求，第一问：终端用户在哪里看内容？**
2. **盲目相信 teammate 产出的代码**。4 个 teammate 并行写了 2400+ 行，我信他们都 PASS 就没 E2E 验证。rank bug 要到跑 dry-run 才暴露。**下次写完 wire 立即 dry-run，不要等所有代码"备齐"才跑**。
3. **过度发散的调研浪费时间**。我在中文源 / RSSHub / Folo cookie 上绕了几轮才定下来。**架构师的职责是快速决策 + 承担后果**，不是穷举所有选项。
4. **不要把 upstream 的 URL 当默认**。我把 slack-notify.js 的 `reportURL = ai.hubtoday.app/...` 直接 port 过来，等于还在依赖 upstream。**自治 = 所有外部 URL 都要审查**。

### 用户的有效 feedback
1. "一切从简，先实现核心功能" — 每次我发散时他拉回
2. "不能随便放弃" — 我说砍国内源时他让我想办法（Google News / AI 大 V 博客补）
3. "你不会在耍我吧" — 严厉但及时，让我停下自辩立即展示证据
4. "先看清楚 ai.hubtoday.app 再模仿" — 让我用 curl 实地看页面结构

---

**本文档最后更新**: 2026-04-10，v1.0.0 里程碑 M4 (dry-run #2 验证中)
**下一步**: 等 dry-run #2 → 真实 --target test 全链路 → crontab 部署 → 等明早 09:25 自动触发
