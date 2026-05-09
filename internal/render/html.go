// Package render — HTML page generator.
//
// v1.0.0 rewrite: light Hextra-inspired theme + year/month sidebar +
// floating AI chat widget. Design requirements from the user:
//
//   - light theme by default, dark toggle available
//   - left sidebar listing every issue grouped by year → month,
//     collapsed by default, the current month expanded
//   - right-side table of contents for the current issue
//   - floating chat button bottom-right that opens a real chat UI
//     calling /api/chat on the same host
//   - chat replies must be markdown-rendered (we use marked.js from CDN)
//   - professional terminology annotations are enforced upstream in the
//     compose prompt, nothing to do here
//
// This file intentionally contains no external dependencies beyond the
// Go standard library. The generated HTML is a single self-contained
// file with inlined CSS and minimal JS.
package render

import (
	"fmt"
	"html"
	"html/template"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"briefing-v3/internal/store"
)

// IssueHTMLInput bundles everything the per-issue template needs.
type IssueHTMLInput struct {
	Issue       *store.Issue
	Items       []*store.IssueItem
	Insight     *store.IssueInsight
	Sections    []SectionMeta
	HeadlineImg string // path relative to docs/ (e.g. "../data/images/2026-04-10.png")
}

// IssueHTMLResult is what WriteIssueHTML returns.
type IssueHTMLResult struct {
	Path string
	Size int64
}

// IndexEntry describes a single historical issue.
type IndexEntry struct {
	Date      string // "2026-04-10"
	DateZH    string // "2026年4月10日"
	Title     string
	Link      string // relative path e.g. "2026-04-10.html"
	ItemCount int
}

// sidebarMonth groups sidebarIssues under a single YYYY-MM key.
type sidebarMonth struct {
	YearMonth string // "2026-04"
	MonthZH   string // "4 月"
	Issues    []IndexEntry
}

// sidebarYear groups sidebarMonth under a YYYY key.
type sidebarYear struct {
	Year   string // "2026"
	Months []sidebarMonth
}

// WriteIssueHTML renders the full per-issue HTML and saves it to
// docsDir/YYYY-MM-DD.html. It also auto-scans the docs dir and builds
// the left-sidebar history tree (grouped by year → month) so the page
// always shows up-to-date navigation.
func WriteIssueHTML(docsDir string, in *IssueHTMLInput) (*IssueHTMLResult, error) {
	if in == nil || in.Issue == nil {
		return nil, fmt.Errorf("render html: nil input")
	}
	if err := os.MkdirAll(docsDir, 0o755); err != nil {
		return nil, fmt.Errorf("render html: mkdir %s: %w", docsDir, err)
	}

	// Write the canonical markdown copy of this issue first (side effect
	// is OK — the sidebar scan below includes the current issue because
	// it has been persisted already in a previous stage).
	tpl, err := template.New("issue").Funcs(issueFuncMap).Parse(issuePageTemplate)
	if err != nil {
		return nil, fmt.Errorf("render html: parse template: %w", err)
	}

	// Precompute the sidebar tree so it can be rendered inside the page.
	sidebarYears, err := buildSidebarTree(docsDir, in.Issue.IssueDate.Format("2006-01-02"))
	if err != nil {
		return nil, fmt.Errorf("render html: sidebar: %w", err)
	}

	data := buildIssueTemplateData(in, sidebarYears)

	dateStr := in.Issue.IssueDate.Format("2006-01-02")
	outPath := filepath.Join(docsDir, dateStr+".html")
	f, err := os.Create(outPath)
	if err != nil {
		return nil, fmt.Errorf("render html: create %s: %w", outPath, err)
	}
	defer f.Close()

	if err := tpl.Execute(f, data); err != nil {
		return nil, fmt.Errorf("render html: execute: %w", err)
	}

	fi, _ := os.Stat(outPath)
	absPath, _ := filepath.Abs(outPath)
	return &IssueHTMLResult{Path: absPath, Size: fi.Size()}, nil
}

// WriteIndexHTML rewrites docs/index.html to a landing page that
// redirects to the most recent issue if any, otherwise shows an empty
// state. Historical browsing happens via the sidebar inside each issue.
func WriteIndexHTML(docsDir string, entries []IndexEntry, subtitle string) (string, error) {
	if err := os.MkdirAll(docsDir, 0o755); err != nil {
		return "", fmt.Errorf("render index: mkdir: %w", err)
	}
	// Sort desc by Date so entries[0] is the latest.
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Date > entries[j].Date
	})

	tpl, err := template.New("index").Funcs(issueFuncMap).Parse(indexPageTemplate)
	if err != nil {
		return "", fmt.Errorf("render index: parse: %w", err)
	}

	var latest IndexEntry
	if len(entries) > 0 {
		latest = entries[0]
	}

	data := struct {
		Entries   []IndexEntry
		Latest    IndexEntry
		HasLatest bool
		Subtitle  string
		BuildTime string
	}{
		Entries:   entries,
		Latest:    latest,
		HasLatest: latest.Date != "",
		Subtitle:  subtitle,
		BuildTime: time.Now().Format("2006-01-02 15:04:05 MST"),
	}

	outPath := filepath.Join(docsDir, "index.html")
	f, err := os.Create(outPath)
	if err != nil {
		return "", fmt.Errorf("render index: create: %w", err)
	}
	defer f.Close()

	if err := tpl.Execute(f, data); err != nil {
		return "", fmt.Errorf("render index: execute: %w", err)
	}
	abs, _ := filepath.Abs(outPath)
	return abs, nil
}

// CollectIndexEntries scans docsDir for *.html files matching YYYY-MM-DD.html.
func CollectIndexEntries(docsDir string) ([]IndexEntry, error) {
	if _, err := os.Stat(docsDir); os.IsNotExist(err) {
		return nil, nil
	}
	entries, err := os.ReadDir(docsDir)
	if err != nil {
		return nil, err
	}
	dateFileRe := regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})\.html$`)
	var out []IndexEntry
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		m := dateFileRe.FindStringSubmatch(ent.Name())
		if m == nil {
			continue
		}
		date := m[1]
		t, err := time.Parse("2006-01-02", date)
		if err != nil {
			continue
		}
		out = append(out, IndexEntry{
			Date:   date,
			DateZH: fmt.Sprintf("%d年%d月%d日", t.Year(), int(t.Month()), t.Day()),
			Title:  fmt.Sprintf("AI 资讯日报 %d/%d/%d", t.Year(), int(t.Month()), t.Day()),
			Link:   ent.Name(),
		})
	}
	return out, nil
}

// buildSidebarTree walks docsDir and groups every YYYY-MM-DD.html file
// into a year → month → issue hierarchy. Returns the tree ordered by
// year desc, then month desc, then date desc. The month containing
// currentDate is marked Expanded=true; all other months collapsed.
func buildSidebarTree(docsDir, currentDate string) ([]sidebarYear, error) {
	entries, err := CollectIndexEntries(docsDir)
	if err != nil {
		return nil, err
	}
	// Make sure the current issue is present even if the file was just
	// written a few milliseconds ago and the OS has not flushed yet.
	currentSeen := false
	for _, e := range entries {
		if e.Date == currentDate {
			currentSeen = true
			break
		}
	}
	if !currentSeen && currentDate != "" {
		if t, err := time.Parse("2006-01-02", currentDate); err == nil {
			entries = append(entries, IndexEntry{
				Date:   currentDate,
				DateZH: fmt.Sprintf("%d年%d月%d日", t.Year(), int(t.Month()), t.Day()),
				Title:  fmt.Sprintf("AI 资讯日报 %d/%d/%d", t.Year(), int(t.Month()), t.Day()),
				Link:   currentDate + ".html",
			})
		}
	}

	// Sort desc by Date.
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Date > entries[j].Date
	})

	// Group.
	currentMonth := ""
	if len(currentDate) >= 7 {
		currentMonth = currentDate[:7]
	}
	_ = currentMonth

	byYear := make(map[string]map[string][]IndexEntry)
	yearOrder := []string{}
	monthOrder := map[string][]string{}
	for _, e := range entries {
		if len(e.Date) < 10 {
			continue
		}
		year := e.Date[:4]
		ym := e.Date[:7]
		if _, ok := byYear[year]; !ok {
			byYear[year] = make(map[string][]IndexEntry)
			yearOrder = append(yearOrder, year)
		}
		if _, ok := byYear[year][ym]; !ok {
			monthOrder[year] = append(monthOrder[year], ym)
		}
		byYear[year][ym] = append(byYear[year][ym], e)
	}

	var out []sidebarYear
	for _, year := range yearOrder {
		months := monthOrder[year]
		var ms []sidebarMonth
		for _, ym := range months {
			mi, _ := time.Parse("2006-01", ym)
			ms = append(ms, sidebarMonth{
				YearMonth: ym,
				MonthZH:   fmt.Sprintf("%d 月", int(mi.Month())),
				Issues:    byYear[year][ym],
			})
		}
		out = append(out, sidebarYear{
			Year:   year,
			Months: ms,
		})
	}
	return out, nil
}

// ---------------- template data & helpers ----------------

type sectionTemplateData struct {
	ID      string
	Title   string
	HTMLRaw template.HTML
}

type issueTemplateData struct {
	Title           string
	DateZH          string
	DateStr         string
	Subtitle        string
	HeadlineImg     string
	SummaryLines    []string
	Sections        []sectionTemplateData
	IndustryHTML    template.HTML
	TakeawayHTML    template.HTML
	IndustryCount   int
	TakeawayCount   int
	HasHeadlineImg  bool
	HasInsight      bool
	BuildTime       string
	SidebarYears    []sidebarYear
	CurrentMonth    string // "2026-04", used by sidebar JS to auto-expand
}

func buildIssueTemplateData(in *IssueHTMLInput, sidebarYears []sidebarYear) issueTemplateData {
	issue := in.Issue
	dateStr := issue.IssueDate.Format("2006-01-02")
	dateZH := fmt.Sprintf("%d年%d月%d日", issue.IssueDate.Year(), int(issue.IssueDate.Month()), issue.IssueDate.Day())

	// Group items by section.
	bySection := make(map[string][]*store.IssueItem)
	for _, it := range in.Items {
		if it != nil {
			bySection[it.Section] = append(bySection[it.Section], it)
		}
	}
	for k := range bySection {
		sort.SliceStable(bySection[k], func(i, j int) bool {
			return bySection[k][i].Seq < bySection[k][j].Seq
		})
	}

	sections := make([]sectionTemplateData, 0, len(in.Sections))
	for _, meta := range in.Sections {
		items := bySection[meta.ID]
		if len(items) == 0 {
			continue
		}
		var sb strings.Builder
		for _, it := range items {
			body := strings.TrimSpace(it.BodyMD)
			if body == "" {
				body = fmt.Sprintf("%d. **%s**", it.Seq, strings.TrimSpace(it.Title))
			}
			sb.WriteString(body)
			sb.WriteString("\n\n")
		}
		sections = append(sections, sectionTemplateData{
			ID:      meta.ID,
			Title:   meta.Title,
			HTMLRaw: template.HTML(miniMarkdownToHTML(sb.String())),
		})
	}

	summaryLines := []string{}
	if s := strings.TrimSpace(issue.Summary); s != "" {
		for _, line := range strings.Split(s, "\n") {
			l := strings.TrimSpace(line)
			if l != "" {
				summaryLines = append(summaryLines, l)
			}
		}
	}

	var industryHTML, takeawayHTML template.HTML
	var industryCount, takeawayCount int
	hasInsight := false
	if in.Insight != nil {
		industry := strings.TrimSpace(in.Insight.IndustryMD)
		takeaway := strings.TrimSpace(in.Insight.OurMD)
		if industry != "" {
			industryHTML = template.HTML(miniMarkdownToHTML(industry))
			industryCount = countNumberedLines(industry)
			hasInsight = true
		}
		if takeaway != "" {
			takeawayHTML = template.HTML(miniMarkdownToHTML(takeaway))
			takeawayCount = countNumberedLines(takeaway)
			hasInsight = true
		}
	}

	headlineImg := strings.TrimSpace(in.HeadlineImg)

	currentMonth := ""
	if len(dateStr) >= 7 {
		currentMonth = dateStr[:7]
	}

	return issueTemplateData{
		Title:          fmt.Sprintf("AI 资讯日报 %s", dateZH),
		DateZH:         dateZH,
		DateStr:        dateStr,
		Subtitle:       "AI 早报 · 每日早读 · 全网深度聚合",
		HeadlineImg:    headlineImg,
		SummaryLines:   summaryLines,
		Sections:       sections,
		IndustryHTML:   industryHTML,
		TakeawayHTML:   takeawayHTML,
		IndustryCount:  industryCount,
		TakeawayCount:  takeawayCount,
		HasHeadlineImg: headlineImg != "",
		HasInsight:     hasInsight,
		BuildTime:      time.Now().Format("2006-01-02 15:04:05 MST"),
		SidebarYears:   sidebarYears,
		CurrentMonth:   currentMonth,
	}
}

// ---------------- mini markdown → HTML ----------------

var (
	boldRe       = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	linkRe       = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	imgRe        = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)
	numberedRule = regexp.MustCompile(`^(\d+)\.\s+(.*)$`)
	// rawURLRe catches bare http/https URLs in text that slipped past
	// the LLM formatter. We convert them into a default anchor-text
	// "原文链接" so they render as buttons instead of raw links.
	rawURLRe = regexp.MustCompile(`(?i)(^|[\s(])((?:https?://)[^\s)<"'）】]+)`)
	// videoPlaceholderRe matches [[VIDEO:url]] which the pipeline
	// inserts when an article carries an og:video or <video> tag.
	videoPlaceholderRe = regexp.MustCompile(`\[\[VIDEO:([^\]]+)\]\]`)
)

func miniMarkdownToHTML(md string) string {
	md = strings.ReplaceAll(md, "\r\n", "\n")
	lines := strings.Split(md, "\n")

	var buf strings.Builder
	var (
		inOL      bool
		currentLI strings.Builder
		haveItem  bool
	)

	flushLI := func() {
		if !haveItem {
			return
		}
		body := strings.TrimSpace(currentLI.String())
		if body != "" {
			buf.WriteString("<li>")
			buf.WriteString(inlineMarkdownToHTML(body))
			buf.WriteString("</li>\n")
		}
		currentLI.Reset()
		haveItem = false
	}

	openOL := func() {
		if !inOL {
			buf.WriteString("<ol class=\"items\">\n")
			inOL = true
		}
	}
	closeOL := func() {
		if inOL {
			flushLI()
			buf.WriteString("</ol>\n")
			inOL = false
		}
	}

	for _, rawLine := range lines {
		line := strings.TrimRight(rawLine, " \t")
		trimmed := strings.TrimSpace(line)
		m := numberedRule.FindStringSubmatch(trimmed)
		switch {
		case m != nil:
			openOL()
			flushLI()
			currentLI.WriteString(m[2])
			haveItem = true
		case trimmed == "":
			if haveItem {
				currentLI.WriteString("\n")
			} else {
				closeOL()
			}
		default:
			if haveItem {
				currentLI.WriteString("\n")
				currentLI.WriteString(trimmed)
			} else if trimmed != "" {
				buf.WriteString("<p>")
				buf.WriteString(inlineMarkdownToHTML(trimmed))
				buf.WriteString("</p>\n")
			}
		}
	}
	closeOL()
	return buf.String()
}

func inlineMarkdownToHTML(s string) string {
	escaped := html.EscapeString(s)
	escaped = strings.ReplaceAll(escaped, "&#34;", "\"")
	escaped = strings.ReplaceAll(escaped, "&#39;", "'")

	// 1. Videos: [[VIDEO:url]] → native <video> embed with controls.
	escaped = videoPlaceholderRe.ReplaceAllStringFunc(escaped, func(m string) string {
		mm := videoPlaceholderRe.FindStringSubmatch(m)
		if len(mm) != 2 {
			return m
		}
		src := mm[1]
		return fmt.Sprintf(`<video class="item-video" src="%s" controls preload="metadata"></video>`, src)
	})

	// 2. Images: ![alt](url) → <img class="item-img">. Must be
	// processed BEFORE links because the regex "![" starts the same
	// way as "[".
	escaped = imgRe.ReplaceAllStringFunc(escaped, func(m string) string {
		mm := imgRe.FindStringSubmatch(m)
		if len(mm) != 3 {
			return m
		}
		alt := mm[1]
		url := mm[2]
		return fmt.Sprintf(`<img class="item-img" src="%s" alt="%s" loading="lazy">`, url, alt)
	})

	// 3. Bold.
	escaped = boldRe.ReplaceAllString(escaped, "<strong>$1</strong>")

	// 4. Explicit markdown links [text](url) → pill button.
	escaped = linkRe.ReplaceAllStringFunc(escaped, func(m string) string {
		mm := linkRe.FindStringSubmatch(m)
		if len(mm) != 3 {
			return m
		}
		text := strings.TrimSpace(mm[1])
		url := strings.TrimSpace(mm[2])
		// If the text IS the URL (lazy LLM output), shorten it to the
		// host so the button still looks like a button.
		if text == url || strings.HasPrefix(text, "http://") || strings.HasPrefix(text, "https://") {
			text = shortenHost(url) + " · 原文"
		}
		return fmt.Sprintf(`<a class="src-btn" href="%s" target="_blank" rel="noopener noreferrer">%s ↗</a>`, url, text)
	})

	// 5. Raw URLs that slipped past the LLM formatter. Wrap them in a
	// default pill button so nothing renders as a naked URL.
	escaped = rawURLRe.ReplaceAllStringFunc(escaped, func(m string) string {
		mm := rawURLRe.FindStringSubmatch(m)
		if len(mm) != 3 {
			return m
		}
		prefix := mm[1]
		url := mm[2]
		// Skip URLs already inside an <a> tag we just emitted: they
		// start with '"' in the captured group because of the attr
		// boundary. The regex's prefix group guards against that.
		if strings.Contains(prefix, `"`) {
			return m
		}
		return fmt.Sprintf(`%s<a class="src-btn" href="%s" target="_blank" rel="noopener noreferrer">%s · 原文 ↗</a>`,
			prefix, url, shortenHost(url))
	})

	// 6. Soft line breaks inside a list item become <br>.
	escaped = strings.ReplaceAll(escaped, "\n", "<br>")
	return escaped
}

// shortenHost extracts the host portion of a URL for use as a short
// pill button label, e.g. "https://www.anthropic.com/news/xxx" →
// "anthropic.com".
func shortenHost(raw string) string {
	raw = strings.TrimSpace(raw)
	lower := strings.ToLower(raw)
	for _, p := range []string{"https://", "http://"} {
		if strings.HasPrefix(lower, p) {
			raw = raw[len(p):]
			break
		}
	}
	if i := strings.IndexAny(raw, "/?#"); i > 0 {
		raw = raw[:i]
	}
	raw = strings.TrimPrefix(raw, "www.")
	if raw == "" {
		return "原文"
	}
	return raw
}

// ---------------- templates ----------------

var issueFuncMap = template.FuncMap{
	"inc": func(i int) int { return i + 1 },
}

// issuePageTemplate: light theme, left sidebar year→month tree, floating
// chat widget bottom-right, marked.js for chat markdown rendering.
const issuePageTemplate = `<!DOCTYPE html>
<html lang="zh-CN" data-theme="light">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>{{.Title}}</title>
<link rel="icon" type="image/svg+xml" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'%3E%3Ctext y='26' font-size='28'%3E🤖%3C/text%3E%3C/svg%3E">
<script src="https://cdn.jsdelivr.net/npm/marked@12.0.2/marked.min.js"></script>
<style>
:root {
  --bg: #ffffff;
  --bg-soft: #f9fafb;
  --bg-card: #ffffff;
  --fg: #111827;
  --fg-soft: #4b5563;
  --fg-muted: #9ca3af;
  --border: #e5e7eb;
  --border-strong: #d1d5db;
  --accent: #2563eb;
  --accent-soft: #dbeafe;
  --accent-strong: #1d4ed8;
  --success: #059669;
  --warning: #d97706;
  --danger: #dc2626;
  --sidebar-width: 260px;
  --chat-width: 380px;
}
html[data-theme="dark"] {
  --bg: #0f172a;
  --bg-soft: #1e293b;
  --bg-card: #1e293b;
  --fg: #e2e8f0;
  --fg-soft: #cbd5e1;
  --fg-muted: #64748b;
  --border: #334155;
  --border-strong: #475569;
  --accent: #60a5fa;
  --accent-soft: #1e40af;
  --accent-strong: #93c5fd;
}
* { box-sizing: border-box; margin: 0; padding: 0; }
html, body { height: 100%; }
body {
  background: var(--bg);
  color: var(--fg);
  font-family: -apple-system, BlinkMacSystemFont, "PingFang SC", "Noto Sans CJK SC", "Microsoft YaHei", sans-serif;
  line-height: 1.7;
  -webkit-font-smoothing: antialiased;
  font-size: 15px;
}

/* ---------- top navbar ---------- */
.navbar {
  position: sticky;
  top: 0;
  z-index: 50;
  background: var(--bg);
  border-bottom: 1px solid var(--border);
  height: 56px;
  display: flex;
  align-items: center;
  padding: 0 24px;
  gap: 16px;
}
.navbar .brand {
  display: flex;
  align-items: center;
  gap: 8px;
  font-weight: 700;
  font-size: 17px;
  color: var(--fg);
  text-decoration: none;
}
.navbar .brand .brand-icon { font-size: 22px; }
.navbar .spacer { flex: 1; }
.navbar .btn-theme {
  background: transparent;
  border: 1px solid var(--border);
  border-radius: 6px;
  padding: 6px 10px;
  font-size: 14px;
  cursor: pointer;
  color: var(--fg-soft);
}
.navbar .btn-theme:hover { border-color: var(--border-strong); }

/* ---------- layout ---------- */
.layout {
  display: flex;
  max-width: 1440px;
  margin: 0 auto;
  gap: 24px;
  padding: 24px 16px;
}
.sidebar {
  flex: 0 0 var(--sidebar-width);
  width: var(--sidebar-width);
  position: sticky;
  top: 72px;
  align-self: flex-start;
  max-height: calc(100vh - 96px);
  overflow-y: auto;
  padding-right: 8px;
}
.main {
  flex: 1 1 auto;
  min-width: 0;
  max-width: 880px;
}
@media (max-width: 960px) {
  .sidebar { display: none; }
  .layout { padding: 16px 12px; }
}

/* ---------- sidebar tree ---------- */
.sidebar h3 {
  font-size: 11px;
  text-transform: uppercase;
  letter-spacing: 0.08em;
  color: var(--fg-muted);
  margin-bottom: 12px;
  padding-left: 8px;
}
.sidebar-year {
  margin-bottom: 8px;
}
.sidebar-year-label {
  font-weight: 600;
  padding: 6px 8px;
  color: var(--fg-soft);
  font-size: 14px;
}
.sidebar-month {
  margin-left: 4px;
}
.sidebar-month-label {
  cursor: pointer;
  user-select: none;
  padding: 6px 8px;
  border-radius: 6px;
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 14px;
  color: var(--fg-soft);
  transition: background 0.15s;
}
.sidebar-month-label:hover { background: var(--bg-soft); }
.sidebar-month-label .arrow {
  display: inline-block;
  width: 10px;
  transition: transform 0.15s;
}
.sidebar-month[data-open="true"] > .sidebar-month-label .arrow {
  transform: rotate(90deg);
}
.sidebar-month-list {
  list-style: none;
  padding: 2px 0 2px 20px;
  margin: 0;
  display: none;
}
.sidebar-month[data-open="true"] > .sidebar-month-list { display: block; }
.sidebar-month-list li {
  padding: 4px 0;
}
.sidebar-month-list a {
  display: block;
  padding: 5px 10px;
  border-radius: 6px;
  font-size: 13px;
  color: var(--fg-soft);
  text-decoration: none;
  transition: all 0.15s;
}
.sidebar-month-list a:hover {
  background: var(--bg-soft);
  color: var(--fg);
}
.sidebar-month-list a.active {
  background: var(--accent-soft);
  color: var(--accent-strong);
  font-weight: 600;
}

/* ---------- main content ---------- */
/* Masthead — a pure-text "大字报" hero that needs no image file and
   works even when no headline illustration was generated. Dark poster
   look with a kicker label, oversized headline, and a list of today's
   top storylines. Matches the "important summary billboard" ask. */
.masthead {
  background: linear-gradient(135deg, #111 0%, #1e1e2b 100%);
  color: #fff;
  padding: 44px 40px;
  margin-bottom: 32px;
  border-radius: 14px;
  box-shadow: 0 10px 30px rgba(0,0,0,.12);
  position: relative;
  overflow: hidden;
}
.masthead::before {
  content: "";
  position: absolute;
  top: 0; left: 0; right: 0;
  height: 4px;
  background: linear-gradient(90deg, #f59e0b 0%, #ef4444 50%, #8b5cf6 100%);
}
.masthead-label {
  font-size: 11px;
  letter-spacing: 3px;
  color: #f59e0b;
  text-transform: uppercase;
  font-weight: 700;
  margin-bottom: 14px;
}
.masthead-title {
  font-size: 40px;
  font-weight: 900;
  line-height: 1.15;
  margin: 0 0 22px 0;
  color: #fff;
  letter-spacing: -0.5px;
}
.masthead-sub {
  font-size: 15px;
  line-height: 1.6;
  color: #cbd5e1;
  margin: 0 0 22px 0;
  max-width: 820px;
}
.masthead-tops {
  list-style: none;
  padding: 0;
  margin: 20px 0 0 0;
  border-top: 1px solid #2d3040;
  padding-top: 22px;
}
.masthead-tops li {
  font-size: 15px;
  line-height: 1.55;
  color: #e2e8f0;
  padding: 10px 0 10px 26px;
  border-bottom: 1px dashed #2d3040;
  position: relative;
  font-weight: 500;
}
.masthead-tops li:last-child { border-bottom: none; }
.masthead-tops li::before {
  content: "▸";
  position: absolute;
  left: 0;
  color: #f59e0b;
  font-weight: 700;
}
@media (max-width: 760px) {
  .masthead { padding: 28px 22px; }
  .masthead-title { font-size: 26px; }
}
.headline-img {
  width: 100%;
  border-radius: 10px;
  margin-bottom: 28px;
  display: block;
  box-shadow: 0 4px 20px rgba(0, 0, 0, 0.08);
}
.page-header {
  border-bottom: 1px solid var(--border);
  padding-bottom: 20px;
  margin-bottom: 32px;
}
.page-header h1 {
  font-size: 32px;
  font-weight: 800;
  line-height: 1.3;
  color: var(--fg);
}
.page-header .subtitle {
  color: var(--fg-soft);
  font-size: 14px;
  margin-top: 6px;
}
.date-badge {
  display: inline-block;
  background: var(--accent);
  color: #fff;
  padding: 3px 10px;
  border-radius: 999px;
  font-size: 12px;
  font-weight: 600;
  margin-top: 10px;
}
.summary {
  background: var(--bg-soft);
  border-left: 4px solid var(--success);
  padding: 18px 22px;
  border-radius: 6px;
  margin: 28px 0;
}
.summary h2 {
  font-size: 15px;
  color: var(--success);
  margin-bottom: 10px;
  font-weight: 700;
}
.summary ol { padding-left: 22px; }
.summary ol li { margin-bottom: 4px; color: var(--fg); font-size: 14px; }

section.chapter { margin: 36px 0; }
section.chapter h2 {
  font-size: 22px;
  border-left: 4px solid var(--accent);
  padding-left: 12px;
  margin-bottom: 18px;
  color: var(--fg);
  font-weight: 700;
}
ol.items { padding-left: 28px; }
ol.items li {
  margin-bottom: 24px;
  padding-left: 4px;
}
ol.items li strong {
  color: var(--accent);
  font-size: 15px;
}
a {
  color: var(--accent);
  text-decoration: none;
}
a:hover {
  text-decoration: underline;
}

/* Source-link pill button style: every article link rendered as a
   button instead of a plain underline. */
a.src-btn {
  display: inline-flex;
  align-items: center;
  gap: 3px;
  padding: 2px 10px;
  margin: 0 2px;
  background: var(--accent-soft);
  color: var(--accent-strong);
  border: 1px solid var(--border);
  border-radius: 999px;
  font-size: 12px;
  font-weight: 500;
  text-decoration: none;
  line-height: 1.6;
  transition: all 0.15s;
  white-space: nowrap;
  max-width: 100%;
  overflow: hidden;
  text-overflow: ellipsis;
}
a.src-btn:hover {
  background: var(--accent);
  color: #fff;
  text-decoration: none;
  border-color: var(--accent);
}

/* Per-item hero image hot-linked from the original article. */
img.item-img {
  display: block;
  width: 100%;
  max-width: 720px;
  height: auto;
  margin: 14px 0 6px;
  border-radius: 8px;
  border: 1px solid var(--border);
  background: var(--bg-soft);
}

/* Per-item hero video hot-linked from the original article. */
video.item-video {
  display: block;
  width: 100%;
  max-width: 720px;
  margin: 14px 0 6px;
  border-radius: 8px;
  border: 1px solid var(--border);
  background: #000;
}

.insight {
  margin-top: 40px;
  padding: 28px;
  background: var(--bg-soft);
  border: 1px solid var(--border);
  border-radius: 10px;
}
.insight h2 { font-size: 18px; margin-bottom: 14px; font-weight: 700; }
.insight .industry h2 { color: var(--warning); }
.insight .takeaway h2 { color: var(--success); margin-top: 28px; }
.insight ol { padding-left: 22px; }
.insight ol li { margin-bottom: 12px; font-size: 14px; }
hr {
  border: none;
  border-top: 1px solid var(--border);
  margin: 40px 0;
}

footer {
  margin-top: 56px;
  padding: 24px 0;
  border-top: 1px solid var(--border);
  color: var(--fg-muted);
  font-size: 13px;
  text-align: center;
}
footer a { color: var(--fg-muted); }

/* ---------- floating chat widget ---------- */
.chat-fab {
  position: fixed;
  right: 24px;
  bottom: 24px;
  width: 56px;
  height: 56px;
  border-radius: 50%;
  background: var(--accent);
  color: #fff;
  border: none;
  cursor: pointer;
  box-shadow: 0 6px 20px rgba(37, 99, 235, 0.4);
  z-index: 100;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 24px;
  transition: all 0.2s;
}
.chat-fab:hover { transform: scale(1.05); }
.chat-panel {
  position: fixed;
  right: 24px;
  bottom: 92px;
  width: var(--chat-width);
  max-width: calc(100vw - 48px);
  height: 560px;
  max-height: calc(100vh - 120px);
  background: var(--bg-card);
  border: 1px solid var(--border);
  border-radius: 14px;
  box-shadow: 0 20px 60px rgba(0, 0, 0, 0.15);
  z-index: 100;
  display: none;
  flex-direction: column;
  overflow: hidden;
}
.chat-panel[data-open="true"] { display: flex; }
.chat-header {
  padding: 14px 18px;
  border-bottom: 1px solid var(--border);
  display: flex;
  align-items: center;
  justify-content: space-between;
  background: var(--bg);
}
.chat-header .title {
  font-weight: 700;
  font-size: 14px;
  color: var(--fg);
  display: flex;
  align-items: center;
  gap: 6px;
}
.chat-header .btn-close {
  background: transparent;
  border: none;
  color: var(--fg-muted);
  cursor: pointer;
  font-size: 18px;
  line-height: 1;
}
.chat-body {
  flex: 1 1 auto;
  overflow-y: auto;
  padding: 14px 16px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.chat-msg {
  max-width: 85%;
  padding: 10px 14px;
  border-radius: 12px;
  font-size: 14px;
  line-height: 1.6;
  word-wrap: break-word;
}
.chat-msg.user {
  align-self: flex-end;
  background: var(--accent);
  color: #fff;
  border-bottom-right-radius: 4px;
}
.chat-msg.assistant {
  align-self: flex-start;
  background: var(--bg-soft);
  color: var(--fg);
  border: 1px solid var(--border);
  border-bottom-left-radius: 4px;
}
.chat-msg.assistant h1,
.chat-msg.assistant h2,
.chat-msg.assistant h3 { font-size: 15px; margin: 8px 0 4px; font-weight: 700; }
.chat-msg.assistant p { margin: 6px 0; }
.chat-msg.assistant ul,
.chat-msg.assistant ol { margin: 6px 0 6px 22px; }
.chat-msg.assistant li { margin-bottom: 4px; }
.chat-msg.assistant code {
  background: rgba(0,0,0,0.06);
  padding: 1px 5px;
  border-radius: 3px;
  font-size: 12px;
}
.chat-msg.assistant pre {
  background: rgba(0,0,0,0.06);
  padding: 10px;
  border-radius: 6px;
  overflow-x: auto;
  font-size: 12px;
  margin: 8px 0;
}
.chat-msg.assistant a { color: var(--accent); }
.chat-msg.thinking {
  align-self: flex-start;
  color: var(--fg-muted);
  font-style: italic;
  font-size: 13px;
}
.chat-input {
  border-top: 1px solid var(--border);
  padding: 12px 14px;
  background: var(--bg);
  display: flex;
  gap: 8px;
  align-items: flex-end;
}
.chat-input textarea {
  flex: 1;
  resize: none;
  border: 1px solid var(--border);
  border-radius: 8px;
  padding: 8px 10px;
  font: inherit;
  font-size: 14px;
  color: var(--fg);
  background: var(--bg);
  max-height: 120px;
  outline: none;
  line-height: 1.5;
}
.chat-input textarea:focus { border-color: var(--accent); }
.chat-input button {
  background: var(--accent);
  color: #fff;
  border: none;
  border-radius: 8px;
  padding: 8px 14px;
  font: inherit;
  font-size: 14px;
  font-weight: 600;
  cursor: pointer;
  white-space: nowrap;
}
.chat-input button:disabled { opacity: 0.5; cursor: not-allowed; }
.chat-tip {
  font-size: 11px;
  color: var(--fg-muted);
  text-align: center;
  padding: 6px 0 2px;
}
</style>
</head>
<body>

<nav class="navbar">
  <a class="brand" href="index.html">
    <span class="brand-icon">🤖</span>
    <span>AI 资讯日报</span>
  </a>
  <div class="spacer"></div>
  <button class="btn-theme" id="theme-toggle" title="切换主题">🌙 深色</button>
</nav>

<div class="layout">
  <aside class="sidebar" id="sidebar">
    <h3>历史早报</h3>
    {{range .SidebarYears}}
    <div class="sidebar-year">
      <div class="sidebar-year-label">{{.Year}} 年</div>
      {{range .Months}}
      <div class="sidebar-month" data-month="{{.YearMonth}}" data-open="{{if eq .YearMonth $.CurrentMonth}}true{{else}}false{{end}}">
        <div class="sidebar-month-label">
          <span class="arrow">▶</span>
          <span>{{.MonthZH}}</span>
        </div>
        <ul class="sidebar-month-list">
          {{range .Issues}}
          <li><a href="{{.Link}}" {{if eq .Date $.DateStr}}class="active"{{end}}>{{.DateZH}}</a></li>
          {{end}}
        </ul>
      </div>
      {{end}}
    </div>
    {{end}}
  </aside>

  <main class="main">
    {{if .HasHeadlineImg}}
    <img class="headline-img" src="{{.HeadlineImg}}" alt="briefing-v3 {{.DateStr}} 头版大字报">
    {{else}}
    <section class="masthead">
      <div class="masthead-label">HEADLINE · {{.DateStr}}</div>
      <h1 class="masthead-title">{{.Title}}</h1>
      {{if .SummaryLines}}
      <ol class="masthead-tops">
      {{range .SummaryLines}}<li>{{.}}</li>
      {{end}}</ol>
      {{end}}
    </section>
    {{end}}
    <header class="page-header">
      <h1>{{.Title}}</h1>
      <div class="subtitle">{{.Subtitle}}</div>
      <div class="date-badge">{{.DateStr}}</div>
    </header>

    {{if .SummaryLines}}
    <div class="summary">
      <h2>📋 今日摘要（{{len .SummaryLines}} 条）</h2>
      <ol>
      {{range .SummaryLines}}<li>{{.}}</li>
      {{end}}</ol>
    </div>
    {{end}}

    {{range .Sections}}
    <section class="chapter" id="section-{{.ID}}">
      <h2>{{.Title}}</h2>
      {{.HTMLRaw}}
    </section>
    {{end}}

    {{if .HasInsight}}
    <hr>
    <div class="insight">
      {{if .IndustryHTML}}
      <div class="industry">
        <h2>📊 行业洞察（今日 {{.IndustryCount}} 条）</h2>
        {{.IndustryHTML}}
      </div>
      {{end}}
      {{if .TakeawayHTML}}
      <div class="takeaway">
        <h2>💭 对我们的启发（今日 {{.TakeawayCount}} 条）</h2>
        {{.TakeawayHTML}}
      </div>
      {{end}}
    </div>
    {{end}}

    <footer>
      由 briefing-v3 自动生成 · {{.BuildTime}}
    </footer>
  </main>
</div>

<!-- floating chat widget -->
<button class="chat-fab" id="chat-toggle" title="和 AI 对话">💬</button>
<aside class="chat-panel" id="chat-panel">
  <div class="chat-header">
    <div class="title"><span>🤖</span><span>AI 助手</span></div>
    <button class="btn-close" id="chat-close" title="关闭">✕</button>
  </div>
  <div class="chat-body" id="chat-body">
    <div class="chat-msg assistant">
      <p>你好，我是 briefing-v3 的 AI 助手。我可以帮你：</p>
      <ul>
        <li>解释今日早报里的任何技术术语</li>
        <li>深入分析某条新闻的背景</li>
        <li>对比几家公司的策略</li>
      </ul>
      <p>直接把问题输入下面就行 👇</p>
    </div>
  </div>
  <form class="chat-input" id="chat-form">
    <textarea id="chat-input" rows="2" placeholder="问点什么…（Shift+Enter 换行）"></textarea>
    <button type="submit" id="chat-send">发送</button>
  </form>
  <div class="chat-tip">AI 回答仅供参考，重要信息请交叉核实</div>
</aside>

<script>
(function(){
  // ----- theme toggle -----
  var themeBtn = document.getElementById('theme-toggle');
  function applyTheme(t){
    document.documentElement.setAttribute('data-theme', t);
    themeBtn.textContent = t === 'light' ? '🌙 深色' : '☀️ 浅色';
    try { localStorage.setItem('briefing-theme', t); } catch(e){}
  }
  var savedTheme = 'light';
  try { savedTheme = localStorage.getItem('briefing-theme') || 'light'; } catch(e){}
  applyTheme(savedTheme);
  themeBtn.addEventListener('click', function(){
    applyTheme(document.documentElement.getAttribute('data-theme') === 'light' ? 'dark' : 'light');
  });

  // ----- sidebar month collapse -----
  document.querySelectorAll('.sidebar-month').forEach(function(m){
    var label = m.querySelector('.sidebar-month-label');
    label.addEventListener('click', function(){
      m.setAttribute('data-open', m.getAttribute('data-open') === 'true' ? 'false' : 'true');
    });
  });

  // ----- floating chat -----
  var chatBtn = document.getElementById('chat-toggle');
  var chatPanel = document.getElementById('chat-panel');
  var chatClose = document.getElementById('chat-close');
  var chatBody = document.getElementById('chat-body');
  var chatForm = document.getElementById('chat-form');
  var chatInput = document.getElementById('chat-input');
  var chatSend = document.getElementById('chat-send');
  var issueDate = "{{.DateStr}}";
  var history = []; // {role, content} for conversation memory

  chatBtn.addEventListener('click', function(){
    chatPanel.setAttribute('data-open', chatPanel.getAttribute('data-open') === 'true' ? 'false' : 'true');
    if (chatPanel.getAttribute('data-open') === 'true') chatInput.focus();
  });
  chatClose.addEventListener('click', function(){ chatPanel.setAttribute('data-open', 'false'); });

  chatInput.addEventListener('keydown', function(e){
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      chatForm.dispatchEvent(new Event('submit', {cancelable: true}));
    }
  });

  // marked.js config: disable raw HTML in AI output for safety
  if (window.marked) {
    marked.setOptions({ breaks: true, gfm: true });
  }
  function renderMarkdown(text){
    if (!window.marked) return text.replace(/</g, '&lt;');
    try { return marked.parse(text); } catch(e) { return text.replace(/</g, '&lt;'); }
  }
  function addMsg(role, text){
    var div = document.createElement('div');
    div.className = 'chat-msg ' + role;
    if (role === 'assistant') {
      div.innerHTML = renderMarkdown(text);
    } else {
      div.textContent = text;
    }
    chatBody.appendChild(div);
    chatBody.scrollTop = chatBody.scrollHeight;
    return div;
  }
  function addThinking(){
    var div = document.createElement('div');
    div.className = 'chat-msg thinking';
    div.textContent = '思考中…';
    chatBody.appendChild(div);
    chatBody.scrollTop = chatBody.scrollHeight;
    return div;
  }

  chatForm.addEventListener('submit', function(e){
    e.preventDefault();
    var msg = chatInput.value.trim();
    if (!msg) return;
    chatInput.value = '';
    chatSend.disabled = true;
    addMsg('user', msg);
    history.push({role: 'user', content: msg});
    var thinking = addThinking();

    fetch('/api/chat', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({issue_date: issueDate, messages: history})
    })
    .then(function(r){ if(!r.ok) throw new Error('HTTP '+r.status); return r.json(); })
    .then(function(data){
      thinking.remove();
      var reply = (data && data.reply) || '(空回复)';
      addMsg('assistant', reply);
      history.push({role: 'assistant', content: reply});
    })
    .catch(function(err){
      thinking.remove();
      addMsg('assistant', '**出错了** ❌\n\n' + (err.message || err));
    })
    .finally(function(){ chatSend.disabled = false; chatInput.focus(); });
  });
})();
</script>
</body>
</html>
`

// indexPageTemplate: landing page. If there is any history, it auto
// redirects to the most recent issue; otherwise shows an empty state.
const indexPageTemplate = `<!DOCTYPE html>
<html lang="zh-CN" data-theme="light">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>briefing-v3 · AI 资讯日报</title>
{{if .HasLatest}}
<meta http-equiv="refresh" content="0; url={{.Latest.Link}}">
{{end}}
<style>
body{
  background:#fff;color:#111827;
  font-family:-apple-system,BlinkMacSystemFont,"PingFang SC","Noto Sans CJK SC","Microsoft YaHei",sans-serif;
  display:flex;align-items:center;justify-content:center;
  min-height:100vh;margin:0;padding:24px;
}
.box{max-width:420px;text-align:center;}
h1{font-size:24px;margin-bottom:8px;}
p{color:#6b7280;margin-bottom:16px;line-height:1.6;}
a{color:#2563eb;text-decoration:none;font-weight:600;}
</style>
</head>
<body>
<div class="box">
<h1>🤖 briefing-v3</h1>
<p>{{.Subtitle}}</p>
{{if .HasLatest}}
<p>正在跳转到最新一期：{{.Latest.DateZH}}</p>
<p><a href="{{.Latest.Link}}">如果没有自动跳转，请点击这里</a></p>
{{else}}
<p>还没有生成过日报，首次运行后会自动出现在这里。</p>
{{end}}
</div>
</body>
</html>
`
