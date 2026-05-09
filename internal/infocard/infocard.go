// Package infocard asks the LLM to distill every IssueItem into a
// structured info-card JSON that a downstream PIL renderer turns into
// an editorial infographic (米黄报纸底 + 杂志排版).
//
// One LLM call per run generates info-card JSON for ALL items at once,
// which keeps token costs linear (not per-item). The prompt is hard-
// locked to return a JSON array so the parser does not have to do
// fancy natural-language extraction.
//
// Package layout mirrors internal/rank: standalone, minimal deps, its
// own tiny HTTP client. Does not import generate/ or rank/.
package infocard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"briefing-v3/internal/llm"
	"briefing-v3/internal/store"
)

// Config parameterizes the LLM client used by the info-card pass.
type Config struct {
	BaseURL    string
	APIKey     string
	Model      string
	MaxRetries int
	Timeout    time.Duration
}

func (c *Config) fillDefaults() {
	if c.MaxRetries <= 0 {
		c.MaxRetries = 3
	}
	if c.Timeout <= 0 {
		c.Timeout = 120 * time.Second
	}
}

// Card is the structured info-card payload for a single IssueItem,
// exactly the shape the Python PIL template consumes. Every field is
// optional — the renderer handles empty values gracefully, but the
// LLM prompt asks it to fill them all.
type Card struct {
	ItemSeq        int      `json:"item_seq"`
	MainTitle      string   `json:"main_title"`
	Subtitle       string   `json:"subtitle"`
	Intro          string   `json:"intro"`
	HeroNumber     string   `json:"hero_number"`
	HeroLabel      string   `json:"hero_label"`
	StatNumbers    []Stat   `json:"stat_numbers"`
	KeyPoints      []Point  `json:"key_points"`
	FooterSummary  string   `json:"footer_summary"`
	BrandTag       string   `json:"brand_tag"`
	CategoryTag    string   `json:"category_tag"`
}

type Stat struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type Point struct {
	Title string `json:"title"`
	Desc  string `json:"desc"`
}

// HeaderCard is the once-per-issue "大字报" front-page payload. The
// PIL renderer turns it into the hero banner shown at the top of the
// HTML page. It is structurally similar to Card but the semantics
// are different: the hero banner summarises the WHOLE issue, whereas
// Card summarises one news item.
//
// v1.0.0 急救：增加 Edition / LeadParagraph / KeyNumbers 三个字段并把
// TopStories 从 3 条扩到 6 条，让 1600x1600 画布有足够内容把空间塞满，
// 解决"大字报内容太空泛 + 大量留白"的视觉问题。
type HeaderCard struct {
	IssueDate     string     `json:"issue_date"`
	Edition       string     `json:"edition"`        // 新：期号 / 短文 (例 "v1.0.0 · 第 1 期")
	MainHeadline  string     `json:"main_headline"`
	SubHeadline   string     `json:"sub_headline"`
	LeadParagraph string     `json:"lead_paragraph"` // 新：导语段 100-160 字
	KeyNumbers    []KeyNum   `json:"key_numbers"`    // 新：3 条 突出数字 + 标签
	TopStories    []TopStory `json:"top_stories"`
	FooterSlogan  string     `json:"footer_slogan"`
}

// KeyNum is one big-typography number cell on the hero banner: a value
// (the eye-catching figure or short phrase) plus an 8-12 char label
// explaining what it means. v1.0.0 急救新增。
type KeyNum struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type TopStory struct {
	Title string `json:"title"`
	Tag   string `json:"tag"`
}

// Generator is the public interface of this package.
type Generator interface {
	// Generate produces a card for every item plus a single page-wide
	// header card. items must be non-empty.
	Generate(ctx context.Context, items []*store.IssueItem, summary string) (*HeaderCard, []*Card, error)
}

// New builds an LLM-backed Generator.
func New(cfg Config) (Generator, error) {
	if cfg.BaseURL == "" || cfg.APIKey == "" || cfg.Model == "" {
		return nil, errors.New("infocard: BaseURL / APIKey / Model are required")
	}
	cfg.fillDefaults()
	return &llmGenerator{cfg: cfg, hc: &http.Client{}}, nil
}

type llmGenerator struct {
	cfg Config
	hc  *http.Client
}

const infoCardSystemPrompt = `你是一名 AI 日报视觉编辑, 负责把每条新闻提炼成"信息卡片"的结构化要点, 供下游的报纸风格信息图模板渲染使用。

你会收到:
1) 今日全部新闻条目 (每条有 seq、section、title、body)
2) 今日早报整体摘要 (3 行)

你必须输出一段严格 JSON, 形如:
{
  "header": {
    "edition": "期号短文, 例 'v1.0.0 · 第 1 期 · 早八更新' (12-20 字)",
    "main_headline": "整期早报一句话大字报标题, 标题党风格, 必须点出最大事件 (20-32 字)",
    "sub_headline": "整期早报副标题, 补充语境与角度 (18-32 字)",
    "lead_paragraph": "新闻第一段导语, 把今天 3-4 件最重要的事用一段话讲清楚, 像报纸 lead, 语气连贯, 不分点 (100-160 字)",
    "key_numbers": [
      {"value": "突出数字或短语 (例 '$0.08/h' / '5.4' / '12 家')", "label": "解释这个数字, 6-12 字"},
      {"value": "...", "label": "..."},
      {"value": "...", "label": "..."}
    ],
    "top_stories": [
      {"title": "重磅新闻标题党短句 (12-20 字)", "tag": "产品/研究/开源/行业/社媒 中之一"},
      {"title": "...", "tag": "..."},
      {"title": "...", "tag": "..."},
      {"title": "...", "tag": "..."},
      {"title": "...", "tag": "..."},
      {"title": "...", "tag": "..."}
    ],
    "footer_slogan": "一句品牌口号, 8-14 字"
  },
  "cards": [
    {
      "item_seq": 1,
      "main_title": "一句话提炼的产品/技术名称 10-15 字",
      "subtitle": "一句话补充卖点 15-25 字",
      "intro": "导语段落 40-80 字, 用口语化中文解释这条新闻为什么重要",
      "hero_number": "最核心的一个数字或关键词 (例 '68%' / '4.6' / '3 秒' / '750B')",
      "hero_label": "这个数字代表什么, 4-8 字",
      "stat_numbers": [
        {"value": "次要数据", "label": "解释"},
        {"value": "次要数据", "label": "解释"}
      ],
      "key_points": [
        {"title": "要点 1 小标题 4-8 字", "desc": "一句话 15-30 字"},
        {"title": "要点 2", "desc": "..."},
        {"title": "要点 3", "desc": "..."}
      ],
      "footer_summary": "底部一行总结 20-30 字",
      "brand_tag": "item 所属 section 中文",
      "category_tag": "新闻类别的 1-2 个英文标签 (如 'AGENT' / 'MODEL' / 'OPENSOURCE')"
    },
    ... 每一条 item 一条 card ...
  ]
}

硬规则:
- 必须严格 JSON (无注释、无多余 markdown 围栏)
- cards 数组的长度必须等于输入 items 的数量
- 每个 card 的 item_seq 必须和输入 item 的 seq 匹配
- 所有数字字段 (hero_number/stat_numbers.value) 必须来自原文, 不得捏造
- 如果某条新闻天然缺少明显数字, hero_number 用一个关键短语 (例 "安全下线" / "首次开源"), 不要填 "N/A"
- 专业术语加括号注释 (非技术用户友好)
- top_stories 输出 11-14 条 (不是示例中的 6 条)
- key_numbers 输出 4-6 条 (不是示例中的 3 条)
- sub_headline 可多行, 用 \n 分隔不同角度的副标题 (每行 18-32 字)
- 不要输出任何 JSON 以外的文字
`

// Generate implements Generator.Generate.
func (g *llmGenerator) Generate(ctx context.Context, items []*store.IssueItem, summary string) (*HeaderCard, []*Card, error) {
	if len(items) == 0 {
		return nil, nil, errors.New("infocard: no items")
	}

	userPrompt := buildInfoCardUserPrompt(items, summary)

	llmCfg := llm.Config{
		BaseURL: g.cfg.BaseURL, APIKey: g.cfg.APIKey, Model: g.cfg.Model,
		Temperature: 0.3, MaxTokens: 8192, Timeout: g.cfg.Timeout,
	}
	var lastErr error
	for attempt := 1; attempt <= g.cfg.MaxRetries; attempt++ {
		raw, err := llm.ChatComplete(ctx, g.hc, llmCfg, infoCardSystemPrompt, userPrompt)
		if err != nil {
			lastErr = err
			continue
		}
		header, cards, perr := parseInfoCardJSON(raw, items)
		if perr != nil {
			lastErr = perr
			continue
		}
		return header, cards, nil
	}
	if lastErr == nil {
		lastErr = errors.New("infocard: failed with no specific error")
	}
	// v1.0.1 Phase 4.5 (T15): LLM 全部 retry 失败 → 用 rule-based fallback 拼
	// HeaderCard + Cards JSON, 让 PIL 渲染照常出 PNG. 用户原则: 大字报必出.
	// 朴素但版式完整, 优于"infocard 失败 → 整个 image 阶段失败 → gate 怒火".
	log.Printf("[WARN] infocard: LLM 全部 retry 失败 (%v), 启用 rule-based fallback 保大字报", lastErr)
	header := ruleBasedHeader(items, summary)
	cards := ruleBasedCards(items)
	return header, cards, nil
}

// ruleBasedHeader 用 issue items + summary 规则化拼 HeaderCard, 不调 LLM.
// 字段都填非空 default, 让 gen_info_card.py 渲染时不出现"空字段大留白".
func ruleBasedHeader(items []*store.IssueItem, summary string) *HeaderCard {
	summaryLines := splitNonBlankLines(summary)
	mainHeadline := truncateRunes(firstOr(summaryLines, "今日 AI 早报"), 28)
	subHeadline := ""
	if len(summaryLines) >= 2 {
		subHeadline = truncateRunes(summaryLines[1], 30)
		if len(summaryLines) >= 3 {
			subHeadline = subHeadline + "\n" + truncateRunes(summaryLines[2], 30)
		}
	}

	// 导语段 100-160 字: 取前 5 个 item title 用句号连接.
	var leadParts []string
	for i, it := range items {
		if i >= 5 || it == nil {
			break
		}
		leadParts = append(leadParts, strings.TrimSpace(it.Title))
	}
	leadParagraph := truncateRunes(strings.Join(leadParts, "; "), 160)
	if leadParagraph == "" {
		leadParagraph = "今日 AI 行业动态汇总, 详见正文."
	}

	// section 统计.
	sectionSet := make(map[string]bool)
	for _, it := range items {
		if it != nil {
			sectionSet[it.Section] = true
		}
	}

	// top_stories: 取前 14 条 (或不足时全部), 用 section 中文名做 tag.
	stories := make([]TopStory, 0, 14)
	for _, it := range items {
		if len(stories) >= 14 || it == nil {
			break
		}
		stories = append(stories, TopStory{
			Title: truncateRunes(strings.TrimSpace(it.Title), 30),
			Tag:   sectionToTagZH(it.Section),
		})
	}

	return &HeaderCard{
		Edition:       "briefing-v3 · 每日 AI 早报",
		MainHeadline:  mainHeadline,
		SubHeadline:   subHeadline,
		LeadParagraph: leadParagraph,
		KeyNumbers: []KeyNum{
			{Value: fmt.Sprintf("%d 条", len(items)), Label: "今日精选条目"},
			{Value: fmt.Sprintf("%d 个", len(sectionSet)), Label: "覆盖板块"},
			{Value: "全网", Label: "深度聚合"},
		},
		TopStories:   stories,
		FooterSlogan: "briefing-v3 · 每日 AI 早读",
	}
}

// ruleBasedCards 用 issue items 规则化拼 12 张 Card, 不调 LLM.
func ruleBasedCards(items []*store.IssueItem) []*Card {
	cards := make([]*Card, 0, len(items))
	for i, it := range items {
		if it == nil {
			continue
		}
		body := stripMarkdownNoise(it.BodyMD)
		runes := []rune(body)
		subtitle := ""
		intro := ""
		footer := ""
		if len(runes) > 0 {
			end := 30
			if end > len(runes) {
				end = len(runes)
			}
			subtitle = string(runes[:end])
		}
		if len(runes) > 30 {
			end := 100
			if end > len(runes) {
				end = len(runes)
			}
			intro = string(runes[30:end])
		}
		if len(runes) > 100 {
			end := 160
			if end > len(runes) {
				end = len(runes)
			}
			footer = string(runes[100:end])
		}
		hero, heroLabel := extractHeroNumber(body)
		brand := sectionToTagZH(it.Section)
		category := sectionToTagEN(it.Section)
		cards = append(cards, &Card{
			ItemSeq:       it.Seq,
			MainTitle:     truncateRunes(strings.TrimSpace(it.Title), 18),
			Subtitle:      truncateRunes(subtitle, 25),
			Intro:         truncateRunes(intro, 80),
			HeroNumber:    hero,
			HeroLabel:     heroLabel,
			StatNumbers:   nil,
			KeyPoints:     buildPoints(body),
			FooterSummary: truncateRunes(footer, 30),
			BrandTag:      brand,
			CategoryTag:   category,
		})
		if i+1 >= 12 {
			break
		}
	}
	return cards
}

// 辅助函数 ----------------------------------------------------------------

var ruleHeroNumRe = regexp.MustCompile(`(\d{1,6}(?:\.\d+)?[万亿%]?|\$\d+(?:\.\d+)?[BMK]?|\d{1,6}\s*(?:倍|条|个|分|秒|min|小时|天))`)

func extractHeroNumber(body string) (string, string) {
	if m := ruleHeroNumRe.FindString(body); m != "" {
		return truncateRunes(m, 8), "关键数据"
	}
	return "重磅", "今日要闻"
}

var rulePointSplitRe = regexp.MustCompile(`[。；;\n]`)

func buildPoints(body string) []Point {
	parts := rulePointSplitRe.Split(body, -1)
	points := make([]Point, 0, 3)
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t == "" {
			continue
		}
		runes := []rune(t)
		titleEnd := 8
		if titleEnd > len(runes) {
			titleEnd = len(runes)
		}
		points = append(points, Point{
			Title: string(runes[:titleEnd]),
			Desc:  truncateRunes(t, 30),
		})
		if len(points) >= 3 {
			break
		}
	}
	if len(points) == 0 {
		points = append(points, Point{Title: "今日要闻", Desc: "详见正文"})
	}
	return points
}

var ruleNoiseRe = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)|\[[^\]]*\]\([^)]*\)|[*_` + "`" + `>#]+`)

func stripMarkdownNoise(s string) string {
	s = ruleNoiseRe.ReplaceAllString(s, " ")
	s = regexp.MustCompile(`\s+`).ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func splitNonBlankLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		t := strings.TrimSpace(l)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

func firstOr(lines []string, dflt string) string {
	if len(lines) == 0 {
		return dflt
	}
	return lines[0]
}

func truncateRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}

func sectionToTagZH(secID string) string {
	switch secID {
	case "product_update":
		return "产品"
	case "research":
		return "研究"
	case "industry":
		return "行业"
	case "opensource":
		return "开源"
	case "social":
		return "社媒"
	}
	return "新闻"
}

func sectionToTagEN(secID string) string {
	switch secID {
	case "product_update":
		return "PRODUCT"
	case "research":
		return "RESEARCH"
	case "industry":
		return "INDUSTRY"
	case "opensource":
		return "OPENSOURCE"
	case "social":
		return "SOCIAL"
	}
	return "AI"
}

// buildInfoCardUserPrompt serializes the items for the LLM user turn.
// We include the section tag, title and a truncated body so the LLM
// has enough context to extract specific numbers and key points.
func buildInfoCardUserPrompt(items []*store.IssueItem, summary string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "【今日整体摘要】\n%s\n\n", strings.TrimSpace(summary))
	b.WriteString("【全部候选新闻】(共 ")
	fmt.Fprintf(&b, "%d 条)\n\n", len(items))
	for _, it := range items {
		if it == nil {
			continue
		}
		title := strings.TrimSpace(it.Title)
		body := strings.TrimSpace(it.BodyMD)
		if n := len([]rune(body)); n > 500 {
			body = string([]rune(body)[:500]) + "……"
		}
		fmt.Fprintf(&b, "=== seq=%d | section=%s ===\n", it.Seq, it.Section)
		fmt.Fprintf(&b, "标题: %s\n", title)
		if body != "" {
			fmt.Fprintf(&b, "内容: %s\n", body)
		}
		b.WriteString("\n")
	}
	b.WriteString("请严格按 system message 要求输出 JSON。")
	return b.String()
}

// parseInfoCardJSON handles the common cases: raw JSON, JSON wrapped
// in a ```json ... ``` fence, or JSON with surrounding prose. Uses a
// brace counter to find the outermost object.
func parseInfoCardJSON(raw string, items []*store.IssueItem) (*HeaderCard, []*Card, error) {
	raw = strings.TrimSpace(raw)
	// Strip common markdown fences.
	raw = fencedJSONRe.ReplaceAllString(raw, "$1")
	raw = strings.TrimSpace(raw)

	// If still not a bare JSON object, extract the outermost braces.
	if !strings.HasPrefix(raw, "{") {
		start := strings.Index(raw, "{")
		end := strings.LastIndex(raw, "}")
		if start >= 0 && end > start {
			raw = raw[start : end+1]
		}
	}

	var wrapper struct {
		Header HeaderCard `json:"header"`
		Cards  []*Card    `json:"cards"`
	}
	if err := json.Unmarshal([]byte(raw), &wrapper); err != nil {
		return nil, nil, fmt.Errorf("infocard: parse json: %w", err)
	}
	if len(wrapper.Cards) == 0 {
		return nil, nil, errors.New("infocard: empty cards array")
	}
	// Backfill item_seq when the LLM lazily omits it by matching the
	// index order against the input items.
	if len(wrapper.Cards) == len(items) {
		for i, c := range wrapper.Cards {
			if c.ItemSeq == 0 && items[i] != nil {
				c.ItemSeq = items[i].Seq
			}
		}
	}
	// Return a pointer header because the caller mutates it later
	// (to set IssueDate). Convert the value to a pointer.
	h := wrapper.Header
	return &h, wrapper.Cards, nil
}

var fencedJSONRe = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*\\})\\s*```")

// LLM HTTP client moved to internal/llm/client.go
