# 信息源优化计划

> **Status**: Pending approval to start
> **Author**: Claude (Opus 4.6, 2026-04-14 夜 session)
> **Trigger**: 2026-04-14 v1.0.1 critical fix 结束后, 用户要求客观分析现有信息源并列优化计划
> **Scope**: `config/ai.yaml` + `internal/ingest/*` + rank/classify 的 signal quality
> **文档结构**: 每个优化事项包含 4 个维度 —— **优化事项 / 技术方案 / 技术验证 / 影响范围评估**
> **删除条件**: Phase 1-4 全部完成后可删除 (Phase 5 作为长期 backlog 另存档)

---

## 0. 基线状态 (2026-04-14 实测)

### 0.1 现有 21 个信息源分布

| 类别 | 数量 | 今日抓取量 | 占比 |
|---|---|---|---|
| 英文 AI 新闻 | 5 (smol.ai / The Decoder / TechCrunch / OpenAI News / Anthropic News) | ~1500 | 45% |
| 中文内容 | 4 (宝玉 / 阮一峰 / Google News 人工智能 / Google News 大语言模型) | ~153 | 5% |
| 研究者博客 | 5 (Mollick / Willison / J.Clark / Raschka / L.Weng) | ~131 | 4% |
| 学术论文 | 3 (arXiv cs.AI / cs.LG / HF Papers) | 1272 | 38% |
| 开源项目 | 1 (GitHub Trending via ossinsight) | 100 | 3% |
| 社区讨论 | 3 (HN Top / r/ML / r/LocalLLaMA) | 76 | 2% |
| **合计** | **21** | **~3320** | **100%** |

### 0.2 三个核心异常

| # | 观察 | 证据 |
|---|---|---|
| A | **OpenAI News 单源 935 items (28%)** — 异常多 | 单个厂商 RSS 不应 900+/天，大概率 adapter 没做日期预过滤返回全量历史 |
| B | **Google News "大语言模型" 返回 0** | 两次实测均 `[WARN] ingest: empty feed for query "大语言模型"` |
| C | **arXiv 1248 items (38%)** | rank LLM 要处理 1500+ items → 耗时 11-13 分钟，成本大头 |

### 0.3 结构性缺口

- 中文技术媒体薄（2 博客 + 2 搜索）
- 商业/融资 0 覆盖
- 政策/监管 0 覆盖
- 主流 AI 厂商官博不全（有 OpenAI / Anthropic，缺 DeepMind / Meta / Microsoft / Mistral / Nvidia）

---

## 1. 核心设计原则

1. **目标导向**: 用户是"全公司包括非技术岗"，覆盖应立体（技术 + 商业 + 政策 + 应用）
2. **ROI 优先**: 先修 bug 再扩量
3. **Fail-soft**: 单源失败不阻塞 (已实现)，但长期失败要告警
4. **Signal quality > quantity**: 信号强的源 > 更多的源
5. **本土化**: 中文源不足是已知缺口, 但服务器在境外无法直连国内 CDN, 推到 Phase 5.3 自建 RSShub 时一起做

---

## Phase 1 — 修现有 bug (1-2 天, ROI 最高)

### 1.1 OpenAI News adapter 返回量过大

#### 优化事项
`https://openai.com/news/rss.xml` 今日返回 935 items，占全量抓取 28%，是 rank 阶段 LLM 处理量大头、耗时 13 分钟的主因。

#### 技术方案
**诊断** (必须先做):
```bash
curl -sS https://openai.com/news/rss.xml | xmllint --format - | grep -E "<pubDate>|<title>" | head -40
```
看返回的 `pubDate` 分布。如果跨度远超 24h → 确认 adapter 未做日期过滤。

**修复** (基于诊断结果，二选一)：

**方案 A (推荐，若 feed 本身全量返回)**: 在 `internal/ingest/rss.go` 通用 adapter 的 `Fetch()` 里，在 `feed.Items` 循环中加日期过滤：
```go
// internal/ingest/rss.go
cutoff := time.Now().Add(-time.Duration(ingestWindowHours) * time.Hour)
for _, fi := range feed.Items {
    if !fi.PublishedParsed.IsZero() && fi.PublishedParsed.Before(cutoff) {
        continue  // 跳过超窗口的老条目
    }
    // ... 现有逻辑
}
```

`ingestWindowHours` 从 `config.WindowConfig.ExtendedHours` 读 (默认 48h)，留给后续 filter 阶段再细筛到 24h。

**方案 B (若是 adapter 解析 bug)**: 具体修 parse 逻辑。

**配置改动**: 可能需要给 source 加 `max_age_hours: 48` 字段，通用 adapter 识别。

#### 技术验证
- **单测**: 给 rss.go 加 table-driven test：给定 feed.Items 含 (已 1h 前 / 10h 前 / 30h 前 / 100h 前) 4 条，确认 48h 窗口只保留前 3 条
- **集成测**: 跑 `briefing run --dry-run`，观察 `[ingest] OpenAI News → X items` 的 X 值
- **成功标准**: OpenAI News 从 935 → 10-30 items/天 (合理范围)
- **Rank 验证**: rank 数据量 1500 → 800，rank 耗时 13min → 6-8min

#### 影响范围
| 维度 | 影响 |
|---|---|
| **代码文件** | `internal/ingest/rss.go` 主改（100% 覆盖）；可能 `internal/config/config.go` 加字段 |
| **数据库** | 无影响（纯 ingest 改动） |
| **其他 source** | 所有用 `type: rss` 的源受益（因 rss.go 共享），包括 OpenAI News / Anthropic News(HTML) / TechCrunch / smol.ai / The Decoder / 阮一峰 / 宝玉 / blog-emollick / 等 15+ 个源 |
| **风险** | 如果 feed 没有 pubDate 字段，filter 会漏掉所有 item → 需加 fallback: pubDate 缺失 → 保留，交给后续 filter stage（它有 title-date fallback） |
| **回滚** | 如果改坏，`git revert` 单个文件即可，无状态需要清理 |

---

### 1.2 Google News 空 feed 处理 + fallback 查询词

#### 优化事项
"大语言模型" 查询今天返回 0 items，两次实测均为 empty。现在 adapter 直接返回空数组，调用者（rank/classify）看不到信号。

#### 技术方案
**1. fallback 查询词列表**：
```yaml
# config/ai.yaml
- id: gnews-llm-cn
  type: google_news
  name: "Google News: 大语言模型"
  queries:  # 新增，代替单个 query
    - "大语言模型"
    - "LLM 大模型"
    - "生成式AI"
    - "ChatGPT OR Claude OR Gemini"
```

**2. adapter 支持**：
```go
// internal/ingest/gnews.go
type GNewsConfig struct {
    Queries  []string
    // ...
}

func (s *gnewsSource) Fetch(ctx context.Context) ([]*store.RawItem, error) {
    for _, q := range s.cfg.Queries {
        items, err := s.fetchOneQuery(ctx, q)
        if err != nil { continue }
        if len(items) > 0 { return items, nil }
    }
    return nil, errors.New("gnews: all fallback queries empty")
}
```

**3. 长期空源自动禁用**：结合 1.3 的 source_health 表，连续 N 天失败标记 `auto_disabled`。

#### 技术验证
- **单测**: mock fetchOneQuery 返回 (空, 空, 10 items) → 验证最后返回 10 items
- **集成测**: 修改 gnews config 让第一个 query 故意错误 → 跑 `briefing run --dry-run`，看日志 `[ingest] Google News → X items` 应该用 fallback 拿到数据
- **回归测**: 现有单 query 配置不破坏（backward compat）

#### 影响范围
| 维度 | 影响 |
|---|---|
| **代码文件** | `internal/ingest/gnews.go` （adapter 改动）；`internal/config/config.go` (SourceConfig 加 Queries 字段) |
| **配置文件** | `config/ai.yaml` gnews-* 2 个 source 改 `query` → `queries` 数组 |
| **向后兼容** | YAML 旧单 query 字段与新 queries 数组共存（`query` 转成单元素 queries） |
| **风险** | fallback 越多，单次 ingest 时间越长（N 个查询 × 每次 2-3 秒）；但只在前面空时才 fallback，正常情况 latency 不变 |
| **回滚** | 改 YAML 回 `query` + revert adapter code |

---

### 1.3 per-source 健康监控表

#### 优化事项
现在单源静默失败（如 Anthropic HTML scrape 被反爬）只在 log 里，没累积。某个 source 连续 3 天失败没有告警，问题被淹没。

#### 技术方案
**新表**:
```sql
-- migration 006_source_health.sql
CREATE TABLE source_health (
    source_id            INTEGER PRIMARY KEY REFERENCES sources(id),
    last_success_at      TIMESTAMP,
    last_error_at        TIMESTAMP,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    last_error_text      TEXT,
    last_item_count      INTEGER NOT NULL DEFAULT 0,
    auto_disabled        BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at           TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
```

**Store 接口新增**:
```go
UpsertSourceHealth(ctx, sourceID, success bool, errorText string, itemCount int) error
ListSourceHealth(ctx, domainID) ([]*SourceHealth, error)
```

**ingest 流程接入** (`cmd/briefing/run.go` 的 ingestAll 之后):
```go
for _, r := range results {
    if r.err != nil {
        _ = s.UpsertSourceHealth(ctx, r.sourceID, false, r.err.Error(), 0)
    } else {
        _ = s.UpsertSourceHealth(ctx, r.sourceID, true, "", len(r.items))
    }
}
```

**`briefing status --sources` 新子命令**:
```
SOURCE                           OK  LAST_SUCCESS       ITEMS  CONSEC_FAIL
smol.ai news                     ✓   2026-04-14 05:58   610    0
Google News: 大语言模型           ✗   2026-04-12 08:05   0      3  ← 告警
Anthropic News                   ✓   2026-04-14 05:58   13     0
```

**长期失败告警**: orchestrator.sh pre-flight 加一条检查：任一 source `consecutive_failures >= 5` → POST 告警到 SLACK_TEST_WEBHOOK。

#### 技术验证
- **单测**: 写 source_health 插入/更新 3 种场景（新行 / 成功更新 / 失败更新）
- **集成测**: 故意让 2 个 source 失败（改 URL 为 404）→ 跑 `briefing run --dry-run` → 查 `source_health` 表确认记录正确
- **`briefing status --sources`**: 手动 run 确认输出格式可读

#### 影响范围
| 维度 | 影响 |
|---|---|
| **数据库** | 新建 `source_health` 表 (~100 行数据规模)；需新 migration 006 |
| **代码文件** | `internal/store/{types.go, store.go, sqlite.go}` 加接口 + 实现；`cmd/briefing/run.go` ingest 后加写入；`cmd/briefing/status.go` 加 subcommand；`internal/store/migrations/006_*.sql` 新文件 |
| **现有功能** | 零破坏（只新增表，不改现有逻辑） |
| **性能** | 每次 run 多 21 个 UPSERT，sqlite <5ms，可忽略 |
| **回滚** | 删除表 + revert code；可通过保留 migration 006 但不调用 UpsertSourceHealth 回滚 |

---

## Phase 2 — 中文技术媒体扩充 (已废弃 / SKIPPED, 2026-04-14)

**决议**: 跳过, 不实施。

**原因 (实测 2026-04-14)**:
- 服务器位置在境外 (LLM endpoint 64.186.239.99 同地)
- 国内 AI 媒体 (qbitai / jiqizhixin / 36kr / infoq.cn / paperweekly) 全部依托国内 CDN (腾讯云 / 阿里云 / 36kr 自建), 海外 IP 一律 connection timeout
- 公开 RSShub 实例 (rsshub.app) 已全部 HTTP 403, 不能作为代理
- 唯一可行路径需自建 RSShub on 国内 VPS, 工程量超出 Phase 2 预算

**重新归位**:
- 国内媒体覆盖**整体推到 Phase 5.3** (国内公众号爬取), 等有国内 VPS / 自建 RSShub 时一起做
- 中文用户友好度由现有 Google News 中文 query (Phase 1.2 已扩 4 个 fallback 词) + 宝玉 + 阮一峰 维持

---

## Phase 3 — 结构性覆盖扩充 (3-5 天)

### 3.1 AI 厂商官博补齐

#### 优化事项
现有只 OpenAI News + Anthropic News，缺 DeepMind / Meta AI / Microsoft Research / Mistral / NVIDIA。这些都是研究首发/产品首发的源头，不应缺位。

#### 技术方案

加入 5 个 RSS 源（都是 `type: rss`，零代码改动）：

| # | Source | URL | category | priority |
|---|---|---|---|---|
| 1 | DeepMind blog | `https://deepmind.google/discover/blog/rss.xml` | news | 10 |
| 2 | Meta AI Research | `https://ai.meta.com/blog/rss` | news | 9 |
| 3 | Microsoft Research AI | `https://www.microsoft.com/en-us/research/blog/feed` | news | 8 |
| 4 | Mistral blog | `https://mistral.ai/news/rss` | news | 9 |
| 5 | NVIDIA AI blog | `https://blogs.nvidia.com/blog/category/deep-learning/feed/` | news | 8 |

#### 技术验证 (4 步骤通用模板)
1. **URL 存活性**: `curl -sLS --max-time 10 -o /dev/null -w "HTTP=%{http_code}" $url`, 期望 HTTP 200
2. **RSS 格式**: `curl -sL $url | xmllint --format - | head -30`, 确认 `<item>` 或 `<entry>` > 5
3. **集成测**: 改 `config/ai.yaml` 加新源 → `briefing seed` → `briefing run --dry-run` → 确认 `[ingest] {name} → X items` 的 X > 5
4. **内容质量抽样**: 手工 review 5 条新源 item, 排除广告/noise

特别注意：
- Microsoft Research blog 可能混杂非 AI 内容，需要 classify LLM 兜底过滤
- Mistral / NVIDIA URL 可能有变化，需验证
- DeepMind blog 质量最高 (priority=10)

#### 影响范围
| 维度 | 影响 |
|---|---|
| **代码文件** | 零改动 |
| **配置文件** | `config/ai.yaml` 追加 5 条 |
| **数据库** | sources 表 +5 |
| **内容分布** | product_update / research section 内容更立体（之前 OpenAI/Anthropic 寡头） |
| **rank 权重** | priority=10 的 DeepMind 会压过 priority=7 的 arxiv 随机 paper |
| **风险** | 低，全是大公司官博 |

---

### 3.2 商业/融资覆盖

#### 优化事项
商业视角现在 0 覆盖。Platformer / Big Technology / SemiAnalysis 是行业前三档的 Substack，免费 RSS 可读。

#### 技术方案

| # | Source | URL | category | priority |
|---|---|---|---|---|
| 1 | Platformer (Casey Newton) | `https://www.platformer.news/feed` | blog | 9 |
| 2 | Big Technology (Alex Kantrowitz) | `https://www.bigtechnology.com/feed` | blog | 8 |
| 3 | SemiAnalysis | `https://www.semianalysis.com/feed` | blog | 9 |
| 4 | Axios AI | `https://api.axios.com/feed` (需筛 AI 标签) | news | 7 |

**Axios 特殊处理**: 通用 feed 含所有 Axios 新闻，需要在 ingest 层或 rank 阶段筛 AI 相关：
- 方案 A: `internal/ingest/axios.go` 新 adapter（复杂）
- 方案 B: 用现有 rss.go 抓取，靠 classify LLM 筛（简单但可能浪费 rank quota）
- **推荐方案 B** 作为起点，Phase 4 再优化

#### 技术验证
同 Phase 3.1 的 4 步骤模板（URL 存活 / RSS 格式 / 集成测 / 内容抽样）

内容抽样特别关注：
- Platformer 每日 1-3 篇深度 → 保留率高
- Big Technology 每日 1-2 篇
- SemiAnalysis 频率低 (2-3 篇/周)，但信号强
- Axios 量多需 LLM classify 过滤

#### 影响范围
| 维度 | 影响 |
|---|---|
| **代码文件** | 零改动 |
| **配置文件** | `config/ai.yaml` 追加 4 条 |
| **section 分布** | industry section 信息密度提升（现在 industry 常常内容单薄） |
| **rank 调度** | blog 类被分类为 social，可能稀释 priority；需要 classify 规则检查 |
| **风险** | Substack 可能有 rate limit，但一次请求 < 10 req 问题不大 |

---

### 3.3 政策/监管覆盖

#### 优化事项
AI 行业洞察板块现在容易只谈产品不谈环境。政策监管 (EU AI Act, 中国 AI 治理) 是重要叙事。

#### 技术方案

**RSS 可行的源**：
| # | Source | URL | category | priority |
|---|---|---|---|---|
| 1 | Politico Tech | `https://www.politico.com/rss/politicopicks.xml` | news | 7 |
| 2 | EU AI Act tracker | 需调研 (aiact.substack.com / euaiact.com 等) | news | 8 |
| 3 | NIST AI releases | `https://www.nist.gov/news-events/news/ai` (如有 RSS) | news | 7 |

**无 RSS 但战略级**：
- 国家网信办公告: 需爬 → 暂缓 Phase 5
- 工信部 AI 政策: 需爬 → 暂缓
- 美国 AI Executive Order 更新: 走 Politico 覆盖

#### 技术验证
先做 URL 存活调研，挑 1-2 个信号强的加入。

特殊步骤：
1. 跑 2 周累积数据
2. 人工看 policy 相关 item 是否进了 industry section
3. 如果 classify 失败（被归到其他 section），需加 classify prompt 里的 industry 判断规则：提政策/监管/诉讼 → industry

#### 影响范围
| 维度 | 影响 |
|---|---|
| **代码文件** | 可能需改 `internal/classify/classify.go` 的 industry 判断规则（加政策关键词） |
| **配置文件** | `config/ai.yaml` 追加 1-3 条 |
| **内容 section 分布** | industry section 权重上升 |
| **风险** | 政策类英文内容对非技术中文读者可能门槛高，需 insight prompt 强调"对我们的影响"而非"原文翻译" |

---

## Phase 4 — 信号质量升级 (5-7 天)

### 4.1 Source priority 接入 rank 权重

#### 优化事项
现在 `config/ai.yaml` 每个 source 有 `priority: 1-10` 但 `internal/rank/rank.go` 完全不用它。DeepMind (priority=10) 和无名博客 (priority=5) 在 rank 阶段同等竞争，浪费 priority 配置。

#### 技术方案

在 `rank.go` 的 Rank 函数内，LLM 返回 quality score 后，乘以 source priority 权重：
```go
// internal/rank/rank.go
for i, item := range ranked {
    sourcePriority := sourcePrioritiesMap[item.SourceID]  // 0..10
    // 权重函数: priority=10 → ×1.5, priority=5 → ×1.0, priority=1 → ×0.6
    weight := 0.5 + float64(sourcePriority)/10.0
    ranked[i].FinalScore = ranked[i].LLMScore * weight
}
sort.Slice(ranked, func(i, j int) bool {
    return ranked[i].FinalScore > ranked[j].FinalScore
})
```

**新数据流**: rank 需要接收 `sourcePrioritiesMap map[int64]int` 参数（类似现有 sourceCategoriesMap）。

**配置灵活性**: weight 函数的 0.5+N/10 系数未来可做 config 可调。

#### 技术验证
- **单测**: 给定 30 items，其中 priority=10 的 LLM score=0.7，priority=5 的 LLM score=0.9，预期 priority=10 final score > priority=5 final score (前者 0.7×1.5=1.05, 后者 0.9×1.0=0.9)
- **集成测**: 现有 top 30 分布 vs 改后 top 30 分布对比，DeepMind/OpenAI 等高权重源占比应上升
- **A/B 对比**: 跑一周生成两版 rank 结果（开启 vs 关闭 priority 权重），人工 review

#### 影响范围
| 维度 | 影响 |
|---|---|
| **代码文件** | `internal/rank/rank.go` 主改；`cmd/briefing/run.go` 传参；可能 `internal/store/sqlite.go` 加 helper |
| **接口变化** | rank.Rank() 签名增加 sourcePriorities 参数，向后不兼容但调用者只有 run.go 一处 |
| **rank 结果** | 同一批 item 的 top 30 分布会变化，内容质量应该提升 |
| **风险** | 可能过度惩罚 arxiv 论文（priority=7）导致好论文被挤出 top 30；可监控 research section 内容质量 |
| **回滚** | 权重函数改回 1.0 即可（保留参数传递，不影响逻辑） |

---

### 4.2 多源交叉验证 (signal strength)

#### 优化事项
同一事件被多个源同时报道 = 行业公认重大事件，信号强度高。当前系统看不到这个信号，所以"OpenAI 官博独家" 和 "smol.ai + TechCrunch + The Decoder 同时报道同一事件" 在 rank 里权重一样。

#### 技术方案

**Step 1 — 计算 signal_strength**:
在 filter/dedup 之后、rank 之前，添加一步：
```go
// new: internal/ingest/signal_strength.go
func CalculateSignalStrength(items []*store.RawItem) {
    groups := groupBySimilarTitle(items)  // fuzzy match
    for _, group := range groups {
        distinctHosts := uniqueHosts(group)
        strength := float64(len(distinctHosts))
        for _, it := range group {
            it.SignalStrength = strength  // 新字段
        }
    }
}

// fuzzy match: title Levenshtein ratio > 0.7 OR 主体词(title前3个token)匹配
func groupBySimilarTitle(items []*store.RawItem) [][]*store.RawItem {
    // 实现省略
}
```

**Step 2 — RawItem struct 加字段**: `SignalStrength float64` (非持久化，只在 pipeline 内存中用)

**Step 3 — rank 用 signal_strength 加权**:
```go
// rank.go
for i, item := range ranked {
    weight := 0.5 + float64(sourcePriority)/10.0
    signalBonus := 1.0 + math.Log1p(item.SignalStrength)  // 1 host 时 bonus=1, 3 hosts 时 ~1.69, 5 hosts 时 ~1.92
    ranked[i].FinalScore = ranked[i].LLMScore * weight * signalBonus
}
```

#### 技术验证
- **单测 groupBySimilarTitle**: 给定 ["OpenAI acquires Hiro", "OpenAI 收购 Hiro 加码金融服务", "Hiro 被 OpenAI 收购"] → 归为同一 group；"xAI 发布 Grok 3" 独立 group
- **单测 signal_strength 计算**: 同 group items 的 SignalStrength 相同；distinctHosts 计算准确
- **集成测**: 今日实际 rank 数据里，看是否出现 strength > 1 的 items；它们应该有更高 final rank
- **验证标准**: 人工抽查：被 2+ 源报道的大事件，是否进了 top 30 前列

#### 影响范围
| 维度 | 影响 |
|---|---|
| **代码文件** | 新增 `internal/ingest/signal_strength.go`；`internal/store/types.go` RawItem 加字段；`cmd/briefing/run.go` 插入 CalculateSignalStrength 调用点；`internal/rank/rank.go` 用字段 |
| **数据库** | 无（字段仅内存） |
| **性能** | fuzzy match 是 O(n²) 对 1500 items 做 = 2.25M 比较，每次 Levenshtein ~1μs → ~2 秒；可接受 |
| **风险** | fuzzy match 准确度是关键：阈值太低会误合并不同事件，太高会漏合并相同事件；需 manual tuning |
| **回滚** | signalBonus 系数改 1.0 即可禁用 |

---

### 4.3 语义去重 (LLM-based)

#### 优化事项
URL + 标题 dedup 对"同一事件不同源不同标题"命中率低。例如："OpenAI acquires Hiro, a personal finance AI startup" vs "OpenAI 收购 Hiro 加码个人理财" 当前不会被 dedup。

#### 技术方案

**介入位置**: rank 完成后 (30 items) 加一轮 LLM 调用："这 30 条里哪些是同一事件？"

**LLM prompt**:
```
以下 30 条新闻，请返回 JSON 数组指出哪些是同一事件。
格式: [{"group": [1, 5, 12], "canonical_title": "一句话概述"}, ...]
只合并"明显是同一事件"(同一公司同一产品同一动作)，不合并"相关事件"(比如 OpenAI 发新模型 + Anthropic 同日发新模型 是两个事件)
```

**Step 后**: 每 group 保留一条 (按 signal_strength × priority 排序选最佳)，其余丢弃。

#### 技术验证
- **单测**: 给定已知测试案例 (构造 3 条同事件标题, 1 条独立标题) → 验证返回正确 group
- **集成测**: 跑真实数据，对比 dedup 前 30 条 vs dedup 后 N 条；人工抽查 dedup 掉的是否真是重复
- **Cost 验证**: 单次 LLM 调用耗时 + token 数在可接受范围 (<30s, <500 tokens output)

#### 影响范围
| 维度 | 影响 |
|---|---|
| **代码文件** | 新增 `internal/rank/semantic_dedup.go` + prompt + LLM client；`cmd/briefing/run.go` 插入调用 |
| **数据库** | 可选：把 dedup group 写入 DB 便于审计 |
| **性能** | +1 次 LLM 调用，~30 秒；pipeline 总时间 +30s |
| **Cost** | 单次 $0.01 量级（30 items × 100 tokens per item input + 500 tokens output） |
| **风险** | LLM 可能过度合并（丢掉独立事件），设 A/B 开关先并行跑 1 周 |
| **回滚** | config 加 `semantic_dedup_enabled: false` 即可关 |

---

### 4.4 Reddit/HN 价值度分层

#### 优化事项
r/LocalLLaMA / r/MachineLearning / HN Top 的热帖有时是 meme 或 drama，不是新闻。当前一股脑归 social section，稀释了 social 内容质量。

#### 技术方案

**Step 1 — adapter 加 min_score 过滤**:
```yaml
# config/ai.yaml
- id: reddit-ml
  type: reddit_json
  min_score: 50  # 新增：upvotes 阈值
- id: hn-top
  type: hn_top
  min_points: 100  # 新增
```

```go
// internal/ingest/reddit.go
if item.Score < s.cfg.MinScore { continue }
// internal/ingest/hn.go
if item.Points < s.cfg.MinPoints { continue }
```

**Step 2 — classify 规则加强**:
在 `internal/classify/classify.go` 的 fallbackSection 或 ruleClassify，加规则：
- reddit / hn item 的 title 包含具体 AI 公司 + 产品/功能关键词 → `product_update`
- reddit / hn item 的 title 含"release"/"launch"/"announce" → `product_update`
- 否则才归 social

#### 技术验证
- **单测 min_score**: mock reddit 返回 10 条 (score 从 10 到 200 均匀分布)，min_score=50 → 保留约 7 条
- **单测 classify 规则**: "OpenAI releases GPT-5.5" → product_update（即使来自 HN）
- **集成测**: 跑一周看 social section 质量是否提升（人工 review，看 meme 出现频率）

#### 影响范围
| 维度 | 影响 |
|---|---|
| **代码文件** | `internal/ingest/reddit.go`, `internal/ingest/hn.go`, `internal/classify/classify.go` |
| **配置文件** | `config/ai.yaml` 3 个 community source 加 min_score/min_points |
| **数据分布** | social section 数量减少、质量提升；product_update/industry section 可能多出 reddit/hn 来的 item |
| **风险** | min_score 太高会过滤掉有价值的 niche 讨论；建议从 50/100 起，按 1 周数据调优 |
| **回滚** | min_score=0 即可放开 |

---

## Phase 5 — 长期实验 (按 ROI 做)

### 5.1 Twitter/X key accounts

- **价值**: Sam Altman / Karpathy / LeCun 等首发 X
- **成本**: X API $100+/月 OR Nitter RSS 镜像（不稳）
- **状态**: 暂缓，等 X 降价或 Nitter 稳定

### 5.2 Product Hunt AI 新品
- **RSS**: `https://www.producthunt.com/feed`
- **价值**: 低，适合周刊不适合日报
- **状态**: P3

### 5.3 国内公众号爬取 (战略级)

**Targets**: 阿里达摩院 / 字节豆包 / 腾讯 AI Lab / 华为诺亚 / DeepSeek / 百度文心
**方案**: 自建 RSSHub / WeRSS
**复杂度**: 高（反爬风险，服务不稳定）
**价值**: 战略级 (国内厂商首发消息)

### 5.4 Arxiv 前置过滤

现状：1200 papers/天 进 rank，80% 噪声
方案：
- Semantic Scholar API 拿 influence score 预筛
- 或关键词过滤 (LLM / transformer / agent / RAG 等)
- 目标：arxiv ingest 1200 → 100-200

---

## 6. 执行策略

### 6.1 顺序 (ROI × 紧迫度)

```
Phase 1 (bug 修)          — ROI 最高,立刻
Phase 3.1 (厂商官博)      — 零代码
Phase 4.1 (priority 权重) — 小改大收益
Phase 3.2 (商业/融资)     — 覆盖空白
Phase 4.2 (多源交叉)      — signal 强度
Phase 4.3 (语义去重)      — 锦上添花 (需 LLM key)
Phase 4.4 (Reddit/HN)    — social 质量
Phase 3.3 (政策)          — 立体化
Phase 5 (长期)            — 按 ROI (含国内媒体, 原 Phase 2 推到这里)
```

### 6.2 每 phase DoD

- 代码改动通过 `go build ./cmd/briefing` + 现有单测不破
- 新增改动带对应单测
- `briefing run --dry-run` 跑通，ingest 日志对应源 item 数合理
- 本文对应章节加 `✅ done 日期` 勾选
- commit message 引用本 plan 章节号
- 本地测试 → test Slack 验证 → 用户 approve → git commit → 部署 (参考 v1.0.1 plan §6)

### 6.3 总时间估 (AI 辅助开发, 专注投入)

**注**: 初版估 9-13 天叠了两层 buffer (人工节奏 + 测试/review 时间), 已修正为真实工作量。

| Phase | 任务详解 | 真实工作量 |
|---|---|---|
| 1.1 OpenAI News fix | 诊断 RSS + 1 处代码改 + 单测 | 1-2 小时 |
| 1.2 Google News fallback | queries 数组 + adapter 改 | 1 小时 |
| 1.3 source_health 表 | migration + 2 方法 + status 子命令 | 3-4 小时 |
| ~~2.x 5 个中文源~~ | **跳过 (服务器无法直连国内 CDN), 推到 Phase 5.3** | — |
| 3.1 5 个厂商官博 | YAML 配置 + 每源 URL 验证 | 2 小时 (平均 25 分钟/源) |
| 3.2 4 个商业源 | 同 3.1 | 1.5 小时 |
| 3.3 政策源调研 + 接入 | 调研 URL + 1-2 源 + classify 规则 | 3-4 小时 |
| 4.1 priority 权重 | rank.go 小改 + 单测 | 2-3 小时 |
| 4.2 signal_strength | fuzzy match + 集成 + 单测 | 4-5 小时 |
| 4.3 语义去重 | prompt + LLM 调用 + 集成 | 2-3 小时 |
| 4.4 reddit/hn 分层 | config + adapter + classify 规则 | 2 小时 |
| **Phase 1+3+4 合计** | — | **~22-28 小时** = **3 整天** |
| 5 (长期 4 项, 含原 Phase 2) | — | 不在 Phase 1-4 预算内 |

### 按"Claude 执行"的真实估算

本项目由 AI (Claude) 辅助开发, 执行节奏与人工程师不同：
- 写代码: 分钟级, 不是小时级
- 编译 + 单测: 几秒
- 主要耗时: **pipeline 真跑验证** (每次 20-30 分钟, LLM 延迟决定)

| 任务 | Claude 编码耗时 | 含 pipeline 验证 |
|---|---|---|
| 1.1 OpenAI News fix | 30 min | 1 小时 (含 1 轮验证) |
| 1.2 Google News fallback | 15 min | 15 min (单测为主) |
| 1.3 source_health 表 | 45 min | 1 小时 |
| ~~2.x 5 中文源~~ | **跳过 (服务器条件不支持)** | — |
| 3.1 5 厂商官博 | 15 min | 30 分钟 |
| 3.2 4 商业源 | 15 min | 30 分钟 |
| 3.3 2 政策源调研+接入 | 45 min | 1 小时 |
| 4.1 priority 权重 | 20 min | 30 分钟 |
| 4.2 signal_strength | 45 min | 1 小时 |
| 4.3 语义去重 | 40 min | 1 小时 |
| 4.4 reddit/hn 分层 | 20 min | 30 分钟 |
| **Phase 1+3+4 编码合计** | **~5 小时** | — |
| **含验证合计** | — | **6-7 小时 = 一晚 session** |

### 三档执行计划 (按用户可用时长切)

| 档位 | 范围 | 时间 | 说明 |
|---|---|---|---|
| 🟢 **一口气** | Phase 1-4 全部 | **一晚 7-8 小时** | 类似今晚的 critical fix 节奏 |
| 🟡 **两晚分开** | 第一晚 Phase 1-3 / 第二晚 Phase 4 | **2 × 3-4 小时** | 更舒服的节奏 |
| 🟠 **按 Phase 慢推** | 每晚 1-2 个 Phase | **4-5 晚 × 2-3 小时** | 最保守 |
| 🔵 **含 A/B 观察** | Phase 1-4 完 + 数据跑 2 周调优 | **1 晚 + 2 周观察** | 只在追求极致质量时 |

---

## 7. 删除条件

本 plan 在 Phase 1-4 全部完成后可删除（Phase 5 抽成独立 `long-term-backlog.md`）。

删除前 check:
- [ ] Phase 1.1-1.3 全 done
- [x] ~~Phase 2 5 个中文源集成~~ (跳过, 推到 Phase 5.3)
- [ ] Phase 3.1-3.3 所有源集成
- [ ] Phase 4.1-4.4 所有 signal quality 改动 merged
- [ ] 每一项在 git log 可找到对应 commit

---

## 8. 变更日志

- 2026-04-14 夜 — 初版 (Claude Opus 4.6, session 757058be)
- 2026-04-14 夜 — 按用户要求补充每项 4 个维度 (优化事项/技术方案/技术验证/影响范围)
