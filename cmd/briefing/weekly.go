package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"briefing-v3/internal/config"
	"briefing-v3/internal/gate"
	"briefing-v3/internal/generate"
	"briefing-v3/internal/infocard"
	"briefing-v3/internal/render"
	"briefing-v3/internal/store"
)

// weeklyCommand generates a weekly analysis report for the ISO week
// containing the given date. It reads all daily issues from that week,
// calls the LLM for a structured weekly analysis, persists the result,
// writes a Hugo blog post, and optionally pushes to Slack.
func weeklyCommand(ctx context.Context, cfg *config.Config, date time.Time, gf *globalFlags) error {
	stage := func(msg string) {
		fmt.Printf("[%s] %s\n", time.Now().Format("15:04:05"), msg)
	}

	// --- resolve ISO week boundaries ---
	isoYear, isoWeek := date.ISOWeek()
	startDate := isoWeekStart(isoYear, isoWeek)
	endDate := startDate.AddDate(0, 0, 6) // Sunday
	stage(fmt.Sprintf("weekly: %d-W%02d (%s ~ %s)",
		isoYear, isoWeek,
		startDate.Format("2006-01-02"), endDate.Format("2006-01-02")))

	// --- open store ---
	s, err := store.New("data/briefing.db")
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	// --- load daily issues for this week ---
	dailyIssues, err := s.ListDailyIssuesByDateRange(ctx, gf.domain, startDate, endDate)
	if err != nil {
		return fmt.Errorf("list daily issues: %w", err)
	}
	if len(dailyIssues) == 0 {
		return fmt.Errorf("weekly: no daily issues found for %d-W%02d (%s ~ %s)",
			isoYear, isoWeek,
			startDate.Format("2006-01-02"), endDate.Format("2006-01-02"))
	}
	stage(fmt.Sprintf("weekly: found %d daily issues", len(dailyIssues)))

	// --- assemble daily bundles (batch queries, not N+1) ---
	var issueIDs []int64
	for _, di := range dailyIssues {
		issueIDs = append(issueIDs, di.ID)
	}
	allItems, err := s.ListIssueItemsByIssueIDs(ctx, issueIDs)
	if err != nil {
		return fmt.Errorf("batch list items: %w", err)
	}
	allInsights, err := s.ListIssueInsightsByIssueIDs(ctx, issueIDs)
	if err != nil {
		return fmt.Errorf("batch list insights: %w", err)
	}
	var bundles []generate.DailyBundle
	for _, di := range dailyIssues {
		bundles = append(bundles, generate.DailyBundle{
			Issue:   di,
			Items:   allItems[di.ID],
			Insight: allInsights[di.ID],
		})
	}
	if len(bundles) == 0 {
		return fmt.Errorf("weekly: no usable daily bundles")
	}

	// --- generate weekly analysis via LLM ---
	stage("weekly: calling LLM for weekly analysis")
	weeklyCfg := generate.WeeklyConfig{
		BaseURL:     cfg.LLM.BaseURL,
		APIKey:      cfg.LLM.APIKey,
		Model:       cfg.LLM.Model,
		Temperature: 0.4,
		Timeout:     180 * time.Second,
		// v1.0.1 Phase 4.5 (W4): 对齐日报 LLM retry 策略. MaxRetries 从 3 涨到
		// 5, RetryBackoffSeconds 从 ai.yaml llm.retry_backoff_seconds 读取.
		MaxRetries:          5,
		RetryBackoffSeconds: cfg.LLM.RetryBackoffSeconds,
	}
	result, err := generate.GenerateWeekly(ctx, weeklyCfg, startDate, endDate, bundles)
	if err != nil {
		return fmt.Errorf("generate weekly: %w", err)
	}
	stage("weekly: LLM generation OK")

	title := fmt.Sprintf("第%d周 AI周报：%s", isoWeek, result.TitleKeywords)

	// --- persist to DB ---
	issueIDsJSON, _ := json.Marshal(issueIDs)
	now := time.Now()
	weekly := &store.WeeklyIssue{
		DomainID:      gf.domain,
		Year:          isoYear,
		Week:          isoWeek,
		StartDate:     startDate,
		EndDate:       endDate,
		Title:         title,
		FocusMD:       result.FocusMD,
		SignalsMD:     result.SignalsMD,
		TrendsMD:      result.TrendsMD,
		TrendsDiagram:       result.TrendsDiagram,
		TrendsDiagramDetail: result.TrendsDiagramDetail,
		TakeawaysMD:         result.TakeawaysMD,
		PonderMD:      result.PonderMD,
		FullMD:        "",
		DailyIssueIDs: string(issueIDsJSON),
		Status:        store.IssueStatusGenerated,
		GeneratedAt:   &now,
	}

	weeklyID, err := s.UpsertWeeklyIssue(ctx, weekly)
	if err != nil {
		return fmt.Errorf("upsert weekly: %w", err)
	}
	stage(fmt.Sprintf("weekly: persisted to DB (id=%d)", weeklyID))

	// --- gate 质量检查 (v1.0.1 Phase 4.5 W6+W3) ---
	// 三态判断, 跟日报 run.go:866-892 完全对齐:
	//   Pass → 正常推 test+prod (BRIEFING_MODE=debug 时只推 test, W5 守卫)
	//   Warn → 推 test+prod 但加"质量待审"前缀 alert
	//   Fail → 不推 prod, 仅 alert 到 test, return contentFail 让外层退出
	report := gate.CheckWeekly(weekly, len(dailyIssues), nil)
	stage(fmt.Sprintf("weekly gate: pass=%v warn=%v focus=%d trends=%d takeaway=%d dailyIssues=%d banned=%d",
		report.Pass, report.Warn,
		report.FocusChars, report.TrendsChars, report.TakeawayChars,
		report.DailyIssueCount, len(report.BannedHits)))

	if !report.Pass {
		if report.Warn {
			for _, w := range report.Warnings {
				fmt.Printf("[WEEKLY GATE WARN] %s\n", w)
			}
			stage("weekly gate: warn state — 继续推送但发 alert 标记质量待审")
			if !gf.dryRun {
				alertMsg := buildWeeklyGateAlert(weekly, report)
				alertBody, _ := json.Marshal(map[string]any{"text": alertMsg})
				_ = postAlert(ctx, cfg.Slack.TestWebhook, alertBody)
			}
		} else {
			for _, reason := range report.Reasons {
				fmt.Printf("[WEEKLY GATE FAIL] %s\n", reason)
			}
			if !gf.dryRun {
				alertMsg := buildWeeklyGateAlert(weekly, report)
				alertBody, _ := json.Marshal(map[string]any{"text": alertMsg})
				_ = postAlert(ctx, cfg.Slack.TestWebhook, alertBody)
			}
			return fmt.Errorf("weekly gate hard fail: %s", gate.WeeklyFailureDetail(report))
		}
	}

	// --- dry-run: print and exit ---
	if gf.dryRun {
		stage("dry-run: skipping hugo write and publish")
		fmt.Printf("\n=== %s ===\n聚焦:\n%s\n趋势:\n%s\n", title, result.FocusMD, result.TrendsMD)
		return nil
	}

	// --- generate weekly header card (大字报) ---
	if !gf.noImages {
		weeklyHeader := buildWeeklyHeaderCard(weekly, result)
		weeklyDateStr := fmt.Sprintf("%d-W%02d", isoYear, isoWeek)
		headerDir := fmt.Sprintf("data/images/cards/%s", weeklyDateStr)
		if err := os.MkdirAll(headerDir, 0o755); err == nil {
			headerPath := headerDir + "/header.png"
			if err := renderInfoCardPNG(ctx, "header", weeklyHeader, headerPath); err != nil {
				fmt.Printf("[WARN] weekly headercard: %v\n", err)
			} else {
				stage(fmt.Sprintf("weekly: header card → %s", headerPath))
			}
		}
	}

	// --- write Hugo post ---
	hextraDir := os.Getenv("HEXTRA_SITE_DIR")
	if hextraDir != "" {
		hugoPath, hugoErr := render.WriteWeeklyPost(hextraDir, weekly, dailyIssues)
		if hugoErr != nil {
			fmt.Printf("[WARN] weekly hugo write failed: %v (continuing)\n", hugoErr)
		} else {
			stage(fmt.Sprintf("weekly hugo: wrote %s", hugoPath))
		}

		// Hugo build if HUGO_BIN is set.
		if hugoBin := os.Getenv("HUGO_BIN"); hugoBin != "" {
			stage("weekly: running hugo build")
			if err := hugoBuildf(ctx, hugoBin, hextraDir); err != nil {
				fmt.Printf("[WARN] hugo build: %v\n", err)
			} else {
				stage("weekly: hugo build OK")
			}
		}
	}

	// --- Slack publish ---
	// v1.0.1 Phase 4.5 (W5): 跟日报 run.go:935-940 对齐, BRIEFING_MODE=debug
	// 一律不推 prod 频道. 防止周报调试阶段误推正式频道 (用户原则"只 test 频道").
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("BRIEFING_MODE")))
	wantsProdTarget := gf.target == "auto" || gf.target == "prod"
	targetWantsProd := wantsProdTarget && mode == "prod"
	if wantsProdTarget && mode != "prod" {
		stage("weekly publish: BRIEFING_MODE=" + mode + ", skipping prod channel (仅 prod 模式推正式频道)")
	}
	// Build weekly page URL from BRIEFING_REPORT_URL_BASE.
	weeklyPageURL := ""
	if base := os.Getenv("BRIEFING_REPORT_URL_BASE"); base != "" {
		if idx := strings.Index(base, "{{"); idx > 0 {
			siteRoot := strings.TrimRight(base[:idx], "/")
			weeklyPageURL = fmt.Sprintf("%s/blog/weekly/%d-w%02d/", siteRoot, isoYear, isoWeek)
		}
	}
	slackBlocks := buildWeeklySlackBlocks(weekly, dailyIssues, weeklyPageURL)
	slackBody, _ := json.Marshal(slackBlocks)

	// 持久化 snapshot 给 promote-weekly / promote-weekly-feishu 用; 跟日报
	// run.go 的 savePayloadSnapshot/saveDailyFeishuSnapshot 对齐. 即使本次
	// 跳过 publish, snapshot 也存好, 后续 promote 命令可重放.
	if err := saveWeeklyPayloadSnapshot(date, slackBody); err != nil {
		fmt.Printf("[WARN] save weekly slack snapshot: %v\n", err)
	}
	weeklyFeishuCard := buildWeeklyFeishuCardSnapshot(weekly, weeklyPageURL)
	if cardBytes, err := json.Marshal(weeklyFeishuCard); err != nil {
		fmt.Printf("[WARN] marshal weekly feishu snapshot: %v\n", err)
	} else if err := saveWeeklyFeishuSnapshot(date, cardBytes); err != nil {
		fmt.Printf("[WARN] save weekly feishu snapshot: %v\n", err)
	}

	// BRIEFING_WEEKLY_NO_PUBLISH=1: 跳过 Slack(test+prod) 与飞书推送, 仅
	// 保留 LLM 生成 + DB 持久化 + Hugo 写入 + snapshot 保存. 用于"先更新
	// 线上链接, 等审核确认, 再用 promote-weekly 补推"的工作流.
	noPublish := strings.EqualFold(strings.TrimSpace(os.Getenv("BRIEFING_WEEKLY_NO_PUBLISH")), "1")
	if noPublish {
		stage("weekly publish: BRIEFING_WEEKLY_NO_PUBLISH=1, snapshot saved, skipping all Slack/Feishu push")
		stage("weekly: done")
		return nil
	}

	stage("weekly: posting to Slack test channel")
	testDelivery := postSlackPayload(ctx, store.ChannelSlackTest, cfg.Slack.TestWebhook, slackBody, 0)
	if testDelivery.Status != store.DeliveryStatusSent {
		fmt.Printf("[WARN] weekly slack test: %s\n", testDelivery.ResponseJSON)
	}

	if targetWantsProd {
		stage("weekly: posting to Slack prod channel")
		prodDelivery := postSlackPayload(ctx, store.ChannelSlackProd, cfg.Slack.ProdWebhook, slackBody, 0)
		if prodDelivery.Status != store.DeliveryStatusSent {
			fmt.Printf("[WARN] weekly slack prod: %s\n", prodDelivery.ResponseJSON)
		} else {
			stage("weekly: slack prod OK")
		}

		// v1.0.1 Phase 4.5: 飞书推送 (跟 Slack prod 同条件, fail-soft).
		publishWeeklyToFeishu(ctx, weekly, weeklyPageURL)

		// 与 daily 对齐: prod publish 后 mark weekly_issues.status='published'.
		// fail-soft: Slack/Feishu 已推, mark 失败只 warn.
		if err := s.MarkWeeklyPublished(ctx, weeklyID); err != nil {
			fmt.Printf("[WARN] weekly: mark published: %v\n", err)
		}
	}

	stage("weekly: done")
	return nil
}

// isoWeekStart returns the Monday of the given ISO year/week.
func isoWeekStart(isoYear, isoWeek int) time.Time {
	// Jan 4 is always in ISO week 1.
	jan4 := time.Date(isoYear, 1, 4, 0, 0, 0, 0, time.UTC)
	// Weekday offset: Monday=0 ... Sunday=6.
	offset := int(jan4.Weekday()+6) % 7 // days since Monday
	week1Monday := jan4.AddDate(0, 0, -offset)
	return week1Monday.AddDate(0, 0, (isoWeek-1)*7)
}

// buildWeeklySlackBlocks creates a Slack blocks payload for the weekly report.
// Layout: Header → 日期 → [聚焦标题+图+摘要] → [趋势图] → 启发 → 思考 → 按钮
// Slack limits: section text ≤ 3000 chars, max 50 blocks.
func buildWeeklySlackBlocks(w *store.WeeklyIssue, dailyIssues []*store.Issue, weeklyPageURL string) map[string]any {
	var blocks []map[string]any

	// 1. Header
	blocks = append(blocks, map[string]any{
		"type": "header",
		"text": map[string]any{"type": "plain_text", "text": w.Title, "emoji": true},
	})

	// 2. Date range
	blocks = append(blocks, map[string]any{
		"type": "context",
		"elements": []map[string]any{
			{"type": "mrkdwn", "text": fmt.Sprintf("📅 %s ~ %s · %d 期日报",
				w.StartDate.Format("01-02"),
				w.EndDate.Format("01-02"),
				len(dailyIssues))},
		},
	})
	blocks = append(blocks, map[string]any{"type": "divider"})

	// 3. 本周聚焦: 每个话题 = 事实 + 【洞察】（和日报行业洞察版式一致）
	if focus := strings.TrimSpace(w.FocusMD); focus != "" {
		topics := extractFocusTopics(focus)
		if len(topics) > 0 {
			var focusText strings.Builder
			focusText.WriteString("*🎯 本周聚焦（" + fmt.Sprintf("%d", len(topics)) + " 条）*\n\n")
			for i, t := range topics {
				fmt.Fprintf(&focusText, "%d. %s\n  【洞察】%s\n", i+1, t.title, t.insight)
			}
			focusStr := render.TruncateAtSentence(focusText.String(), 2900)
			blocks = append(blocks, map[string]any{
				"type": "section",
				"text": map[string]any{"type": "mrkdwn", "text": focusStr},
			})
			blocks = append(blocks, map[string]any{"type": "divider"})
		}
	}

	// 4. 对我们的启发（有序列表）
	if t := strings.TrimSpace(w.TakeawaysMD); t != "" {
		cleaned := cleanForSlack(t, 800)
		cleaned = ensureOrderedList(cleaned)
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": "*💡 对我们的启发*\n\n" + cleaned},
		})
	}

	// 5. 本周思考（多条加序号，单条不加）
	if t := strings.TrimSpace(w.PonderMD); t != "" {
		cleaned := mdToSlack(t)
		lines := nonEmptyLines(cleaned)
		if len(lines) > 1 {
			cleaned = ensureOrderedList(cleaned)
		}
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": "*🤔 本周思考*\n\n" + cleaned},
		})
	}

	// 7. Button
	if weeklyPageURL != "" {
		blocks = append(blocks, map[string]any{
			"type": "actions",
			"elements": []map[string]any{{
				"type":  "button",
				"text":  map[string]any{"type": "plain_text", "text": "📖 查看完整周报", "emoji": true},
				"url":   weeklyPageURL,
				"style": "primary",
			}},
		})
	}

	// 8. Footer
	blocks = append(blocks, map[string]any{
		"type": "context",
		"elements": []map[string]any{
			{"type": "mrkdwn", "text": "briefing-v3 · AI 周报"},
		},
	})

	return map[string]any{"blocks": blocks}
}

// cleanForSlack strips mermaid blocks, <details> blocks, HTML tags,
// converts markdown to Slack mrkdwn, and truncates to maxRunes.
func cleanForSlack(md string, maxRunes int) string {
	s := render.StripMermaidBlocks(md)
	s = detailsBlockRe.ReplaceAllString(s, "")
	s = htmlTagRe.ReplaceAllString(s, "")
	s = mdToSlack(s)
	// Collapse multiple blank lines.
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	s = strings.TrimSpace(s)
	return render.TruncateAtSentence(s, maxRunes)
}

// mdToSlack converts standard markdown to Slack mrkdwn format:
//   - ### Header → *Header*
//   - **bold** → *bold*
//   - [text](url) → <url|text>
//   - Strip remaining # prefixes
func mdToSlack(s string) string {
	return render.ConvertToSlackMrkdwn(s)
}

// buildWeeklyHeaderCard constructs a HeaderCard for the weekly report's
// 大字报 PNG. Reuses the same PIL template as the daily headercard.
func buildWeeklyHeaderCard(w *store.WeeklyIssue, result *generate.WeeklyResult) *infocard.HeaderCard {
	truncRunes := func(s string, n int) string {
		rs := []rune(strings.TrimSpace(s))
		if len(rs) <= n {
			return string(rs)
		}
		return string(rs[:n-1]) + "…"
	}

	// Main headline from title keywords.
	mainHeadline := truncRunes(w.Title, 40)

	// Sub headlines from the first line of each focus topic (### titles).
	var subLines []string
	for _, line := range strings.Split(result.FocusMD, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "### ") {
			// Strip "### 1. " prefix → keep the topic title.
			topic := strings.TrimPrefix(line, "### ")
			// Remove leading number.
			if idx := strings.Index(topic, " "); idx > 0 && idx < 5 {
				topic = strings.TrimSpace(topic[idx:])
			}
			subLines = append(subLines, truncRunes(topic, 50))
		}
		if len(subLines) >= 3 {
			break
		}
	}
	subHeadline := strings.Join(subLines, "\n")

	// Lead paragraph from trends summary.
	lead := truncRunes(strings.ReplaceAll(strings.TrimSpace(result.TrendsMD), "\n", " "), 160)

	// Key numbers: week number, daily count, keyword count.
	keyNums := []infocard.KeyNum{
		{Value: fmt.Sprintf("W%d", w.Week), Label: "本周期号"},
		{Value: fmt.Sprintf("%s~%s", w.StartDate.Format("01/02"), w.EndDate.Format("01/02")), Label: "覆盖日期"},
	}

	// Top stories: 聚焦话题 + 信号与噪音 + 趋势方向，填满 MORE STORIES 栏。
	var stories []infocard.TopStory

	// 聚焦话题 (2-3 条)
	for _, sl := range subLines {
		stories = append(stories, infocard.TopStory{Title: truncRunes(sl, 30), Tag: "聚焦"})
	}

	// 信号与噪音：提取每条的第一句 (5-7 条)
	for _, line := range strings.Split(result.SignalsMD, "\n") {
		line = strings.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		// 跳过非列表行
		if len(line) < 3 || (line[0] < '0' || line[0] > '9') {
			continue
		}
		// 去掉 "1. " 前缀
		if idx := strings.Index(line, ". "); idx > 0 && idx < 4 {
			line = strings.TrimSpace(line[idx+2:])
		}
		// 取到第一个句号
		for _, sep := range []string{"。", "；", "."} {
			if idx := strings.Index(line, sep); idx > 0 {
				line = line[:idx]
				break
			}
		}
		stories = append(stories, infocard.TopStory{Title: truncRunes(line, 30), Tag: "信号"})
		if len(stories) >= 12 {
			break
		}
	}

	// 趋势方向补充 (如果还不够)
	for _, line := range strings.Split(result.TrendsMD, "\n") {
		if len(stories) >= 14 {
			break
		}
		line = strings.TrimSpace(line)
		if len(line) < 3 || (line[0] < '0' || line[0] > '9') {
			continue
		}
		if idx := strings.Index(line, ". "); idx > 0 && idx < 4 {
			line = strings.TrimSpace(line[idx+2:])
		}
		for _, sep := range []string{"。", "；", ".", "，"} {
			if idx := strings.Index(line, sep); idx > 0 {
				line = line[:idx]
				break
			}
		}
		stories = append(stories, infocard.TopStory{Title: truncRunes(line, 30), Tag: "趋势"})
	}

	return &infocard.HeaderCard{
		IssueDate:     fmt.Sprintf("%d-W%02d", w.Year, w.Week),
		Edition:       fmt.Sprintf("AI 周报 · 第 %d 周", w.Week),
		MainHeadline:  mainHeadline,
		SubHeadline:   subHeadline,
		LeadParagraph: lead,
		KeyNumbers:    keyNums,
		TopStories:    stories,
		FooterSlogan:  "每周一更 · 趋势尽览",
	}
}

type focusTopic struct {
	title   string // ### heading
	insight string // first sentence of the analysis (as the 【洞察】)
}

// extractFocusTopics extracts ### title + first 1-2 complete sentences as insight.
func extractFocusTopics(md string) []focusTopic {
	var topics []focusTopic
	lines := strings.Split(md, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "### ") {
			continue
		}
		title := strings.TrimSpace(strings.TrimPrefix(line, "### "))

		// Collect text lines after heading (skip mermaid/HTML blocks).
		var textBuf strings.Builder
		for j := i + 1; j < len(lines); j++ {
			l := strings.TrimSpace(lines[j])
			if l == "" && textBuf.Len() > 0 {
				break // stop at first blank line after we have text
			}
			if l == "" {
				continue
			}
			if strings.HasPrefix(l, "```") {
				if strings.HasPrefix(l, "```mermaid") {
					for j++; j < len(lines); j++ {
						if strings.HasPrefix(strings.TrimSpace(lines[j]), "```") {
							break
						}
					}
				}
				continue
			}
			if strings.HasPrefix(l, "<") {
				continue
			}
			if strings.HasPrefix(l, "### ") {
				break // next topic
			}
			textBuf.WriteString(l)
		}

		// Extract first 1-2 complete sentences as the insight.
		fullText := textBuf.String()
		insight := extractLeadSentences(fullText, 2)
		if insight != "" {
			topics = append(topics, focusTopic{title: title, insight: insight})
		}
	}
	return topics
}

// extractLeadSentences takes the first N complete sentences from text.
func extractLeadSentences(text string, n int) string {
	seps := []rune{'。', '；', '.', ';'}
	runes := []rune(text)
	count := 0
	for i, r := range runes {
		for _, sep := range seps {
			if r == sep {
				count++
				if count >= n {
					return strings.TrimSpace(string(runes[:i+1]))
				}
				break
			}
		}
	}
	// If not enough sentences found, return up to 100 chars at a natural break.
	if len(runes) > 100 {
		sub := string(runes[:100])
		for _, sep := range []string{"，", ","} {
			if idx := strings.LastIndex(sub, sep); idx > 30 {
				return strings.TrimSpace(sub[:idx+len(sep)])
			}
		}
		return strings.TrimSpace(sub)
	}
	return strings.TrimSpace(text)
}

// ensureOrderedList adds "N. " prefix to lines that don't already have it.
func ensureOrderedList(s string) string {
	lines := strings.Split(s, "\n")
	n := 0
	for i, l := range lines {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" {
			continue
		}
		n++
		if !orderedListRe.MatchString(trimmed) {
			lines[i] = fmt.Sprintf("%d. %s", n, trimmed)
		}
	}
	return strings.Join(lines, "\n")
}

// nonEmptyLines returns non-blank lines from s.
func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

var orderedListRe = regexp.MustCompile(`^\d+\.\s`)

// stripDetailsBlocks removes entire <details>...</details> sections.
var detailsBlockRe = regexp.MustCompile(`(?s)<details>.*?</details>`)

// stripHTMLTags removes HTML tags from text (for Slack which doesn't render HTML).
var htmlTagRe = regexp.MustCompile(`<[^>]+>`)

func stripHTMLTags(s string) string {
	s = detailsBlockRe.ReplaceAllString(s, "")
	return htmlTagRe.ReplaceAllString(s, "")
}

// hugoBuildf runs hugo --source {siteDir} with a timeout.
func hugoBuildf(ctx context.Context, hugoBin, siteDir string) error {
	subCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(subCtx, hugoBin, "--source", siteDir, "--gc", "--minify")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// buildWeeklyGateAlert formats a Slack plain-text alert for the test channel
// when weekly gate state != Pass. 复用日报 buildGateFailAlert (run.go:2037) 的
// 风格: 表情区分 warn vs hard-fail, 简短说明量化指标和原因.
func buildWeeklyGateAlert(w *store.WeeklyIssue, r *gate.WeeklyReport) string {
	var b strings.Builder
	weekLabel := fmt.Sprintf("%d-W%02d", w.Year, w.Week)
	if r.Warn {
		fmt.Fprintf(&b, "⚠️ briefing-v3 周报 %s 质量待审, 已同步正式频道\n", weekLabel)
	} else {
		fmt.Fprintf(&b, "🚨 briefing-v3 周报 %s 质量不达标, 正式频道已跳过\n", weekLabel)
	}
	fmt.Fprintf(&b, "• 本周日报 %d 份 | Focus %d 字 | Trends %d 字 | Takeaway %d 字\n",
		r.DailyIssueCount, r.FocusChars, r.TrendsChars, r.TakeawayChars)
	if len(r.BannedHits) > 0 {
		fmt.Fprintf(&b, "• 命中 banned pattern: %s\n", strings.Join(r.BannedHits, ", "))
	}
	if len(r.Reasons) > 0 {
		b.WriteString("硬 fail 原因:\n")
		for _, x := range r.Reasons {
			fmt.Fprintf(&b, "  - %s\n", x)
		}
	}
	if len(r.Warnings) > 0 {
		b.WriteString("软 warn 原因:\n")
		for _, x := range r.Warnings {
			fmt.Fprintf(&b, "  - %s\n", x)
		}
	}
	return b.String()
}
