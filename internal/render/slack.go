// Package render — Slack Block Kit payload builder.
//
// This file converts a publish.RenderedIssue into a Slack-ready JSON
// payload. The block layout mirrors scripts/slack-notify.js in the
// legacy AI-Insight-Daily repo so existing user expectations carry over,
// but the source of truth is the structured RenderedIssue rather than a
// re-parsed markdown string. The key additions for v3 are:
//
//   - a headline image block at the top (when HeadlineImageURL is set)
//   - one block per section with truncated markdown, so readers get a
//     real preview of the day's content inside Slack instead of only
//     the insight lines.
package render

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"briefing-v3/internal/publish"
)

// BuildSlackPayload returns the Slack webhook JSON body for rendered.
// It returns an error only if JSON marshalling fails, which should be
// impossible for the simple map/slice structure it builds.
func BuildSlackPayload(rendered *publish.RenderedIssue) ([]byte, error) {
	payload := buildSlackPayloadMap(rendered)
	return json.Marshal(payload)
}

// buildSlackPayloadMap is the testable counterpart of BuildSlackPayload
// that returns the raw map. Kept private; tests can call it via the
// package if needed.
func buildSlackPayloadMap(rendered *publish.RenderedIssue) map[string]any {
	blocks := make([]map[string]any, 0, 24)

	if rendered == nil || rendered.Issue == nil {
		return map[string]any{"blocks": blocks}
	}
	issue := rendered.Issue
	dateStr := issue.IssueDate.Format("2006-01-02")

	chineseDate := rendered.DateZH
	if chineseDate == "" {
		chineseDate = FormatDateZH(issue)
	}

	// 1. Headline image block (optional).
	if strings.TrimSpace(rendered.HeadlineImageURL) != "" {
		blocks = append(blocks, map[string]any{
			"type":      "image",
			"image_url": rendered.HeadlineImageURL,
			"alt_text":  fmt.Sprintf("briefing-v3 %s 头版", dateStr),
		})
	}

	// 2. Header block. v1.0.0+: 之前 gate warn 时会前缀 "🟡 质量待审 | "
	// 但用户反馈"切正式频道时这个标签不应出现", 一次性删除. 测试 + 正式
	// 频道 header 都干净, 内部 quality warn 仍由 gate + journal 记录.
	headerText := fmt.Sprintf("🤖 AI 资讯日报 - %s", chineseDate)
	blocks = append(blocks, map[string]any{
		"type": "header",
		"text": map[string]any{
			"type":  "plain_text",
			"text":  headerText,
			"emoji": true,
		},
	})

	// 3. Industry insight + 4. divider + 5. Our takeaways + 6. divider.
	if rendered.Insight != nil {
		// Strip mermaid blocks from Slack text (rendered as image separately).
		industryMD := strings.TrimSpace(StripMermaidBlocks(rendered.Insight.IndustryMD))
		ourMD := strings.TrimSpace(StripMermaidBlocks(rendered.Insight.OurMD))
		if industryMD != "" {
			n := countSlackNumberedItems(industryMD)
			blocks = append(blocks, map[string]any{
				"type": "section",
				"text": map[string]any{
					"type": "mrkdwn",
					"text": fmt.Sprintf("*📊 行业洞察（今日 %d 条）*\n\n%s",
						n, ConvertToSlackMrkdwn(industryMD)),
				},
			})
			blocks = append(blocks, map[string]any{"type": "divider"})
		}
		if ourMD != "" {
			n := countSlackNumberedItems(ourMD)
			blocks = append(blocks, map[string]any{
				"type": "section",
				"text": map[string]any{
					"type": "mrkdwn",
					"text": fmt.Sprintf("*💭 对我们的启发（今日 %d 条）*\n\n%s",
						n, ConvertToSlackMrkdwn(ourMD)),
				},
			})
			blocks = append(blocks, map[string]any{"type": "divider"})
		}
	}

	// 7. Today's summary. Upstream numbers each non-blank line; we keep
	// that behaviour and skip double-numbering when the line already
	// starts with "N.".
	summary := strings.TrimSpace(issue.Summary)
	if summary != "" {
		rawLines := strings.Split(summary, "\n")
		kept := make([]string, 0, len(rawLines))
		for _, l := range rawLines {
			if t := strings.TrimSpace(l); t != "" {
				kept = append(kept, t)
			}
		}
		if len(kept) > 0 {
			numbered := make([]string, 0, len(kept))
			for i, l := range kept {
				if slackLeadingNumRe.MatchString(l) {
					numbered = append(numbered, l)
				} else {
					numbered = append(numbered, fmt.Sprintf("%d. %s", i+1, l))
				}
			}
			blocks = append(blocks, map[string]any{
				"type": "section",
				"text": map[string]any{
					"type": "mrkdwn",
					"text": fmt.Sprintf("*📋 今日摘要（%d 条）*\n\n%s",
						len(kept), ConvertToSlackMrkdwn(strings.Join(numbered, "\n"))),
				},
			})
			blocks = append(blocks, map[string]any{"type": "divider"})
		}
	}

	// (Section body previews intentionally REMOVED in v1.0.0.)
	// The full per-section content is only visible on the web viewer via
	// the "📖 查看完整早报" button below. This keeps Slack messages
	// scannable — user feedback was that section prose was too dense
	// and hurt readability inside Slack.

	// 9. Actions — view full report button.
	reportURL := strings.TrimSpace(rendered.ReportURL)
	if reportURL == "" {
		reportURL = "https://github.com/ylzsdafei/briefing-v3"
	}
	blocks = append(blocks, map[string]any{
		"type": "actions",
		"elements": []map[string]any{
			{
				"type": "button",
				"text": map[string]any{
					"type":  "plain_text",
					"text":  "📖 查看完整日报",
					"emoji": true,
				},
				"url":   reportURL,
				"style": "primary",
			},
		},
	})

	// 10. Footer context.
	// v1.0.0: 只保留一行简洁的 "briefing-v3 自动推送 | 日期"，
	// 不向 Slack 用户暴露 FailedSections / QualityWarnings 这种内部
	// 质量信号 (用户反馈: 这些字段不用暴露出来).
	footerElems := []map[string]any{
		{
			"type": "mrkdwn",
			"text": fmt.Sprintf("briefing-v3 自动推送 | %s", dateStr),
		},
	}
	blocks = append(blocks, map[string]any{
		"type":     "context",
		"elements": footerElems,
	})

	return map[string]any{"blocks": blocks}
}

// defaultSectionOrder mirrors config/ai.yaml sections[]. Hard-coded here
// so slack rendering does not need to import the config package; if
// ai.yaml is ever reordered this list must be updated too.
var defaultSectionOrder = []SectionMeta{
	{ID: "product_update", Title: "产品与功能更新"},
	{ID: "research", Title: "前沿研究"},
	{ID: "industry", Title: "行业展望与社会影响"},
	{ID: "opensource", Title: "开源TOP项目"},
	{ID: "social", Title: "社媒分享"},
}

// ------- markdown → Slack mrkdwn helpers -------
//
// These are deliberately local to the render package so slack.go can
// live under internal/render without a cross-package call back into
// internal/publish. If both copies ever drift they should be re-unified.

var (
	slackBoldPattern  = regexp.MustCompile(`\*\*(.+?)\*\*`)
	slackLinkPattern  = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	slackNumberedRe   = regexp.MustCompile(`(?m)^\d+\.`)
	slackLeadingNumRe = regexp.MustCompile(`^\d+\.\s`)
)

// convertToSlackMrkdwn translates a narrow subset of CommonMark into the
// custom mrkdwn Slack expects:
//
//   - **bold**  → *bold*
//   - [t](u)    → <u|t>
//   - trailing  truncation at 2900 characters with a literal "..."
//
// We intentionally do NOT touch headings or list bullets — Slack
// renders them fine as-is, and stripping them would lose structure.
func ConvertToSlackMrkdwn(text string) string {
	if text == "" {
		return ""
	}
	out := slackBoldPattern.ReplaceAllString(text, `*$1*`)
	out = slackLinkPattern.ReplaceAllString(out, `<$2|$1>`)
	// Slack's hard limit on a section text is 3000 characters.
	out = TruncateAtSentence(out, 2900)
	return out
}

// countSlackNumberedItems counts "N." lines in text. Mirrors the
// equivalent helper in internal/generate and internal/publish so each
// layer can compute bullet counts without a cross-package dependency.
func countSlackNumberedItems(text string) int {
	return len(slackNumberedRe.FindAllString(text, -1))
}

// --- Mermaid → image helpers (for Slack) ---

var mermaidBlockRe = regexp.MustCompile("(?s)```mermaid\\s*\n(.+?)```")

// insightMapHeadingRe 清除 LLM 按 prompt 输出的 "🗺️ 今日关系图" 小标题.
// LLM prompt 要求把 mermaid 放在 insight 尾部, render 会把 mermaid 块移到
// 洞察上方作为导读 — 但 heading 留在原处就成了"空标题孤儿"(markdown 和
// Slack 都会显示一行"🗺️ 今日关系图"后面什么都没有). 这里把 heading 连
// 同前后空行一起吃掉.
var insightMapHeadingRe = regexp.MustCompile(`(?m)^[ \t]*#{0,6}[ \t]*🗺️[^\n]*关系图[^\n]*\n?`)

// ExtractMermaidCode finds the first mermaid code block in text and returns
// the raw mermaid source (without fences). Returns "" if none found.
func ExtractMermaidCode(text string) string {
	m := mermaidBlockRe.FindStringSubmatch(text)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// MermaidInkURL returns a mermaid.ink image URL for the given mermaid code.
// mermaid.ink is a free rendering service that converts Mermaid text to PNG/SVG.
func MermaidInkURL(code string) string {
	if code == "" {
		return ""
	}
	encoded := base64.URLEncoding.EncodeToString([]byte(code))
	return "https://mermaid.ink/img/" + encoded
}

// StripMermaidBlocks removes all ```mermaid ... ``` blocks from text,
// used to clean insight text before sending to Slack (where raw mermaid
// code would display as gibberish). Also strips the "🗺️ 今日关系图"
// heading line that the LLM prompt asks for — render moves the mermaid
// diagram itself to the top as a visual intro, and leaving the heading
// behind produces an empty "关系图" section in both markdown and Slack.
func StripMermaidBlocks(text string) string {
	text = mermaidBlockRe.ReplaceAllString(text, "")
	text = insightMapHeadingRe.ReplaceAllString(text, "")
	return text
}

// TruncateAtSentence cuts text to fit within maxRunes, always ending at
// a complete sentence boundary. Never produces trailing "..." or half sentences.
func TruncateAtSentence(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	// Scan backwards from the limit to find a sentence-ending punctuation.
	sub := string(runes[:maxRunes])
	for _, sep := range []string{"。", "；", "\n\n", "\n", ".", ";"} {
		if idx := strings.LastIndex(sub, sep); idx > len(sub)/3 {
			return strings.TrimSpace(sub[:idx+len(sep)])
		}
	}
	// No sentence boundary found — cut at last comma or space.
	for _, sep := range []string{"，", ",", " "} {
		if idx := strings.LastIndex(sub, sep); idx > len(sub)/3 {
			return strings.TrimSpace(sub[:idx])
		}
	}
	return strings.TrimSpace(sub)
}

// MermaidToImg replaces all ```mermaid ... ``` code blocks in markdown
// with <img> tags pointing to mermaid.ink rendered PNGs.
// This produces real images that work everywhere (mobile pinch-zoom, etc).
func MermaidToImg(md string) string {
	return mermaidBlockRe.ReplaceAllStringFunc(md, func(block string) string {
		m := mermaidBlockRe.FindStringSubmatch(block)
		if len(m) < 2 {
			return block
		}
		code := strings.TrimSpace(m[1])
		if code == "" {
			return block
		}
		url := MermaidInkURL(code)
		return fmt.Sprintf(`<img src="%s" alt="图谱" style="max-width:100%%;border-radius:0.5rem;">`, url)
	})
}
