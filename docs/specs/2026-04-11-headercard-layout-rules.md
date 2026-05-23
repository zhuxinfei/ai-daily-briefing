# briefing-v3 大字报 (HeaderCard) 设计规则

> **状态**: v1.0.0 急救++ 版生效中  
> **目标**: 1600×1600 PNG, hubtoday-style newspaper layout  
> **适用**: `scripts/gen_info_card.py:render_header_card` + `cmd/briefing/run.go:buildFallbackHeaderCard` + `internal/infocard/infocard.go:HeaderCard`

## 0. 设计哲学 (用户原则)

| # | 原则 | 用户原话 |
|---|---|---|
| P1 | **图片优先信息源** | "优先从信息源链接下拿图，但如果没有允许生成，要确保是有解释意义的图" |
| P2 | **fail-soft 不阻塞** | "图片生成不了也要正常显示信息内容，不能因为局部的小问题影响整篇早报的完整性" |
| P3 | **报纸排版精神** | "参考 hubtoday 排版特点 — 不是一模一样" |
| P4 | **信息满满** | "除了正常四周留白，内部信息满满，像真的报纸一样" |
| P5 | **完整短句** | "起码要一句话把信息主题交代清楚，不能 ...XXX发布,引发....这样" |
| P6 | **梯度清晰** | "起码要分个 1 2 3，再是其他，重点要突出" |
| P7 | **dedup 跨 run** | "我希望每一次都是新的信息，而不是重复的内容收到3次" |
| P8 | **北京时间** | "所有时间统一用 Asia/Shanghai" |

---

## 1. Layout 几何 (1600×1600 画布)

```
+--------------------------------------------------+ 0
| 0-60   masthead bar  (黑底)                        |
+--------------------------------------------------+ 60
| 80-150 [红条 DATE]                  edition (右)   |
+--------------------------------------------------+ 150 ── rule
| 主区 LEFT 62% (922px)  |  RIGHT 38% (510px)        |
| 150-720                |                          |
|                        |  今日要闻 (red label)    |
|  L1 64px 主标题        |  L2 40px 第二头条        |
|  导语 26px lead 7 行   |                          |
|                        |  次要看点 (red label)    |
|                        |  L3 30px 第三头条        |
+--------------------------------------------------+ ~720 ── rule
| 中区 LEFT             |  RIGHT                    |
| TOP STORIES (red)     |  BY THE NUMBERS (red)     |
| 6 stories 2×3 grid    |  6 numbers 2×3 grid       |
| row_h=140            |  num_row_h=128             |
+--------------------------------------------------+ ~1100 ── rule
| 下区 LEFT             |  RIGHT                    |
| MORE STORIES (red)    |  今日板块速览 (red)       |
| 5 stories list        |  5 sections + count       |
+--------------------------------------------------+ ~1480
| footer bar  (黑底)                                 |
+--------------------------------------------------+ 1600
```

**Padding**: `pad_x = 56`  
**Content width**: `1488 = 1600 - 56*2`  
**Left col**: `int(1488 * 0.62) = 922`  
**Col gap**: `40`  
**Right col**: `1488 - 922 - 40 = 526` (实际 ~510)

---

## 2. 文字字号 + 颜色

| 区域 | 字号 | 颜色 | 字体 | 说明 |
|---|---|---|---|---|
| L1 main_headline | **64px** | INK_MAIN (黑) | Bold | 左大栏 max 3 行 |
| Lead paragraph | **26px** | INK_MAIN | Regular | 左大栏 max 7 行 |
| L2 (今日要闻) | **40px** | INK_MAIN | Bold | 右栏 max 3 行 |
| L3 (次要看点) | **30px** | INK_MAIN | Bold | 右栏 max 3 行 |
| section_label (红色 LABEL) | 22px | ACCENT_RED | Bold | "TOP STORIES" "BY THE NUMBERS" 等 |
| story_tag | 20px | ACCENT_RED | Bold | "产品" "研究" 等 |
| story_title (mid grid) | 28px | INK_MAIN | Bold | max_lines 3 |
| keynum_value | 78px | ACCENT_BLUE | Bold | 大字数字 |
| keynum_label | 22px | INK_SOFT | Regular | 数字下标 |
| more_story | 26px | INK_MAIN | Bold | 下区 stories |
| section_name | 28px | INK_MAIN | Bold | 板块速览左 |
| section_count | 34px | ACCENT_BLUE | Bold | 板块速览右 |
| date | 40px | ACCENT_RED | Bold | 顶部 meta |
| edition | 32px | INK_SOFT | Bold | 顶部 meta 右 |
| masthead/footer mono | 26px | INK_MAIN | Bold | 黑底白字 |

**颜色常量** (定义在 `gen_info_card.py` 顶部):
- `BG_MAIN` 米黄底色
- `INK_MAIN` 主文字 (近黑)
- `INK_SOFT` 次文字 (灰)
- `ACCENT_RED` 红色 accent
- `ACCENT_BLUE` 蓝色 accent
- `RULE` 分割线灰

---

## 3. 文字内容生成规则 (`buildFallbackHeaderCard` + `/tmp/regen_header_r2.py`)

### 3.1 main_headline (L1)

**输入**: `summary` 第一行 (`lines[0]`)  
**处理**:
1. **按第一个标点切分** —— 让 L1 永远是一个完整短句
   ```python
   for sep in ["，", "。", "；", ",", ".", ";"]:
       idx = mainHeadline.find(sep)
       if idx > 0:
           mainHeadline = mainHeadline[:idx]
           break
   ```
2. **50 字硬限**保险 (truncate runes)

**为什么**: 用户原话 "起码要一句话把信息主题交代清楚"。按字符 truncate 会出现 "Spark 冲上 App Sto..." 这种半句话；按标点切让 L1 永远是 "Meta AI App 借 Muse Spark 冲上 App Store 第5" 这样一句完整话。

### 3.2 sub_headlines (L2 / L3 / L4)

**输入**: `summary` 第 2-4 行 (`lines[1..3]`)  
**处理**: 跟 L1 完全相同 — 按第一个标点切 + 60 字硬限  
**输出格式**: 用 `\n` 拼接成单一字符串塞进 `HeaderCard.SubHeadline`，PIL 渲染时按 `\n` 拆分独立渲染

### 3.3 lead_paragraph (左大栏导语)

**输入**: `summary 全部行` + `items[0..3].title` 拼接  
**分隔符**: `" · "`  
**280 字硬限**

```go
var leadParts []string
leadParts = append(leadParts, lines...)  // summary 三行
for i, it := range items[:4] {
    if it != nil {
        leadParts = append(leadParts, strings.TrimSpace(it.Title))
    }
}
leadParagraph := truncRunes(strings.Join(leadParts, " · "), 280)
```

### 3.4 top_stories (TOP STORIES + MORE STORIES 共享)

**section quota** (LLM rank 排序后按 section 取):
- product_update: 3
- research: 2
- industry: 2
- opensource: 1
- social: 1
- 总数 max 11 条

**title 处理**:
- `strings.TrimLeft(*)` 剥 markdown emphasis
- 60 字硬限 (允许一句话完整)
- **不**按标点切（stories title 通常本来就是一句话）

**stories[0..6]** → mid zone TOP STORIES 2×3 grid  
**stories[6..11]** → bot zone MORE STORIES list (max 5)

### 3.5 key_numbers (BY THE NUMBERS, max 6)

**优先**: 从 summary 提取数字 (`\d+%|\d+`)  
**兜底统计** (如果不够 6 个):
1. `今日条目` = items 总数
2. `覆盖板块` = unique sections 数
3. `信息源` = 21 (固定，sources 数)
4. `时间窗口` = "24h"
5. `领域` = "AI"
6. `版本` = "v1.0"

### 3.6 sections_overview (今日板块速览)

**自动**: 用 `top_stories` 的 `tag` 字段 Counter 统计 top 5 sections  
**显示**: 板块名 + count

---

## 4. PIL 渲染规则 (`render_header_card`)

### 4.1 wrap 算法 — Smart word-aware wrap

**核心修复**: `wrap_by_width` 不再纯逐字符切，而是：
- **ASCII alphanumeric runs** (`isascii() and isalnum()`): 整个 word 拉为一个单元（避免 "Spark" → "Spar/k"）
- **CJK / 标点 / 空格**: 逐字符切（中文没有词边界，char-level OK）
- **超长 ASCII word fallback**: 如果一个 word > max_width，对这个 word 内部 char-level 强制切

```python
# ASCII word: pull whole
if ord(ch) < 128 and ch.isalnum():
    # collect [a-z A-Z 0-9 . _ ' -] until non-ASCII
    word = text[i:j]
    if measure(line + word) > max_width:
        # 换行 (但不切 word)
        out.append(line)
        line = word
    else:
        line = line + word
# CJK: char-by-char
else:
    # 跟旧逻辑一样
```

### 4.2 主区 (LEFT L1+lead, RIGHT L2+L3)

**两列独立 y-tracker** (`left_y` / `right_y`)，渲染完后取 `max(left_y, right_y)` 作为主区底部。

**RIGHT 渲染顺序**:
```python
sub_lines = sub_headline.split("\n")
for i, line in enumerate(sub_lines[:2]):  # 只渲染 2 个 (L2 + L3)
    label = ["今日要闻", "次要看点"][i]
    font = [f_l2, f_l3][i]
    draw red label + draw black headline
```

### 4.3 中区 (LEFT TOP STORIES grid, RIGHT BY THE NUMBERS grid)

- **TOP STORIES**: `stories[:6]`, 2 cols × 3 rows, `cell_w = (left_w - col_inner_gap) // 2`, `row_h = 140`
- **BY THE NUMBERS**: `key_numbers[:6]`, 2 cols × 3 rows, `num_cell_w = (right_w - num_col_gap) // 2`, `num_row_h = 128`

每个 cell 内部:
- TOP STORIES: 红色 tag (24px) → black title (28px, max_lines 3, line_spacing 1.20)
- BY THE NUMBERS: 蓝色 value (78px) → 灰色 label (22px, value+76px offset)

### 4.4 下区 (LEFT MORE STORIES, RIGHT 今日板块速览)

- **MORE STORIES**: `stories[6:11]`, max 5 条, 每条 = 红色 tag + 黑色 title (26px, max_lines 2)
- **今日板块速览**: 5 行 = 板块名左 (28px black) + count 右 (34px blue), 行高 40px, 自动从 tags counter

### 4.5 footer bar

`draw_footer_bar`: 黑底，左 "briefing-v3 · hero"，右 "HEADLINE · 1600 × 1600"

---

## 5. 缓存与服务规则 (`briefing serve`)

**`/cards/` 路径下的 PNG**: `Cache-Control: no-store, max-age=0, must-revalidate`  
**其他静态图** (theme assets / logo): `public, max-age=86400, immutable`

为什么：hero 大字报路径固定 (`/images/cards/{date}/header.png`) 但内容每天更新一次。`immutable` 让浏览器永远缓存 → 用户永远看到旧图。`/cards/` 必须 no-store。

---

## 6. fail-soft 链 (LLM 失败兜底)

```
infocard.Generate (LLM JSON call)
  ├── 成功 → header + cards → PIL 渲染 hero + N item cards
  └── 失败 (e.g. 6min timeout)
       └── buildFallbackHeaderCard(items, summary, issueNumber, date)
              ├── 用规则构造 HeaderCard JSON (不调 LLM)
              └── renderInfoCardPNG("header", fallbackHeader, headerPath)
                     └── PIL 渲染兜底 hero
```

**关键原则**: infocard LLM 失败 ≠ 没大字报。永远保证有一张当天内容的 PNG。

---

## 7. dedup 规则 (`data/sent_urls.txt`)

**机制**: 一个文件持久化 set，每行一个 URL  
**写入时机**: publish 成功后 (`appendSentURLs`)  
**读取时机**: filter 之后 / rank 之前 (`dedupRawItemsBySent`)  
**fail-soft**: 任何 IO 错误都 print warn 不阻塞 pipeline

**为什么**: 同一天多次 run 必须给用户全新内容，不能 3 次都收到同一批 stories。

---

## 8. 已知遗留问题 / TODO

| # | 问题 | 临时方案 | 长期方案 |
|---|---|---|---|
| T1 | infocard LLM 6 分钟超时 | 本地 fallback header | 减小 prompt size / 换更稳定的 model |
| T2 | cloudflared Quick Tunnel 不稳定 (重启换 hostname) | 手动改 secrets.env BRIEFING_REPORT_URL_BASE | 升级 Named Tunnel + 稳定 subdomain |
| T3 | sub_headline 字段语义现在是 "\n 分隔的 L2/L3" | 改 PIL 拆分 | 加 `SecondaryHeadlines []string` 字段 + LLM prompt 同步 |
| T4 | CJK 词边界 wrap (e.g. "冲上" 可能被分两行) | 视觉无大问题 | jieba 分词 |
| T5 | LLM main prompt 还在生成长字符串 | buildFallbackHeaderCard 兜底 | LLM prompt 加约束 "main_headline 30 字内, 一句完整" |

---

## 9. 改动文件清单 (v1.0.0+ 急救周期)

1. **`scripts/gen_info_card.py`** - PIL 模板
   - `wrap_by_width` 改 ASCII word-aware
   - `render_header_card` 完全重写为 newspaper 左右分栏 layout
2. **`cmd/briefing/run.go`**
   - `buildFallbackHeaderCard` 新增 (本地 PIL 兜底)
   - `buildFallbackSummary` 新增 (summary fail-soft)
   - `enrichItemsWithMedia` 恢复 Pollinations + 改 prompt (`title + body 摘要`)
   - `dedupRawItemsBySent` / `appendSentURLs` / `loadSentURLs` / `collectIssueItemSourceURLs` 新增
   - infocard 失败路径调 fallback header
3. **`cmd/briefing/serve.go`**
   - `wrapFileServer` 给 `/cards/` 路径加 `no-store`
4. **`internal/infocard/infocard.go`**
   - `HeaderCard` 加 `Edition`, `LeadParagraph`, `KeyNumbers` 字段
   - `KeyNum`, `TopStory` 子 struct
5. **`internal/render/hugo.go`**
   - `bannedImageHosts` 清空 (允许 Pollinations 通过)
6. **`/etc/systemd/system/briefing-daily.service`**
   - ExecStart 删 `--no-images` flag
7. **`config/secrets.env`**
   - `BRIEFING_REPORT_URL_BASE` 更新为新 cloudflared hostname
