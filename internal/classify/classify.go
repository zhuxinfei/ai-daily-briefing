// Package classify is the Step 2 classifier: given a list of RawItems
// already filtered by rank, assign each one to exactly one of the five
// briefing-v3 sections (product_update, research, industry, opensource,
// social).
//
// v1.0.0 strategy — rules first, LLM second:
//
//	1. Look up the item's source category (paper/project/community/blog/news).
//	2. If the category has a confident rule mapping (paper → research,
//	   project → opensource, community|blog → social), bucket directly.
//	3. Only news-category items fall through to an LLM batch call that
//	   decides product_update vs industry.
//	4. Anything the LLM misses lands in fallbackSection, which now uses
//	   URL host heuristics instead of dumping everything into social.
//
// The old 70% research skew came from the LLM being asked to classify
// every item into one of five sections when papers dominated the input —
// it produced plausible but lopsided verdicts. The rule-first split caps
// research at whatever fraction of the rank output is actually paper-source,
// which in practice is around 30-40% of 30 top-ranked items.
package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"briefing-v3/internal/store"
)

// Config parameterizes the classifier.
type Config struct {
	BaseURL    string
	APIKey     string
	Model      string
	BatchSize  int           // items per LLM request, default 25
	MaxRetries int           // per-batch retries, default 3
	Timeout    time.Duration // per-request timeout, default 120s
}

func (c *Config) fillDefaults() {
	if c.BatchSize <= 0 {
		c.BatchSize = 25
	}
	if c.MaxRetries <= 0 {
		c.MaxRetries = 3
	}
	if c.Timeout <= 0 {
		c.Timeout = 120 * time.Second
	}
}

// Classifier buckets RawItems into sections.
type Classifier interface {
	// Classify returns a map from section id (store.SectionProductUpdate
	// etc.) to the RawItems assigned to that section. Every non-nil
	// input item is guaranteed to appear in exactly one bucket.
	//
	// sourceCategories maps raw_items.source_id to the source's config
	// category (news / blog / paper / project / community). It is the
	// primary signal for the rule-based pre-bucketing pass. Items whose
	// source is not in the map (or whose category is "news") fall through
	// to the LLM for second-pass disambiguation into product_update vs
	// industry.
	Classify(
		ctx context.Context,
		items []*store.RawItem,
		sourceCategories map[int64]string,
	) (map[string][]*store.RawItem, error)
}

// New constructs an LLM-backed Classifier.
func New(cfg Config) (Classifier, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("classify: Config.BaseURL is required")
	}
	if cfg.APIKey == "" {
		return nil, errors.New("classify: Config.APIKey is required")
	}
	if cfg.Model == "" {
		return nil, errors.New("classify: Config.Model is required")
	}
	cfg.fillDefaults()
	return &llmClassifier{cfg: cfg, hc: &http.Client{}}, nil
}

// classifySystemPrompt is the rubric the LLM follows. v1.0.0 originally
// narrowed the LLM's job to a strict binary for news items, but实战中这会把
// “研究类新闻”错误压到 industry / product_update。当前 prompt 改为三分类:
// news item 只能被分到 product_update / research / industry。
const classifySystemPrompt = `你是 AI 日报编辑。你收到的每一条都来自新闻类来源 (news category)。
你的任务：把每一条分到且仅分到以下 3 个 section 之一：
product_update / research / industry

- product_update: AI 公司或产品的具体发布、更新、新功能、新版本、发布会、开发者工具、API、模型
  (典型标志: OpenAI/Anthropic/Google/DeepMind/Meta/xAI/DeepSeek/Mistral/NVIDIA/Microsoft/Apple/
  AWS/字节/阿里/腾讯/华为/智谱/月之暗面/MiniMax 等公司推出的任何具体产品/模型/工具/API/功能)
- research: 研究结果、基准测试、论文、模型架构、训练方法、数据集、实验结论的报道
  (典型标志: 研究显示/研究发现/新论文/基准测试/benchmark/study/paper/arxiv/数据集/评测/
  架构/训练方法/实验结果/性能下降/system prompt/工作流理解/图表理解)
- industry: 行业趋势、政策、监管、融资并购、人事变动、观点评论、社会影响、市场分析、调查报告
  (典型标志: 融资/监管/诉讼/收购/政策/观点/评论/报告/调研/访谈/专访/行业白皮书/人事变动)

判断要点：
1. 标题/内容含"发布/推出/上线/开源/新增/新功能/新模型/new model/release/launch/ships/unveil/
   available/announce/rollout/beta/preview/API/SDK" 等字眼且指向具体产品 → product_update
2. 标题/内容核心在“研究发现了什么 / 基准测出了什么 / 论文提出了什么方法” → research
3. 标题/内容含"融资/投资/估值/监管/政策/诉讼/收购/并购/合作/观点/评论/分析/报告/调研/趋势/访谈"
   等字眼，且不是具体研究成果或产品发布 → industry
4. 描述某家 AI 公司有新能力、新产品、新工具的，一律 product_update
5. 不确定时：
   - 有明确发布动作 → product_update
   - 有明确研究/评测/论文结果 → research
   - 其余 → industry

只输出严格 JSON 数组 (不要输出其他任何文字):
[{"id": 原 id, "section": "product_update" 或 "research" 或 "industry"}, ...]`

// classifyUserPromptTemplate is the per-batch user message.
const classifyUserPromptTemplate = `以下是新闻类候选条目，请只在 product_update / research / industry 里三选一：

%s

只输出 JSON 数组，不要输出其他文字。`

// validSections is the allowlist that LLM output must fall into.
var validSections = map[string]bool{
	store.SectionProductUpdate: true,
	store.SectionResearch:      true,
	store.SectionIndustry:      true,
	store.SectionOpenSource:    true,
	store.SectionSocial:        true,
}

// llmClassifier is the concrete Classifier implementation.
type llmClassifier struct {
	cfg Config
	hc  *http.Client
}

// chatMessage / chatRequest / chatResponse duplicate the minimal OpenAI
// chat-completions structs; kept local so classify has no build-time
// dependency on the generate package.
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

// classifyVerdict is one element of the LLM-emitted JSON array.
type classifyVerdict struct {
	ID      int64  `json:"id"`
	Section string `json:"section"`
}

// Classify walks items in two passes:
//
//  1. Rule pass: look up sourceCategories[it.SourceID] and ask
//     ruleClassifyByCategory for a confident section verdict. Items
//     whose source category is paper / project / community / blog get
//     bucketed immediately without any LLM call.
//  2. LLM pass: any item that the rule pass declined (news category,
//     or unknown source) gets batched to the LLM with a binary prompt
//     that can only emit product_update or industry. Batch failures
//     and missing verdicts drop through to fallbackSection.
//
// The guarantee is unchanged from v0: every non-nil input item appears
// in exactly one bucket in the returned map.
func (c *llmClassifier) Classify(
	ctx context.Context,
	items []*store.RawItem,
	sourceCategories map[int64]string,
) (map[string][]*store.RawItem, error) {
	result := map[string][]*store.RawItem{
		store.SectionProductUpdate: nil,
		store.SectionResearch:      nil,
		store.SectionIndustry:      nil,
		store.SectionOpenSource:    nil,
		store.SectionSocial:        nil,
	}
	if len(items) == 0 {
		return result, nil
	}

	byID := make(map[int64]*store.RawItem, len(items))
	for _, it := range items {
		if it == nil {
			continue
		}
		byID[it.ID] = it
	}

	// Pass 1 — rule pre-classify. Any item whose source category maps
	// to a confident section verdict is placed directly; ambiguous items
	// (news, or unknown source) accumulate in the LLM queue.
	assigned := make(map[int64]string, len(items))
	var ambiguous []*store.RawItem
	for _, it := range items {
		if it == nil {
			continue
		}
		srcCat := ""
		if sourceCategories != nil {
			srcCat = sourceCategories[it.SourceID]
		}
		sec, confident := ruleClassifyByCategory(srcCat, it.URL, it.Title)
		if confident {
			assigned[it.ID] = sec
			continue
		}
		ambiguous = append(ambiguous, it)
	}

	// Pass 2 — LLM binary disambiguation for the ambiguous bucket only.
	// The LLM is a strict product_update / industry classifier here; any
	// other verdict is ignored and the item falls through to fallback.
	for start := 0; start < len(ambiguous); start += c.cfg.BatchSize {
		end := start + c.cfg.BatchSize
		if end > len(ambiguous) {
			end = len(ambiguous)
		}
		batch := ambiguous[start:end]

		verdicts, err := c.classifyBatchWithRetry(ctx, batch)
		if err != nil {
			// Batch failed outright; leave these items unassigned so the
			// fallback below picks them up.
			continue
		}
		for _, v := range verdicts {
			if _, ok := byID[v.ID]; !ok {
				continue
			}
			// The prompt should only emit these three, but we defend
			// in depth against the LLM hallucinating other sections.
			if v.Section != store.SectionProductUpdate &&
				v.Section != store.SectionResearch &&
				v.Section != store.SectionIndustry {
				continue
			}
			assigned[v.ID] = v.Section
		}
	}

	// Pass 3 — bucket assigned items, fallback-classify unassigned ones.
	for id, it := range byID {
		sec, ok := assigned[id]
		if !ok {
			sec = fallbackSection(it)
		}
		result[sec] = append(result[sec], it)
	}

	return result, nil
}

// ruleClassifyByCategory is the deterministic pre-classifier for
// v1.0.0. It maps source.category (from config/ai.yaml) plus some URL
// hints into a section verdict. Returns (section, true) when the rule
// is confident — caller should NOT re-ask the LLM. Returns ("", false)
// when the item needs LLM help (i.e., it is a news article where the
// product_update vs industry split cannot be derived from metadata
// alone).
//
// Mapping:
//
//	paper      → research      (confident)
//	project    → opensource    (confident)
//	community  → social        (confident)
//	blog       → social        (confident, blogs are commentary not news)
//	news       → ("", false)   (ambiguous — LLM must decide)
//	anything else → ("", false) (unknown — LLM fallback)
//
// A couple of URL-level overrides run before the category lookup so that
// (e.g.) a blog post that links to arxiv.org still lands in research,
// not social.
func ruleClassifyByCategory(srcCategory, urlStr, title string) (string, bool) {
	lowerURL := strings.ToLower(urlStr)
	lowerTitle := strings.ToLower(title)

	// High-confidence URL overrides beat the source category. These
	// catch cases where a blog or news source happens to link to
	// canonical research / open-source destinations.
	switch {
	case strings.Contains(lowerURL, "arxiv.org"),
		strings.Contains(lowerURL, "huggingface.co/papers"),
		strings.Contains(lowerURL, "papers.cool"),
		strings.Contains(lowerURL, "openreview.net"):
		return store.SectionResearch, true
	case strings.Contains(lowerURL, "github.com/"),
		strings.Contains(lowerURL, "gitlab.com/"),
		strings.Contains(lowerURL, "ossinsight.io"):
		return store.SectionOpenSource, true
	}

	switch strings.ToLower(strings.TrimSpace(srcCategory)) {
	case "paper":
		return store.SectionResearch, true
	case "project":
		return store.SectionOpenSource, true
	case "community":
		return store.SectionSocial, true
	case "blog":
		if looksResearchLikeText(lowerTitle + " " + lowerURL) {
			return store.SectionResearch, true
		}
		return store.SectionSocial, true
	case "news":
		// news is intentionally ambiguous — the LLM makes the
		// product_update vs industry call.
		return "", false
	default:
		// Unknown or missing category — let the LLM try. If the LLM
		// also fails, fallbackSection will take over.
		return "", false
	}
}

// classifyBatchWithRetry calls the LLM up to MaxRetries times for a batch
// and returns the first parseable verdict slice.
func (c *llmClassifier) classifyBatchWithRetry(ctx context.Context, batch []*store.RawItem) ([]classifyVerdict, error) {
	userPrompt := fmt.Sprintf(classifyUserPromptTemplate, formatItemsForClassify(batch))

	var lastErr error
	for attempt := 1; attempt <= c.cfg.MaxRetries; attempt++ {
		raw, err := c.chatComplete(ctx, classifySystemPrompt, userPrompt)
		if err != nil {
			lastErr = err
			continue
		}
		verdicts, perr := parseClassifyJSON(raw)
		if perr != nil {
			lastErr = perr
			continue
		}
		return verdicts, nil
	}
	if lastErr == nil {
		lastErr = errors.New("classify: batch failed with no specific error")
	}
	return nil, lastErr
}

// formatItemsForClassify is the per-batch item renderer.
func formatItemsForClassify(batch []*store.RawItem) string {
	var b strings.Builder
	for _, it := range batch {
		if it == nil {
			continue
		}
		desc := firstRunes(strings.TrimSpace(it.Content), 80)
		if desc == "" {
			desc = "(no description)"
		}
		fmt.Fprintf(&b, "[id=%d] %s | %s | %s\n",
			it.ID,
			truncateOneLine(it.Title, 140),
			it.URL,
			truncateOneLine(desc, 160),
		)
	}
	return b.String()
}

// parseClassifyJSON unwraps the LLM response into a []classifyVerdict.
func parseClassifyJSON(raw string) ([]classifyVerdict, error) {
	s := extractJSONArray(raw)
	if s == "" {
		return nil, fmt.Errorf("classify: no JSON array found: %q", truncateOneLine(raw, 200))
	}
	var verdicts []classifyVerdict
	if err := json.Unmarshal([]byte(s), &verdicts); err != nil {
		return nil, fmt.Errorf("classify: parse JSON: %w", err)
	}
	return verdicts, nil
}

// fallbackSection is the last-resort rule-based classifier used when
// both the category rule and the LLM decline to place an item. It
// inspects URL host and title keywords to pick the most plausible
// bucket. v1.0.0 tightens the default: instead of dumping "unknown"
// items into social (which historically starved the research /
// opensource sections), the catch-all now uses title keywords to
// split between product_update and industry — because by the time we
// reach this function the item is almost always a news article that
// the LLM failed to decide.
func fallbackSection(it *store.RawItem) string {
	url := strings.ToLower(it.URL)
	title := strings.ToLower(it.Title)

	switch {
	case strings.Contains(url, "arxiv.org"),
		strings.Contains(url, "huggingface.co/papers"),
		strings.Contains(url, "papers.cool"),
		strings.Contains(url, "openreview.net"):
		return store.SectionResearch

	case strings.Contains(url, "github.com"),
		strings.Contains(url, "ossinsight.io"),
		strings.Contains(url, "gitlab.com"):
		return store.SectionOpenSource

	case strings.Contains(url, "reddit.com"),
		strings.Contains(url, "ycombinator.com"),
		strings.Contains(url, "news.ycombinator"),
		strings.Contains(url, "hacker-news"),
		strings.Contains(url, "simonwillison.net"),
		strings.Contains(url, "oneusefulthing.org"),
		strings.Contains(url, "jack-clark.net"),
		strings.Contains(url, "sebastianraschka"),
		strings.Contains(url, "lilianweng"),
		strings.Contains(url, "baoyu.io"),
		strings.Contains(url, "ruanyifeng.com"):
		return store.SectionSocial

	case strings.Contains(url, "openai.com"),
		strings.Contains(url, "anthropic.com"),
		strings.Contains(url, "deepmind.google"),
		strings.Contains(url, "meta.ai"),
		strings.Contains(url, "ai.meta.com"),
		strings.Contains(url, "deepseek.com"),
		strings.Contains(url, "mistral.ai"),
		strings.Contains(url, "x.ai"):
		return store.SectionProductUpdate
	}

	if looksResearchLikeText(title + " " + url) {
		return store.SectionResearch
	}

	// Title keyword heuristic. News-style URLs (techcrunch, the-decoder,
	// smol.ai, google news, ...) drop through to here; pick product_update
	// vs industry by simple keyword matching so that one fallback-heavy
	// day does not pile everything into a single bucket.
	productKeywords := []string{
		"发布", "推出", "上线", "发布会", "新版本", "新模型", "新功能",
		"release", "launch", "launches", "unveil", "ships", "ship",
		"announce", "announces", "available", "availability", "rolls out",
		"beta", "preview",
	}
	industryKeywords := []string{
		"融资", "监管", "政策", "诉讼", "收购", "并购", "报告", "调查",
		"观点", "评论", "分析", "趋势",
		"funding", "raise", "raised", "regulation", "policy", "lawsuit",
		"acquisition", "acquires", "report", "survey", "analysis",
		"opinion", "perspective", "interview",
	}

	for _, kw := range productKeywords {
		if strings.Contains(title, kw) {
			return store.SectionProductUpdate
		}
	}
	for _, kw := range industryKeywords {
		if strings.Contains(title, kw) {
			return store.SectionIndustry
		}
	}

	// Still unknown — prefer industry over social so news-category
	// items default to the news-shaped section.
	return store.SectionIndustry
}

func looksResearchLikeText(text string) bool {
	s := strings.ToLower(strings.TrimSpace(text))
	researchKeywords := []string{
		"benchmark", "study", "research", "paper", "papers", "arxiv",
		"openreview", "dataset", "evaluation", "experiment", "architecture",
		"training method", "scientific", "workflow for understanding llms",
		"system prompt", "performance when", "new benchmark", "new study",
		"研究", "论文", "基准", "评测", "数据集", "实验", "架构", "训练方法", "研究显示",
	}
	industryBlockers := []string{
		"融资", "ipo", "ceo", "股市", "上市", "估值", "广告", "用户", "资本", "并购",
		"lawsuit", "funding", "startup", "advertiser", "market", "policy", "regulation",
	}
	for _, bad := range industryBlockers {
		if strings.Contains(s, bad) {
			return false
		}
	}
	for _, kw := range researchKeywords {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// extractJSONArray / firstRunes / truncateOneLine are duplicated from
// rank.go to keep classify self-contained.

func extractJSONArray(s string) string {
	start := strings.Index(s, "[")
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if esc {
			esc = false
			continue
		}
		if c == '\\' && inStr {
			esc = true
			continue
		}
		if c == '"' {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		switch c {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

func firstRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}

func truncateOneLine(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.TrimSpace(s)
	return firstRunes(s, n)
}

// chatComplete POSTs a single chat-completions request. Identical shape
// to rank.go / openai.go but local to keep package boundaries clean.
func (c *llmClassifier) chatComplete(parent context.Context, system, user string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, c.cfg.Timeout)
	defer cancel()

	reqBody := chatRequest{
		Model: c.cfg.Model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Temperature: 0,
		MaxTokens:   2000,
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("classify marshal: %w", err)
	}

	url := strings.TrimRight(c.cfg.BaseURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return "", fmt.Errorf("classify new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("classify http do: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("classify read body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(body)
		if len(snippet) > 500 {
			snippet = snippet[:500] + "..."
		}
		return "", fmt.Errorf("classify openai http %d: %s", resp.StatusCode, snippet)
	}

	var cr chatResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return "", fmt.Errorf("classify unmarshal response: %w", err)
	}
	if cr.Error != nil && cr.Error.Message != "" {
		return "", fmt.Errorf("classify openai error: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return "", errors.New("classify openai: empty choices")
	}
	return cr.Choices[0].Message.Content, nil
}
