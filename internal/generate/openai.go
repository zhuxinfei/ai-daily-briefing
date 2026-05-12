package generate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"briefing-v3/internal/store"
)

// finalStrictSystemPrompt is the attempt-5 (last chance) system prompt. It
// is strictly more constrained than systemPrompt/repairSystemPrompt and is
// only used when attempts 1-4 have all failed validation. If this attempt
// also fails, the Generator hard-fails — no degraded fallback is produced.
const finalStrictSystemPrompt = `你是一位资深AI行业分析师。这是最后一次生成机会，前面的尝试都失败了，这次必须严格符合以下所有要求：

1. 必须严格输出两个模块：📊 行业洞察（3-4条）和 💭 对我们的启发（2-3条）
2. 严格使用有序列表格式（1. 2. 3.），每条40-70字，不得少于3条行业洞察，不得少于2条启发
3. 绝对禁止出现任何运维调度词汇：webhook、cron、schedule、缓存、轮询、幂等、具体时间戳、频道、告警、补发、北京时间、推送链路、本地设备、GitHub Actions
4. 每条洞察必须包含：具体事件（提公司/产品名）→ 明确判断 → 为什么这么判断
5. 必须加括号注释非大众熟知的专业名词（标准：会用ChatGPT但不懂代码的HR是否认识）
6. 必须客观中立，不讨好不吹捧，机会和风险都要说
7. "对我们的启发"聚焦于 Agent 调度与进化平台（A2A 方向），从产品、业务、市场、组织判断角度说

前几次失败的具体原因会在 user message 里列出，这次必须完全避开那些问题。`

// finalStrictUserPromptTemplate is the attempt-5 user prompt. Placeholders:
//
//	%s: joined failure reasons from previous attempts
//	%s: today's digest markdown (all issue items)
//	%s: source context (raw item excerpts)
const finalStrictUserPromptTemplate = `前几次输出全部失败，失败原因汇总：%s

这是最后一次机会。请严格输出下面两个模块，不允许任何偏差：

📊 行业洞察（今日N条）
输出3-4条，有序列表格式 1. 2. 3.，每条40-70字。
每条严格使用嵌套格式，第一行是事实，缩进行用【洞察】标签给判断：
1. 事实陈述（公司/产品/具体事件）
  【洞察】你的判断（为什么这么判断）

💭 对我们的启发（今日N条）
输出2-3条，有序列表 1. 2. 3.，每条30-60字。
引用今天的具体事件，说清楚跟我们做的 Agent 调度平台有什么关系，机会和风险都说。

绝对禁止事项：
- 任何运维、调度、发送、监控、缓存、时间戳、频道、告警、补发相关词汇
- 模板化语言（"In today's rapidly evolving..."/ "令人振奋的..." 等空洞套话）
- 编造事实或使用未在源材料中出现的公司和产品
- 一条中既有【洞察】又有序号前缀（【洞察】行不加序号）

--- 今日日报全文 ---
%s

--- 源链接原文 ---
%s`

// Config configures the OpenAI-compatible Generator.
//
// All fields must be set via the caller (typically read from env by main.go):
//
//	BaseURL     - e.g. "http://64.186.239.99:8080"
//	APIKey      - API key for the endpoint
//	Model       - e.g. "gpt-5.4"
//
// The remaining fields have sensible defaults if left zero.
type Config struct {
	BaseURL     string
	APIKey      string
	Model       string
	Temperature float64
	MaxTokens   int
	Timeout     time.Duration
	MaxRetries  int
	// v1.0.1: retry backoff sequence in seconds (e.g. [10,30,90,180,300]).
	// Length determines the effective max attempts; if empty, defaults to
	// [10,30,90,180,300]. Read from ai.yaml llm.retry_backoff_seconds.
	RetryBackoffSeconds []int
}

func (c *Config) fillDefaults() {
	if c.Temperature == 0 {
		c.Temperature = 0.3
	}
	if c.MaxTokens == 0 {
		c.MaxTokens = 2000
	}
	if c.Timeout == 0 {
		c.Timeout = 120 * time.Second
	}
	if c.MaxRetries == 0 {
		c.MaxRetries = 5
	}
	if len(c.RetryBackoffSeconds) == 0 {
		// Safe default: minute-scale backoff for upstream 502 tolerance.
		c.RetryBackoffSeconds = []int{10, 30, 90, 180, 300}
	}
}

// openaiGenerator implements Generator by calling an OpenAI-compatible
// chat/completions endpoint. It does NOT degrade gracefully: if all
// MaxRetries attempts fail validation, GenerateInsight returns a non-nil
// error so the caller can hard-fail the pipeline.
type openaiGenerator struct {
	cfg Config
	hc  *http.Client
}

// New returns a Generator backed by an OpenAI-compatible API endpoint.
// Returns an error if required Config fields are missing.
func New(cfg Config) (Generator, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("generate: Config.BaseURL is required")
	}
	if cfg.APIKey == "" {
		return nil, errors.New("generate: Config.APIKey is required")
	}
	if cfg.Model == "" {
		return nil, errors.New("generate: Config.Model is required")
	}
	cfg.fillDefaults()
	return &openaiGenerator{
		cfg: cfg,
		hc:  &http.Client{}, // per-request timeout applied via context
	}, nil
}

// chatMessage is an OpenAI-compatible role/content pair.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
		Index   int         `json:"index"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// GenerateInsight runs the 5-stage retry loop:
//
//	Attempts 1-2: standard systemPrompt + userPromptTemplate
//	Attempts 3-4: repairSystemPrompt + repairUserPromptTemplate with prior reasons
//	Attempt  5:   finalStrictSystemPrompt + finalStrictUserPromptTemplate, max_tokens x2
//
// Any attempt that produces a ValidationResult.OK==true short-circuits the loop.
// Network / transport errors are treated as retriable at the same stage;
// validation errors escalate to the next stage's prompt.
//
// If all MaxRetries attempts fail, this returns a non-nil error. The caller
// MUST treat this as a hard pipeline failure: do not publish, alert, and
// wait for human intervention.
func (g *openaiGenerator) GenerateInsight(ctx context.Context, in *Input) (*store.IssueInsight, error) {
	if in == nil || in.Issue == nil {
		return nil, errors.New("generate: nil input or issue")
	}

	digest := composeDigestMarkdown(in.Items)
	sourceCtx, snippetCount := composeSourceContext(in.RawItems)

	var (
		lastReasons []string
		lastRaw     string
		industryMD  string
		ourMD       string
		attempts    int
		lastHTTPErr error
	)

	// v1.0.1 Bug J 修复: insight 生成同样对 LLM 502 敏感 (compose 失败
	// 补不上 section 会连带 insight 缺上下文). 加入分钟级退避, 和
	// summarize 共享 cfg.RetryBackoffSeconds 序列 (ai.yaml 默认
	// [10,30,90,180,300]). Length 是有效 MaxAttempts 上限.
	backoffs := g.cfg.RetryBackoffSeconds
	if len(backoffs) == 0 {
		backoffs = []int{10, 30, 90, 180, 300}
	}
	maxAttempts := g.cfg.MaxRetries
	if maxAttempts > len(backoffs) {
		maxAttempts = len(backoffs) // 不超过 backoff 序列长度
	}
	if maxAttempts == 0 {
		maxAttempts = len(backoffs)
	}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		attempts = attempt

		system, user, maxTokens := g.promptForAttempt(
			attempt, digest, sourceCtx, snippetCount, lastReasons, lastRaw,
		)

		raw, err := g.chatComplete(ctx, system, user, maxTokens)
		if err != nil {
			// Network / API transport error. Retry with same stage prompt.
			// v1.0.1: sleep before next attempt to weather upstream抖动.
			lastHTTPErr = err
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
		lastHTTPErr = nil

		vr := ValidateInsight(raw)
		if vr.OK {
			industryMD = vr.IndustryRaw
			ourMD = vr.OurRaw
			break
		}

		// Validation failed. Remember reasons and raw for the next attempt's
		// repair/final prompt to reference. 验证失败不走 backoff (LLM 能
		// 回复但质量不好, 下一次 prompt 会升级到 repair/final strict).
		lastReasons = vr.Reasons
		lastRaw = raw
	}

	if industryMD == "" || ourMD == "" {
		if lastHTTPErr != nil {
			return nil, fmt.Errorf("generate: all %d attempts failed, last transport error: %w",
				attempts, lastHTTPErr)
		}
		return nil, fmt.Errorf("generate: insight validation failed after %d attempts, last reasons: %s",
			attempts, strings.Join(lastReasons, "; "))
	}

	// 保证日报关系图不缺席, 也不能只是 3 个截断节点的敷衍图。
	// 缺图或图太简单时, 用规则化 graph LR 替换, 并移除旧 mermaid/孤儿标题。
	combined := industryMD + "\n" + ourMD
	if block := mermaidBlockRegex.FindString(combined); block == "" || mermaidLooksTooSimple(block) {
		industryMD = stripMermaidArtifacts(industryMD)
		ourMD = stripMermaidArtifacts(ourMD)
		industryMD = ruleBasedMermaidDiagram(in) + "\n\n" + strings.TrimSpace(industryMD)
	}

	return &store.IssueInsight{
		IssueID:     in.Issue.ID,
		IndustryMD:  industryMD,
		OurMD:       ourMD,
		Model:       g.cfg.Model,
		Temperature: g.cfg.Temperature,
		RetryCount:  attempts,
		GeneratedAt: time.Now(),
	}, nil
}

var mermaidHeadingOnlyRe = regexp.MustCompile(`(?m)^\s{0,3}(?:#{1,6}\s*)?🗺️\s*今日关系图.*\n?`)
var mermaidNodeLabelRe = regexp.MustCompile(`[A-Z0-9]+\[([^\]]+)\]`)

// ruleBasedMermaidDiagram 用摘要 + section item 标题抽取主题, 生成一张
// 5 节点、1 条主线 + 1 条分支的关系图。目标不是“图能渲染”，而是 5 秒能看懂。
func ruleBasedMermaidDiagram(in *Input) string {
	labels := extractMermaidThemes(in)
	return "```mermaid\n" +
		"graph LR\n" +
		fmt.Sprintf("A[%s] -->|把结果做出来| D[%s]\n", labels[0], labels[3]) +
		fmt.Sprintf("B[%s] -->|降低创作门槛| D\n", labels[1]) +
		fmt.Sprintf("C[%s] -->|串起多助手| E[%s]\n", labels[2], labels[4]) +
		" D -->|进入业务流程| E\n" +
		"classDef blue fill:#dbeafe,stroke:#3b82f6,color:#111827\n" +
		"classDef green fill:#d1fae5,stroke:#10b981,color:#111827\n" +
		"class A,B,C blue\n" +
		"class D,E green\n" +
		"```"
}

// extractMermaidThemes 提取 3 个“事件/主题”节点 + 2 个固定结果节点.
// 优先从产品/开源/行业条目中抽语义标签, 摘要只做补位。
func extractMermaidThemes(in *Input) [5]string {
	var labels [5]string
	cur := 0

	appendTheme := func(line string) {
		if cur >= 3 {
			return
		}
		clean := mermaidThemeLabel(line)
		if clean == "" {
			return
		}
		for i := 0; i < cur; i++ {
			if labels[i] == clean {
				return
			}
		}
		labels[cur] = clean
		cur++
	}

	if in != nil {
		for _, it := range in.Items {
			if cur >= 3 || it == nil {
				break
			}
			if it.Section == store.SectionProductUpdate || it.Section == store.SectionOpenSource || it.Section == store.SectionIndustry {
				appendTheme(it.Title)
			}
		}
	}
	if in != nil && in.Issue != nil {
		for _, line := range strings.Split(in.Issue.Summary, "\n") {
			if cur >= 3 {
				break
			}
			appendTheme(line)
		}
	}
	if cur < 3 && in != nil {
		for _, it := range in.Items {
			if cur >= 3 || it == nil {
				break
			}
			appendTheme(it.Title)
		}
	}
	defaults := [3]string{"产品能力升级", "创作门槛下降", "多助手协作"}
	for i := cur; i < 3; i++ {
		labels[i] = defaults[i]
	}
	labels[3] = "AI直接交付"
	labels[4] = "企业工作流AI化"
	return labels
}

// mermaidLabelSeparatorRe 按标点切到第一个意义完整的短语. 不切空格
// (中文/英文混写或 "OpenAI 扩展" 这种名词短语要保留).
var mermaidLabelSeparatorRe = regexp.MustCompile(`[，,。；;、]`)

// mermaidLabelFrom 从一句话中提炼 4-8 字节点 label. 去除 markdown 符号/引号,
// 切到第一个分隔符, 截断过长.
func mermaidLabelFrom(line string) string {
	s := strings.TrimSpace(line)
	if s == "" {
		return ""
	}
	// 去 markdown 粗体/序号前缀.
	s = strings.TrimLeft(s, "0123456789. -*#`_")
	s = strings.ReplaceAll(s, "**", "")
	s = strings.Trim(s, "*_ `'\"")
	if s == "" {
		return ""
	}
	// 切到第一个分隔符.
	if idx := mermaidLabelSeparatorRe.FindStringIndex(s); idx != nil && idx[0] > 0 {
		s = s[:idx[0]]
	}
	// 截 8 rune (CJK 显示合适).
	rs := []rune(s)
	if len(rs) > 8 {
		rs = rs[:8]
	}
	return string(rs)
}

func mermaidThemeLabel(line string) string {
	lower := strings.ToLower(strings.TrimSpace(line))
	switch {
	case strings.Contains(lower, "claude design"):
		return "Claude做设计"
	case strings.Contains(lower, "generative ui"),
		strings.Contains(lower, "a2ui"),
		strings.Contains(lower, "界面标准"),
		strings.Contains(lower, "agent 界面"),
		strings.Contains(lower, "生成式界面"):
		return "Google做界面"
	case strings.Contains(lower, "personalized images"),
		strings.Contains(lower, "nano banana"),
		strings.Contains(lower, "google photos"),
		strings.Contains(lower, "图片生成"),
		(strings.Contains(lower, "gemini") && strings.Contains(lower, "图片")):
		return "Gemini做图片"
	case strings.Contains(lower, "openai-agents"),
		strings.Contains(lower, "agents-python"),
		strings.Contains(lower, "多 agent"),
		strings.Contains(lower, "多智能体"),
		strings.Contains(lower, "agent 工作流"),
		strings.Contains(lower, "mcp"):
		return "多Agent框架"
	case strings.Contains(lower, "enterprise"),
		strings.Contains(lower, "企业"),
		strings.Contains(lower, "cursor"),
		strings.Contains(lower, "ipo"),
		strings.Contains(lower, "cerebras"):
		return "企业化落地"
	case strings.Contains(lower, "token"),
		strings.Contains(lower, "成本"),
		strings.Contains(lower, "opus 4.7"):
		return "真实成本抬升"
	}
	return mermaidLabelFrom(line)
}

func mermaidLooksTooSimple(block string) bool {
	if strings.TrimSpace(block) == "" {
		return true
	}
	if len(mermaidNodeLabelRe.FindAllStringSubmatch(block, -1)) < 4 {
		return true
	}
	if strings.Count(block, "-->") < 3 {
		return true
	}
	if !strings.Contains(block, "classDef green") {
		return true
	}
	return false
}

func stripMermaidArtifacts(s string) string {
	s = mermaidBlockRegex.ReplaceAllString(s, "")
	s = mermaidHeadingOnlyRe.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// promptForAttempt returns the (system, user, maxTokens) to use at the given
// attempt number.
func (g *openaiGenerator) promptForAttempt(
	attempt int,
	digest, sourceCtx string,
	snippetCount int,
	lastReasons []string,
	lastRaw string,
) (system string, user string, maxTokens int) {
	switch {
	case attempt <= 2:
		// Stage 1: standard prompt.
		user = fmt.Sprintf(userPromptTemplate, snippetCount, digest, sourceCtx)
		return systemPrompt, user, g.cfg.MaxTokens

	case attempt <= 4:
		// Stage 2: repair prompt carrying prior validation failure reasons.
		reasonStr := strings.Join(lastReasons, "; ")
		if reasonStr == "" {
			reasonStr = "未通过质量校验"
		}
		prior := lastRaw
		if prior == "" {
			prior = "（无）"
		}
		user = fmt.Sprintf(repairUserPromptTemplate, reasonStr, prior, digest, sourceCtx)
		return repairSystemPrompt, user, g.cfg.MaxTokens

	default:
		// Stage 3: final strict last-chance prompt with doubled tokens.
		reasonStr := strings.Join(lastReasons, "; ")
		if reasonStr == "" {
			reasonStr = "前几次输出未能通过质量校验"
		}
		user = fmt.Sprintf(finalStrictUserPromptTemplate, reasonStr, digest, sourceCtx)
		return finalStrictSystemPrompt, user, g.cfg.MaxTokens * 2
	}
}

// chatComplete does a single POST to {BaseURL}/v1/chat/completions with a
// per-request context timeout of g.cfg.Timeout. Returns the assistant text
// on success or a non-nil error describing the transport / API failure.
func (g *openaiGenerator) chatComplete(parent context.Context, system, user string, maxTokens int) (string, error) {
	ctx, cancel := context.WithTimeout(parent, g.cfg.Timeout)
	defer cancel()

	reqBody := chatRequest{
		Model: g.cfg.Model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Temperature: g.cfg.Temperature,
		MaxTokens:   maxTokens,
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	url := strings.TrimRight(g.cfg.BaseURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return "", fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+g.cfg.APIKey)

	resp, err := g.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(body)
		if len(snippet) > 500 {
			snippet = snippet[:500] + "..."
		}
		return "", fmt.Errorf("openai http %d: %s", resp.StatusCode, snippet)
	}

	var cr chatResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return "", fmt.Errorf("unmarshal response: %w", err)
	}
	if cr.Error != nil && cr.Error.Message != "" {
		return "", fmt.Errorf("openai error: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return "", errors.New("openai: empty choices")
	}
	return cr.Choices[0].Message.Content, nil
}

// composeDigestMarkdown joins all IssueItems into a single markdown document
// for the LLM. Items are assumed to already be sorted by (section, seq) —
// compose() guarantees this.
func composeDigestMarkdown(items []*store.IssueItem) string {
	if len(items) == 0 {
		return "（本期无条目）"
	}
	var b strings.Builder
	currentSection := ""
	for _, it := range items {
		if it == nil {
			continue
		}
		if it.Section != currentSection {
			currentSection = it.Section
			b.WriteString("\n## ")
			b.WriteString(sectionDisplayName(it.Section))
			b.WriteString("\n\n")
		}
		b.WriteString("- **")
		b.WriteString(strings.TrimSpace(it.Title))
		b.WriteString("**\n")
		if body := strings.TrimSpace(it.BodyMD); body != "" {
			b.WriteString("  ")
			b.WriteString(strings.ReplaceAll(body, "\n", "\n  "))
			b.WriteString("\n")
		}
	}
	return strings.TrimSpace(b.String())
}

// sectionDisplayName maps internal section IDs to human-readable titles.
// Unknown sections are returned verbatim.
func sectionDisplayName(id string) string {
	switch id {
	case store.SectionProductUpdate:
		return "产品更新"
	case store.SectionResearch:
		return "研究进展"
	case store.SectionIndustry:
		return "行业动态"
	case store.SectionOpenSource:
		return "开源项目"
	case store.SectionSocial:
		return "社区声音"
	default:
		return id
	}
}

// composeSourceContext joins the raw items' bodies (title+content where
// available) into a single source-evidence string. Each item is capped at
// maxPerItem runes and the total is capped at maxItems entries to stay
// within a reasonable token budget.
//
// Returns (joined_text, num_snippets_actually_used).
func composeSourceContext(items []*store.RawItem) (string, int) {
	if len(items) == 0 {
		return "（无源链接原文）", 0
	}

	const maxItems = 10
	const maxPerItem = 800

	var b strings.Builder
	n := 0
	for _, it := range items {
		if it == nil {
			continue
		}
		if n >= maxItems {
			break
		}
		body := strings.TrimSpace(it.Content)
		if body == "" {
			body = strings.TrimSpace(it.Title)
		}
		if body == "" {
			continue
		}
		if len([]rune(body)) > maxPerItem {
			body = string([]rune(body)[:maxPerItem]) + "..."
		}
		fmt.Fprintf(&b, "[来源 %d] %s\n%s\n\n", n+1, it.URL, body)
		n++
	}
	if n == 0 {
		return "（无源链接原文）", 0
	}
	return strings.TrimSpace(b.String()), n
}
