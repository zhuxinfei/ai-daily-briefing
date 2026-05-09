// Package render turns a generated Issue (items + insight) into the
// human-facing formats used by downstream publishers:
//
//   - markdown.go  — the canonical full-length markdown report, replicating
//     the upstream ai.hubtoday.app layout verbatim.
//   - slack.go     — a Slack Block Kit payload derived from RenderedIssue.
//
// The markdown output is the source of truth: Slack, Feishu doc, and any
// future archive target should be able to reconstruct a publishable surface
// from the string returned by RenderMarkdown alone.
package render

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"briefing-v3/internal/store"
)

// SectionMeta describes a single section (ID + display title) and is
// supplied by the caller so that section ordering matches config/ai.yaml.
// The order of the slice dictates the output order; unknown item sections
// are dropped silently (they are a config mistake, not a user surprise).
type SectionMeta struct {
	ID    string
	Title string
}

// sectionEmoji returns a colored emoji marker for visual section scanning.
var sectionEmoji = map[string]string{
	"product_update": "🔵",
	"research":       "🟢",
	"industry":       "🟡",
	"opensource":     "🟣",
	"social":         "🔴",
}

func sectionDisplayTitle(sec SectionMeta) string {
	if emoji, ok := sectionEmoji[sec.ID]; ok {
		return emoji + " " + sec.Title
	}
	return sec.Title
}

// RenderMarkdown renders a complete daily briefing as markdown, structured
// to match the upstream ai.hubtoday.app layout (see tmp/upstream-baseline/
// 2026-04-09.md). It includes:
//
//  1. H2 title line with the date
//  2. A one-line subtitle blockquote
//  3. The "今日摘要" block (wrapped in a fenced code block)
//  4. One H3 block per section, containing its items' BodyMD verbatim
//  5. A horizontal rule separator
//  6. The "📊 行业洞察" and "💭 对我们的启发" insight blocks with
//     dynamically-computed bullet counts in their headers
//
// Items are assumed to already carry compose-stage numbering in their
// BodyMD (1., 2., ...). RenderMarkdown does not re-number them. Items
// whose Section does not appear in sections are silently dropped.
//
// If insight is nil, the insight footer is omitted rather than emitting
// a broken "今日 0 条" header — callers that require the footer must
// enforce this in their quality gate.
func RenderMarkdown(issue *store.Issue, items []*store.IssueItem, insight *store.IssueInsight, sections []SectionMeta) string {
	if issue == nil {
		return ""
	}

	var b strings.Builder

	// 1. Title line. Upstream uses slashes: "2026/4/9".
	fmt.Fprintf(&b, "## AI资讯日报 %d/%d/%d\n\n",
		issue.IssueDate.Year(), int(issue.IssueDate.Month()), issue.IssueDate.Day())

	// 2. Subtitle blockquote. Kept minimal and brand-neutral; upstream
	// has a long link row here but we replace it with our own tagline.
	b.WriteString("> AI 早报 · 每日早读 · 全网深度聚合\n\n")

	// 3. Today's summary, wrapped in a fenced code block like upstream.
	summary := strings.TrimSpace(issue.Summary)
	b.WriteString("## **今日摘要**\n\n")
	b.WriteString("```\n")
	if summary != "" {
		b.WriteString(summary)
		if !strings.HasSuffix(summary, "\n") {
			b.WriteString("\n")
		}
	}
	b.WriteString("```\n\n")

	// 4. Group items by section for fast lookup.
	bySection := make(map[string][]*store.IssueItem, len(sections))
	for _, it := range items {
		if it == nil {
			continue
		}
		bySection[it.Section] = append(bySection[it.Section], it)
	}
	// Stable sort inside each section by Seq so ordering is deterministic
	// even if the caller handed us an unsorted slice.
	for k := range bySection {
		sort.SliceStable(bySection[k], func(i, j int) bool {
			return bySection[k][i].Seq < bySection[k][j].Seq
		})
	}

	// 5. Emit each section in config order.
	for _, sec := range sections {
		secItems := bySection[sec.ID]
		displayTitle := sectionDisplayTitle(sec)
		if len(secItems) == 0 {
			fmt.Fprintf(&b, "### %s\n\n", displayTitle)
			continue
		}
		fmt.Fprintf(&b, "### %s\n\n", displayTitle)
		for _, it := range secItems {
			body := strings.TrimSpace(it.BodyMD)
			if body == "" {
				// Fall back to title only so the item is at least visible.
				fmt.Fprintf(&b, "%d. **%s**\n\n", it.Seq, strings.TrimSpace(it.Title))
				continue
			}
			b.WriteString(body)
			if !strings.HasSuffix(body, "\n") {
				b.WriteString("\n")
			}
			b.WriteString("\n")
		}
	}

	// 6. Insight footer.
	if insight == nil {
		return b.String()
	}

	industryMD := strings.TrimSpace(insight.IndustryMD)
	ourMD := strings.TrimSpace(insight.OurMD)
	if industryMD == "" && ourMD == "" {
		return b.String()
	}

	b.WriteString("---\n\n")

	// v1.0.1: extract mermaid diagram from insight and render it BEFORE
	// the analysis sections (reader sees the map, then reads the analysis).
	combined := industryMD + "\n" + ourMD
	if mermaidBlock := mermaidBlockRe.FindString(combined); mermaidBlock != "" {
		b.WriteString(mermaidBlock)
		b.WriteString("\n\n")
		industryMD = strings.TrimSpace(StripMermaidBlocks(industryMD))
		ourMD = strings.TrimSpace(StripMermaidBlocks(ourMD))
	}

	if industryMD != "" {
		industryN := countNumberedLines(industryMD)
		fmt.Fprintf(&b, "### 📊 行业洞察（今日 %d 条）\n\n", industryN)
		b.WriteString(industryMD)
		if !strings.HasSuffix(industryMD, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	if ourMD != "" {
		ourN := countNumberedLines(ourMD)
		fmt.Fprintf(&b, "### 💭 对我们的启发（今日 %d 条）\n\n", ourN)
		b.WriteString(ourMD)
		if !strings.HasSuffix(ourMD, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	return b.String()
}

// RenderSectionsMap returns a map from section ID to the rendered markdown
// body (header + items, no inter-section separators) for each section in
// sections. It reuses the same item grouping logic as RenderMarkdown so
// Slack blocks can show the same content without re-parsing a full report.
//
// This is intended to populate publish.RenderedIssue.SectionsMarkdown.
func RenderSectionsMap(items []*store.IssueItem, sections []SectionMeta) map[string]string {
	out := make(map[string]string, len(sections))

	bySection := make(map[string][]*store.IssueItem, len(sections))
	for _, it := range items {
		if it == nil {
			continue
		}
		bySection[it.Section] = append(bySection[it.Section], it)
	}
	for k := range bySection {
		sort.SliceStable(bySection[k], func(i, j int) bool {
			return bySection[k][i].Seq < bySection[k][j].Seq
		})
	}

	for _, sec := range sections {
		secItems := bySection[sec.ID]
		if len(secItems) == 0 {
			out[sec.ID] = ""
			continue
		}
		var sb strings.Builder
		for _, it := range secItems {
			body := strings.TrimSpace(it.BodyMD)
			if body == "" {
				fmt.Fprintf(&sb, "%d. **%s**\n", it.Seq, strings.TrimSpace(it.Title))
				continue
			}
			sb.WriteString(body)
			if !strings.HasSuffix(body, "\n") {
				sb.WriteString("\n")
			}
			sb.WriteString("\n")
		}
		out[sec.ID] = strings.TrimRight(sb.String(), "\n")
	}

	return out
}

// FormatDateZH returns the Chinese-style date "YYYY年M月D日" used in
// titles and Slack headers. Exported so callers that don't need the full
// markdown render can still produce a consistent header.
func FormatDateZH(issue *store.Issue) string {
	if issue == nil {
		return ""
	}
	return fmt.Sprintf("%d年%d月%d日",
		issue.IssueDate.Year(), int(issue.IssueDate.Month()), issue.IssueDate.Day())
}

// numberedLineRe matches lines that start with "1.", "2.", etc. at the
// beginning of a line. Used by the insight header bullet counter.
var numberedLineRe = regexp.MustCompile(`(?m)^\d+\.`)

// countNumberedLines counts how many "N." lines appear in text. It is
// intentionally package-private to avoid duplicating the helper already
// exposed from internal/generate; the gate package uses its own copy.
func countNumberedLines(text string) int {
	return len(numberedLineRe.FindAllString(text, -1))
}
