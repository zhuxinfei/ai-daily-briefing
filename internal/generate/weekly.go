package generate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"briefing-v3/internal/llm"
	"briefing-v3/internal/store"
)

// WeeklyConfig parameterizes the LLM client used by the weekly pass.
type WeeklyConfig struct {
	BaseURL     string
	APIKey      string
	Model       string
	Temperature float64
	Timeout     time.Duration
	MaxRetries  int
	// v1.0.1 Phase 4.5 (W4): 对齐日报 openai.go 的 retry 策略 —— 分钟级
	// 指数退避, 从 ai.yaml llm.retry_backoff_seconds 读取. 长度决定有效
	// attempts, 空则 fallback 到 [10,30,90,180,300].
	RetryBackoffSeconds []int
}

func (c *WeeklyConfig) fillDefaults() {
	if c.Temperature == 0 {
		c.Temperature = 0.4
	}
	if c.Timeout <= 0 {
		c.Timeout = 180 * time.Second
	}
	if c.MaxRetries <= 0 {
		c.MaxRetries = 5
	}
	if len(c.RetryBackoffSeconds) == 0 {
		c.RetryBackoffSeconds = []int{10, 30, 90, 180, 300}
	}
}

// DailyBundle groups the data for one daily issue needed by the weekly prompt.
type DailyBundle struct {
	Issue   *store.Issue
	Items   []*store.IssueItem
	Insight *store.IssueInsight
}

// WeeklyResult is the structured output of GenerateWeekly.
type WeeklyResult struct {
	TitleKeywords       string
	FocusMD             string
	SignalsMD           string
	TrendsMD            string
	TrendsDiagram       string // Mermaid code for simple trends diagram
	TrendsDiagramDetail string // Mermaid code for detailed trends diagram
	TakeawaysMD         string
	PonderMD            string
}

const weeklySystemPrompt = `你是一位资深AI行业分析师，负责撰写每周综合分析报告。

你的读者是一家AI创业公司的全体员工——有CEO、技术、设计、HR、运营，大部分人不懂技术。
他们已经看过本周的每日日报，现在需要你做一份"周度总结"，帮他们把碎片化的每日信息串成线索。

公司背景：产品尚未上市的早期团队，方向是Agent调度与进化平台——简单说就是帮普通人像叫外卖一样使用AI，让好的AI方案能被评价、选择和信任。to C为主to B为辅。

你会收到本周每日日报的标题、条目摘要和行业洞察。请输出以下内容（严格 JSON）：

{
  "title_keywords": "2-4 个本周最核心关键词, 用顿号分隔",
  "focus": "本周聚焦 — 选 2-3 件本周最重要的事做深度拆解。每件事: 发生了什么 → 为什么重要 → 对行业意味着什么。共 1200-1800 字。使用 markdown 格式，每件事结构严格按以下顺序输出:\n\n### 小标题\n\n简版图谱（三个反引号mermaid围栏，graph LR，4-5节点，一条主线 A-->|词|B-->|词|C-->|词|D，classDef blue/yellow/green 着色，不用 start/end 保留字，不加分号）\n\n然后紧跟一个 HTML 折叠块，内含详版图谱:\n<details>\n<summary>展开详细图谱</summary>\n\n三个反引号mermaid围栏，graph TD，8-12节点，用 subgraph 分组(事件/影响/启示)，节点形状区分(方括号=事件 花括号=判断 双括号=结论)，classDef 着色(blue fill:#dbeafe,stroke:#3b82f6 / yellow fill:#fef3c7,stroke:#f59e0b / green fill:#d1fae5,stroke:#10b981)，边上标关系词用 -->|词| 语法，展示完整因果链和多维影响\n\n</details>\n\n最后才是文字分析。",
  "signals": "信号与噪音 — 5-7 条本周值得注意但没有大到需要深度拆解的事件。每条用有序列表: 一句话事实 + 一句话点评。共 800-1200 字。",
  "trends": "宏观趋势 — 从本周事件中提炼 3-4 个趋势方向。每个趋势: 趋势名称 + 本周哪些事件验证了这个趋势 + 未来可能走向。共 400-600 字。",
  "trends_diagram": "一段 mermaid 代码（不含围栏标记），画一张简版'本周核心脉络图'。设计原则: 让不懂技术的人 5 秒看懂。要求: graph LR; 3-4 个趋势大节点串联; classDef blue fill:#dbeafe,stroke:#3b82f6,color:#111827 染色; 总共 5-7 个节点; 文字极简(4-10字); 边标签用 -->|词| 语法; 不用 subgraph; 不用 start/end 保留字; classDef 行末不加分号。",
  "trends_diagram_detail": "一段 mermaid 代码（不含围栏标记），画一张详版趋势全景图。要求: graph TD; 用 subgraph 按趋势方向分组(每组 2-3 个事件节点); 组之间用虚线(-.->|关系词|)连接; classDef blue fill:#dbeafe,stroke:#3b82f6,color:#111827 / classDef yellow fill:#fef3c7,stroke:#f59e0b,color:#111827 / classDef green fill:#d1fae5,stroke:#10b981,color:#111827 / classDef purple fill:#ede9fe,stroke:#8b5cf6,color:#111827; 每个 subgraph 内节点用不同颜色; 12-16 个节点; 边标签用 -->|词| 语法; 不用 start/end 保留字; classDef 行末不加分号。",
  "takeaways": "对我们的启发 — 从 Agent 调度平台的角度, 本周事件给我们什么参考。产品方向/竞争策略/时机判断, 各 1-2 条。共 300-500 字。",
  "ponder": "本周思考 — 一个引发深度思考的问题, 不需要有答案, 让读者带着问题进入下一周。1-2 句话。"
}

【写作规则】
1. 每件事都追溯到本周具体日报中的具体事件, 不凭空分析
2. 严格客观, 好消息坏消息都说, 不讨好读者
3. 非大众熟知的概念必须加括号注释（标准：会用ChatGPT但不会写代码的老板是否认识）
4. 不硬凑: 如果本周某个板块素材不足, 宁可少写, 不要注水
5. 禁止输出任何运维、排障、调度、发送、监控信息
6. 不要输出任何 JSON 以外的文字`

// GenerateWeekly calls the LLM to produce a weekly analysis from daily bundles.
func GenerateWeekly(ctx context.Context, cfg WeeklyConfig, startDate, endDate time.Time, dailies []DailyBundle) (*WeeklyResult, error) {
	if len(dailies) == 0 {
		return nil, fmt.Errorf("weekly: no daily bundles")
	}
	cfg.fillDefaults()

	userPrompt := buildWeeklyUserPrompt(startDate, endDate, dailies)

	hc := &http.Client{}
	llmCfg := llm.Config{
		BaseURL: cfg.BaseURL, APIKey: cfg.APIKey, Model: cfg.Model,
		Temperature: cfg.Temperature, MaxTokens: 16384, Timeout: cfg.Timeout,
	}
	// v1.0.1 Phase 4.5 (W4): LLM retry 分钟级退避, 对齐日报 openai.go 行为.
	// 网络/API transport error 等 backoff 后重试; JSON parse error 不等, 直
	// 接下一次 (LLM 能回但格式错, 下次可能就对了).
	backoffs := cfg.RetryBackoffSeconds
	if len(backoffs) == 0 {
		backoffs = []int{10, 30, 90, 180, 300}
	}
	maxAttempts := cfg.MaxRetries
	if maxAttempts > len(backoffs) {
		maxAttempts = len(backoffs)
	}
	if maxAttempts == 0 {
		maxAttempts = len(backoffs)
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		raw, err := llm.ChatComplete(ctx, hc, llmCfg, weeklySystemPrompt, userPrompt)
		if err != nil {
			// transport error → backoff 等待后重试
			lastErr = err
			if attempt < maxAttempts {
				backoff := time.Duration(backoffs[attempt-1]) * time.Second
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(backoff):
				}
			}
			continue
		}
		result, perr := parseWeeklyJSON(raw)
		if perr != nil {
			// parse error 不 backoff (LLM 能回, 下一次 temperature jitter 可能就对)
			lastErr = perr
			continue
		}
		return result, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("weekly: failed with no specific error")
	}
	return nil, lastErr
}

func buildWeeklyUserPrompt(startDate, endDate time.Time, dailies []DailyBundle) string {
	var b strings.Builder
	fmt.Fprintf(&b, "本周日报汇总（%s ~ %s，共 %d 天）:\n\n",
		startDate.Format("2006-01-02"), endDate.Format("2006-01-02"), len(dailies))

	for _, d := range dailies {
		if d.Issue == nil {
			continue
		}
		fmt.Fprintf(&b, "=== %s ===\n", d.Issue.IssueDate.Format("2006-01-02"))
		fmt.Fprintf(&b, "日报标题: %s\n", d.Issue.Title)
		b.WriteString("条目摘要:\n")

		items := d.Items
		if len(items) > 15 {
			items = items[:15]
		}
		for _, it := range items {
			if it == nil {
				continue
			}
			fmt.Fprintf(&b, "- [%s] %s\n", it.Section, it.Title)
		}

		if d.Insight != nil {
			if ind := strings.TrimSpace(d.Insight.IndustryMD); ind != "" {
				fmt.Fprintf(&b, "行业洞察:\n%s\n", ind)
			}
			if our := strings.TrimSpace(d.Insight.OurMD); our != "" {
				fmt.Fprintf(&b, "对我们的启发:\n%s\n", our)
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("请严格按 system message 要求输出 JSON。")
	return b.String()
}

var weeklyFencedRe = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*\\})\\s*```")

func parseWeeklyJSON(raw string) (*WeeklyResult, error) {
	raw = strings.TrimSpace(raw)
	raw = weeklyFencedRe.ReplaceAllString(raw, "$1")
	raw = strings.TrimSpace(raw)

	if !strings.HasPrefix(raw, "{") {
		start := strings.Index(raw, "{")
		end := strings.LastIndex(raw, "}")
		if start >= 0 && end > start {
			raw = raw[start : end+1]
		}
	}

	var parsed struct {
		TitleKeywords string `json:"title_keywords"`
		Focus         string `json:"focus"`
		Signals       string `json:"signals"`
		Trends        string `json:"trends"`
		TrendsDiagram       string `json:"trends_diagram"`
		TrendsDiagramDetail string `json:"trends_diagram_detail"`
		Takeaways           string `json:"takeaways"`
		Ponder        string `json:"ponder"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("weekly: parse json: %w", err)
	}
	if strings.TrimSpace(parsed.Focus) == "" {
		return nil, fmt.Errorf("weekly: focus section is empty")
	}
	// LLM sometimes outputs literal "\n" (two chars) instead of real
	// newlines inside JSON string values. Replace them so markdown
	// renders correctly with proper paragraphs and headings.
	fix := func(s string) string {
		return strings.ReplaceAll(s, `\n`, "\n")
	}
	return &WeeklyResult{
		TitleKeywords:  parsed.TitleKeywords,
		FocusMD:        fix(parsed.Focus),
		SignalsMD:      fix(parsed.Signals),
		TrendsMD:       fix(parsed.Trends),
		TrendsDiagram:       fix(parsed.TrendsDiagram),
		TrendsDiagramDetail: fix(parsed.TrendsDiagramDetail),
		TakeawaysMD:         fix(parsed.Takeaways),
		PonderMD:       fix(parsed.Ponder),
	}, nil
}

