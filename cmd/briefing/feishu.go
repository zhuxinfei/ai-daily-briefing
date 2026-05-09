package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"briefing-v3/internal/render"
	"briefing-v3/internal/store"
)

// feishuGetToken 获取飞书 tenant_access_token. 每次调用都重新获取 (token 有效期
// 2h, pipeline 单次跑完 << 2h, 不需要 cache).
func feishuGetToken(appID, appSecret string) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"app_id":     appID,
		"app_secret": appSecret,
	})
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Post(
		"https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal",
		"application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("feishu token request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var result struct {
		Code  int    `json:"code"`
		Msg   string `json:"msg"`
		Token string `json:"tenant_access_token"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("feishu token parse: %w", err)
	}
	if result.Code != 0 {
		return "", fmt.Errorf("feishu token error %d: %s", result.Code, result.Msg)
	}
	return result.Token, nil
}

// feishuPostCard 发送 interactive card 到飞书群.
func feishuPostCard(ctx context.Context, token, chatID string, card map[string]any) error {
	cardJSON, _ := json.Marshal(card)
	body, _ := json.Marshal(map[string]any{
		"receive_id": chatID,
		"msg_type":   "interactive",
		"content":    string(cardJSON),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://open.feishu.cn/open-apis/im/v1/messages?receive_id_type=chat_id",
		bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("feishu build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("feishu post: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("feishu response parse: %w", err)
	}
	if result.Code != 0 {
		return fmt.Errorf("feishu send error %d: %s", result.Code, result.Msg)
	}
	return nil
}

// slackMrkdwnToLarkMd 把 Slack mrkdwn 的 *bold* 转成飞书 lark_md 的 **bold**.
// Slack 用单 * 加粗, 飞书用双 **. Slack payload 里只用有序列表 (1. 2.) 不用
// * 无序列表, 所以 \*...\* 全局替换安全.
var slackBoldRe = regexp.MustCompile(`\*([^*\n]+)\*`)

func slackMrkdwnToLarkMd(text string) string {
	return slackBoldRe.ReplaceAllString(text, "**$1**")
}

// buildFeishuDailyCard 从 insight + summary + dateZH + reportURL 构建飞书
// interactive card, 跟 Slack payload 内容完全对齐.
func buildFeishuDailyCard(insight *store.IssueInsight, summary, dateZH, reportURL string) map[string]any {
	industryMD := ""
	ourMD := ""
	if insight != nil {
		industryMD = strings.TrimSpace(render.StripMermaidBlocks(insight.IndustryMD))
		ourMD = strings.TrimSpace(render.StripMermaidBlocks(insight.OurMD))
	}

	industryN := countNumberedLines(industryMD)
	ourN := countNumberedLines(ourMD)

	summaryLines := strings.Split(strings.TrimSpace(summary), "\n")
	kept := make([]string, 0)
	for _, l := range summaryLines {
		if t := strings.TrimSpace(l); t != "" {
			kept = append(kept, t)
		}
	}
	numberedSummary := make([]string, 0, len(kept))
	for i, l := range kept {
		numberedSummary = append(numberedSummary, fmt.Sprintf("%d. %s", i+1, l))
	}

	elements := []map[string]any{
		{"tag": "div", "text": map[string]any{
			"tag":     "lark_md",
			"content": slackMrkdwnToLarkMd(fmt.Sprintf("**📊 行业洞察（今日 %d 条）**\n\n%s", industryN, industryMD)),
		}},
		{"tag": "hr"},
		{"tag": "div", "text": map[string]any{
			"tag":     "lark_md",
			"content": slackMrkdwnToLarkMd(fmt.Sprintf("**💭 对我们的启发（今日 %d 条）**\n\n%s", ourN, ourMD)),
		}},
		{"tag": "hr"},
		{"tag": "div", "text": map[string]any{
			"tag":     "lark_md",
			"content": fmt.Sprintf("**📋 今日摘要（%d 条）**\n\n%s", len(kept), strings.Join(numberedSummary, "\n")),
		}},
		{"tag": "hr"},
		{"tag": "action", "actions": []map[string]any{{
			"tag":  "button",
			"text": map[string]any{"tag": "plain_text", "content": "📖 查看完整日报"},
			"type": "primary",
			"url":  reportURL,
		}}},
		{"tag": "note", "elements": []map[string]any{
			{"tag": "plain_text", "content": "briefing-v3 自动推送 | " + dateZH},
		}},
	}

	return map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{
			"title":    map[string]any{"content": "🤖 AI 资讯日报 - " + dateZH, "tag": "plain_text"},
			"template": "blue",
		},
		"elements": elements,
	}
}

func buildDailyFeishuCardSnapshot(insight *store.IssueInsight, summary, dateZH, reportURL string) map[string]any {
	return buildFeishuDailyCard(insight, summary, dateZH, reportURL)
}

// buildFeishuWeeklyCard 构建周报飞书卡片. 内容与 Slack 周报完全一致:
// 聚焦 (topics 列表) + 启发 + 思考, 统一走 Slack 清洗路径剥掉 mermaid/
// <details>/HTML, 然后 slackMrkdwnToLarkMd 转成飞书格式.
func buildFeishuWeeklyCard(weekly *store.WeeklyIssue, weeklyPageURL string) map[string]any {
	focusMD := strings.TrimSpace(weekly.FocusMD)
	takeawayMD := strings.TrimSpace(weekly.TakeawaysMD)
	ponderMD := strings.TrimSpace(weekly.PonderMD)

	dateRange := fmt.Sprintf("%s ~ %s",
		weekly.StartDate.Format("01-02"),
		weekly.EndDate.Format("01-02"))
	weekLabel := fmt.Sprintf("%d-W%02d", weekly.Year, weekly.Week)

	var elements []map[string]any

	// 🎯 本周聚焦 (topics list, 不含 mermaid — 与 Slack 一致)
	if focusMD != "" {
		topics := extractFocusTopics(focusMD)
		if len(topics) > 0 {
			var focusText strings.Builder
			fmt.Fprintf(&focusText, "*🎯 本周聚焦（%d 条）*\n\n", len(topics))
			for i, t := range topics {
				fmt.Fprintf(&focusText, "%d. %s\n  【洞察】%s\n", i+1, t.title, t.insight)
			}
			focusStr := render.TruncateAtSentence(focusText.String(), 2900)
			elements = append(elements, map[string]any{
				"tag": "div", "text": map[string]any{
					"tag":     "lark_md",
					"content": slackMrkdwnToLarkMd(focusStr),
				},
			})
			elements = append(elements, map[string]any{"tag": "hr"})
		}
	}

	// 💡 对我们的启发
	if takeawayMD != "" {
		cleaned := cleanForSlack(takeawayMD, 800)
		cleaned = ensureOrderedList(cleaned)
		elements = append(elements, map[string]any{
			"tag": "div", "text": map[string]any{
				"tag":     "lark_md",
				"content": slackMrkdwnToLarkMd("*💡 对我们的启发*\n\n" + cleaned),
			},
		})
	}

	// 🤔 本周思考
	if ponderMD != "" {
		cleaned := mdToSlack(ponderMD)
		if len(nonEmptyLines(cleaned)) > 1 {
			cleaned = ensureOrderedList(cleaned)
		}
		elements = append(elements, map[string]any{
			"tag": "div", "text": map[string]any{
				"tag":     "lark_md",
				"content": slackMrkdwnToLarkMd("*🤔 本周思考*\n\n" + cleaned),
			},
		})
	}

	elements = append(elements, map[string]any{"tag": "hr"})

	if weeklyPageURL != "" {
		elements = append(elements, map[string]any{
			"tag": "action", "actions": []map[string]any{{
				"tag":  "button",
				"text": map[string]any{"tag": "plain_text", "content": "📖 查看完整周报"},
				"type": "primary",
				"url":  weeklyPageURL,
			}},
		})
	}
	elements = append(elements, map[string]any{
		"tag": "note", "elements": []map[string]any{
			{"tag": "plain_text", "content": fmt.Sprintf("briefing-v3 周报 | %s (%s)", weekLabel, dateRange)},
		},
	})

	return map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{
			"title":    map[string]any{"content": weekly.Title, "tag": "plain_text"},
			"template": "purple",
		},
		"elements": elements,
	}
}

func buildWeeklyFeishuCardSnapshot(weekly *store.WeeklyIssue, weeklyPageURL string) map[string]any {
	return buildFeishuWeeklyCard(weekly, weeklyPageURL)
}

// publishDailyToFeishu 是 run.go publish 阶段调用的入口. fail-soft: 失败只
// warn 不阻塞 pipeline (飞书 API 挂不应拦日报推送).
func publishDailyToFeishu(ctx context.Context, insight *store.IssueInsight, summary, dateZH, reportURL string) {
	appID := os.Getenv("FEISHU_APP_ID")
	appSecret := os.Getenv("FEISHU_APP_SECRET")
	chatID := os.Getenv("FEISHU_CHAT_ID")
	if appID == "" || appSecret == "" || chatID == "" {
		return // 飞书未配置, 静默跳过
	}

	token, err := feishuGetToken(appID, appSecret)
	if err != nil {
		fmt.Printf("[WARN] feishu: %v\n", err)
		return
	}

	card := buildFeishuDailyCard(insight, summary, dateZH, reportURL)
	if err := feishuPostCard(ctx, token, chatID, card); err != nil {
		fmt.Printf("[WARN] feishu daily publish: %v\n", err)
		return
	}
	fmt.Println("[feishu] daily card posted OK")
}

// publishWeeklyToFeishu 是 weekly.go publish 阶段调用的入口. 同样 fail-soft.
func publishWeeklyToFeishu(ctx context.Context, weekly *store.WeeklyIssue, weeklyPageURL string) {
	appID := os.Getenv("FEISHU_APP_ID")
	appSecret := os.Getenv("FEISHU_APP_SECRET")
	chatID := os.Getenv("FEISHU_CHAT_ID")
	if appID == "" || appSecret == "" || chatID == "" {
		return
	}

	token, err := feishuGetToken(appID, appSecret)
	if err != nil {
		fmt.Printf("[WARN] feishu: %v\n", err)
		return
	}

	card := buildFeishuWeeklyCard(weekly, weeklyPageURL)
	if err := feishuPostCard(ctx, token, chatID, card); err != nil {
		fmt.Printf("[WARN] feishu weekly publish: %v\n", err)
		return
	}
	fmt.Println("[feishu] weekly card posted OK")
}

// countNumberedLines counts lines matching "N. " pattern.
func countNumberedLines(text string) int {
	n := 0
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) > 2 && trimmed[0] >= '1' && trimmed[0] <= '9' && strings.Contains(trimmed[:3], ".") {
			n++
		}
	}
	return n
}
