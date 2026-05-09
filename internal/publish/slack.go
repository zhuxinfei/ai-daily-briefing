// Package publish — Slack webhook implementation.
//
// This file ports the Block Kit layout from the legacy
// `scripts/slack-notify.js` reference (buildSlackPayload, convertToSlackMrkdwn),
// but consumes the structured RenderedIssue directly instead of re-parsing
// markdown. The block order intentionally matches the reference so existing
// visual tests and user expectations carry over.
package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"briefing-v3/internal/store"
)

// slackPublisher is the concrete Publisher for either the test or prod channel.
// The only difference between the two is the channel tag returned by Name(),
// so both NewSlackTest and NewSlackProd construct this same struct.
type slackPublisher struct {
	webhookURL  string
	channelName string
	httpClient  *http.Client
}

// NewSlackTest returns a Publisher that posts to the given webhook and tags
// deliveries with ChannelSlackTest.
func NewSlackTest(webhookURL string) Publisher {
	return &slackPublisher{
		webhookURL:  webhookURL,
		channelName: store.ChannelSlackTest,
		httpClient:  &http.Client{Timeout: 15 * time.Second},
	}
}

// NewSlackProd returns a Publisher that posts to the given webhook and tags
// deliveries with ChannelSlackProd. Identical wire format to NewSlackTest.
func NewSlackProd(webhookURL string) Publisher {
	return &slackPublisher{
		webhookURL:  webhookURL,
		channelName: store.ChannelSlackProd,
		httpClient:  &http.Client{Timeout: 15 * time.Second},
	}
}

func (s *slackPublisher) Name() string { return s.channelName }

func (s *slackPublisher) Publish(ctx context.Context, rendered *RenderedIssue) (*store.Delivery, error) {
	now := time.Now()
	delivery := &store.Delivery{
		Channel: s.channelName,
		Target:  s.webhookURL,
		SentAt:  now,
	}
	if rendered == nil || rendered.Issue == nil {
		delivery.Status = store.DeliveryStatusFailed
		delivery.ResponseJSON = `{"error":"rendered issue is nil"}`
		return delivery, fmt.Errorf("slack: rendered issue is nil")
	}
	if s.webhookURL == "" {
		delivery.Status = store.DeliveryStatusSkipped
		delivery.ResponseJSON = `{"reason":"webhook url empty"}`
		return delivery, nil
	}

	payload := buildSlackPayload(rendered)
	body, err := json.Marshal(payload)
	if err != nil {
		delivery.Status = store.DeliveryStatusFailed
		delivery.ResponseJSON = fmt.Sprintf(`{"error":"marshal: %s"}`, err.Error())
		return delivery, fmt.Errorf("slack: marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.webhookURL, bytes.NewReader(body))
	if err != nil {
		delivery.Status = store.DeliveryStatusFailed
		delivery.ResponseJSON = fmt.Sprintf(`{"error":"new request: %s"}`, err.Error())
		return delivery, fmt.Errorf("slack: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		delivery.Status = store.DeliveryStatusFailed
		delivery.ResponseJSON = fmt.Sprintf(`{"error":%q}`, err.Error())
		return delivery, fmt.Errorf("slack: post: %w", err)
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	delivery.ResponseJSON = string(respBytes)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		delivery.Status = store.DeliveryStatusSent
		return delivery, nil
	}
	delivery.Status = store.DeliveryStatusFailed
	return delivery, fmt.Errorf("slack: http %d: %s", resp.StatusCode, string(respBytes))
}

// ------- Block Kit construction -------

// buildSlackPayload mirrors the JS reference in scripts/slack-notify.js.
// Block order: header → industry insight → divider → our takeaways → divider →
// summary → divider → actions (view full report) → context footer.
func buildSlackPayload(r *RenderedIssue) map[string]any {
	issue := r.Issue
	dateStr := issue.IssueDate.Format("2006-01-02")
	chineseDate := fmt.Sprintf("%d年%d月%d日", issue.IssueDate.Year(), int(issue.IssueDate.Month()), issue.IssueDate.Day())

	blocks := make([]map[string]any, 0, 12)

	// Header.
	blocks = append(blocks, map[string]any{
		"type": "header",
		"text": map[string]any{
			"type":  "plain_text",
			"text":  fmt.Sprintf("🤖 AI资讯日报 - %s", chineseDate),
			"emoji": true,
		},
	})

	// Insight sections.
	if r.Insight != nil {
		industryMD := strings.TrimSpace(r.Insight.IndustryMD)
		ourMD := strings.TrimSpace(r.Insight.OurMD)
		if industryMD != "" {
			count := countNumberedItems(industryMD)
			blocks = append(blocks, map[string]any{
				"type": "section",
				"text": map[string]any{
					"type": "mrkdwn",
					"text": fmt.Sprintf("*📊 行业洞察（今日%d条）*\n\n%s", count, convertToSlackMrkdwn(industryMD)),
				},
			})
			blocks = append(blocks, map[string]any{"type": "divider"})
		}
		if ourMD != "" {
			count := countNumberedItems(ourMD)
			blocks = append(blocks, map[string]any{
				"type": "section",
				"text": map[string]any{
					"type": "mrkdwn",
					"text": fmt.Sprintf("*💭 对我们的启发（今日%d条）*\n\n%s", count, convertToSlackMrkdwn(ourMD)),
				},
			})
			blocks = append(blocks, map[string]any{"type": "divider"})
		}
	}

	// Today's summary. The spec stores summary as a pre-formatted paragraph;
	// the reference numbers each non-blank line. Keep that behavior.
	summary := strings.TrimSpace(issue.Summary)
	if summary != "" {
		lines := strings.Split(summary, "\n")
		kept := lines[:0]
		for _, l := range lines {
			if strings.TrimSpace(l) != "" {
				kept = append(kept, strings.TrimSpace(l))
			}
		}
		if len(kept) > 0 {
			numbered := make([]string, 0, len(kept))
			for i, l := range kept {
				// If the line already starts with "1." etc, don't double-number.
				if looksNumbered(l) {
					numbered = append(numbered, l)
				} else {
					numbered = append(numbered, fmt.Sprintf("%d. %s", i+1, l))
				}
			}
			blocks = append(blocks, map[string]any{
				"type": "section",
				"text": map[string]any{
					"type": "mrkdwn",
					"text": fmt.Sprintf("*📋 今日摘要（%d条）*\n\n%s", len(kept), convertToSlackMrkdwn(strings.Join(numbered, "\n"))),
				},
			})
			blocks = append(blocks, map[string]any{"type": "divider"})
		}
	}

	// Actions: view full report.
	reportURL := fmt.Sprintf("https://ai.hubtoday.app/%s/%s/",
		issue.IssueDate.Format("2006-01"), dateStr)
	blocks = append(blocks, map[string]any{
		"type": "actions",
		"elements": []map[string]any{
			{
				"type": "button",
				"text": map[string]any{
					"type":  "plain_text",
					"text":  "📖 查看完整早报",
					"emoji": true,
				},
				"url":   reportURL,
				"style": "primary",
			},
		},
	})

	// Footer context.
	blocks = append(blocks, map[string]any{
		"type": "context",
		"elements": []map[string]any{
			{
				"type": "mrkdwn",
				"text": fmt.Sprintf("由 AI资讯日报 自动推送 | %s", dateStr),
			},
		},
	})

	return map[string]any{"blocks": blocks}
}

// ------- markdown helpers ported from slack-notify.js -------

var (
	slackBoldRe = regexp.MustCompile(`\*\*(.+?)\*\*`)
	slackLinkRe = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	numberedRe  = regexp.MustCompile(`(?m)^\d+\.`)
	leadingNumRe = regexp.MustCompile(`^\d+\.\s`)
)

// convertToSlackMrkdwn converts a subset of CommonMark to Slack's mrkdwn:
// **bold** → *bold*, [text](url) → <url|text>, and hard-truncates at 2800 chars
// to stay under Slack's block text limit.
func convertToSlackMrkdwn(text string) string {
	if text == "" {
		return ""
	}
	out := slackBoldRe.ReplaceAllString(text, `*$1*`)
	out = slackLinkRe.ReplaceAllString(out, `<$2|$1>`)
	if len(out) > 2800 {
		out = out[:2800] + "..."
	}
	return out
}

// countNumberedItems counts lines starting with "N." at the start of a line.
// Mirrors the JS reference.
func countNumberedItems(text string) int {
	return len(numberedRe.FindAllString(text, -1))
}

func looksNumbered(line string) bool {
	return leadingNumRe.MatchString(line)
}
