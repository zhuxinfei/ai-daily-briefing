// Package publish — Feishu group chat (IM v1) interactive card implementation.
//
// We use raw net/http against the Feishu open API instead of the lark SDK for
// this file: the only two calls needed (tenant_access_token + send message)
// are trivial, the SDK would bring ~10 extra transitive deps just for this
// use-case, and the payload shape is easier to audit inline.
package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"briefing-v3/internal/store"
)

// feishuBotPublisher posts an interactive card to one Feishu group chat via
// the tenant bot. tenant_access_token is refreshed lazily per call; caching
// could be added later but a token request costs ~50ms and we send at most a
// few messages per day.
type feishuBotPublisher struct {
	appID      string
	appSecret  string
	chatID     string
	baseURL    string // override for tests; defaults to https://open.feishu.cn
	httpClient *http.Client
}

// NewFeishuBot returns a Publisher that pushes the issue as an interactive
// card to the given chatID. If appID / appSecret / chatID are empty, Publish
// returns a skipped delivery instead of failing (Day 1: Feishu credentials
// may not be configured yet).
func NewFeishuBot(appID, appSecret, chatID string) Publisher {
	return &feishuBotPublisher{
		appID:      appID,
		appSecret:  appSecret,
		chatID:     chatID,
		baseURL:    "https://open.feishu.cn",
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

func (f *feishuBotPublisher) Name() string { return store.ChannelFeishuBot }

func (f *feishuBotPublisher) Publish(ctx context.Context, rendered *RenderedIssue) (*store.Delivery, error) {
	now := time.Now()
	delivery := &store.Delivery{
		Channel: store.ChannelFeishuBot,
		Target:  f.chatID,
		SentAt:  now,
	}

	if f.appID == "" || f.appSecret == "" || f.chatID == "" {
		delivery.Status = store.DeliveryStatusSkipped
		delivery.ResponseJSON = `{"reason":"feishu credentials not configured"}`
		return delivery, nil
	}
	if rendered == nil || rendered.Issue == nil {
		delivery.Status = store.DeliveryStatusFailed
		delivery.ResponseJSON = `{"error":"rendered issue is nil"}`
		return delivery, fmt.Errorf("feishu_bot: rendered issue is nil")
	}

	token, err := f.fetchTenantAccessToken(ctx)
	if err != nil {
		delivery.Status = store.DeliveryStatusFailed
		delivery.ResponseJSON = fmt.Sprintf(`{"error":"tenant_token: %s"}`, err.Error())
		return delivery, fmt.Errorf("feishu_bot: fetch tenant token: %w", err)
	}

	card := buildFeishuBotCard(rendered)
	cardJSON, err := json.Marshal(card)
	if err != nil {
		delivery.Status = store.DeliveryStatusFailed
		delivery.ResponseJSON = fmt.Sprintf(`{"error":"marshal card: %s"}`, err.Error())
		return delivery, fmt.Errorf("feishu_bot: marshal card: %w", err)
	}

	body, err := json.Marshal(map[string]any{
		"receive_id": f.chatID,
		"msg_type":   "interactive",
		// Feishu expects the card payload as a JSON-encoded STRING, not an object.
		"content": string(cardJSON),
	})
	if err != nil {
		delivery.Status = store.DeliveryStatusFailed
		delivery.ResponseJSON = fmt.Sprintf(`{"error":"marshal envelope: %s"}`, err.Error())
		return delivery, fmt.Errorf("feishu_bot: marshal envelope: %w", err)
	}

	url := f.baseURL + "/open-apis/im/v1/messages?receive_id_type=chat_id"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		delivery.Status = store.DeliveryStatusFailed
		delivery.ResponseJSON = fmt.Sprintf(`{"error":"new request: %s"}`, err.Error())
		return delivery, fmt.Errorf("feishu_bot: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := f.httpClient.Do(req)
	if err != nil {
		delivery.Status = store.DeliveryStatusFailed
		delivery.ResponseJSON = fmt.Sprintf(`{"error":%q}`, err.Error())
		return delivery, fmt.Errorf("feishu_bot: post: %w", err)
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	delivery.ResponseJSON = string(respBytes)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		delivery.Status = store.DeliveryStatusFailed
		return delivery, fmt.Errorf("feishu_bot: http %d: %s", resp.StatusCode, string(respBytes))
	}

	// Feishu always returns 200 with a structured envelope; inspect the code
	// field to detect logical failures.
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(respBytes, &env); err == nil && env.Code != 0 {
		delivery.Status = store.DeliveryStatusFailed
		return delivery, fmt.Errorf("feishu_bot: api code=%d msg=%s", env.Code, env.Msg)
	}
	delivery.Status = store.DeliveryStatusSent
	return delivery, nil
}

// fetchTenantAccessToken calls /auth/v3/tenant_access_token/internal.
// The returned token is valid for ~2 hours; caching is intentionally omitted
// because we send at most a handful of messages per day.
func (f *feishuBotPublisher) fetchTenantAccessToken(ctx context.Context) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"app_id":     f.appID,
		"app_secret": f.appSecret,
	})
	url := f.baseURL + "/open-apis/auth/v3/tenant_access_token/internal"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := f.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("http %d: %s", resp.StatusCode, string(respBytes))
	}
	var env struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
	}
	if err := json.Unmarshal(respBytes, &env); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	if env.Code != 0 || env.TenantAccessToken == "" {
		return "", fmt.Errorf("api code=%d msg=%s", env.Code, env.Msg)
	}
	return env.TenantAccessToken, nil
}

// buildFeishuBotCard builds a compact interactive card that announces the
// issue: title, a short 2-line summary teaser, and a "view full report" link
// button. The card @-mentions everyone via an embedded markdown element.
func buildFeishuBotCard(r *RenderedIssue) map[string]any {
	issue := r.Issue
	dateStr := issue.IssueDate.Format("2006-01-02")
	reportURL := fmt.Sprintf("https://ai.hubtoday.app/%s/%s/",
		issue.IssueDate.Format("2006-01"), dateStr)

	// Two-line teaser: take the first two non-blank lines from Summary.
	teaser := firstNLines(issue.Summary, 2)
	if teaser == "" {
		teaser = "今日AI资讯速览，点击查看完整早报。"
	}

	title := issue.Title
	if title == "" {
		title = fmt.Sprintf("%s AI洞察日报", dateStr)
	}

	elements := []map[string]any{
		// @all + teaser.
		{
			"tag":     "markdown",
			"content": "<at id=all></at>\n\n" + teaser,
		},
		{"tag": "hr"},
		// View full report button.
		{
			"tag": "action",
			"actions": []map[string]any{
				{
					"tag": "button",
					"text": map[string]any{
						"tag":     "plain_text",
						"content": "📖 查看完整日报",
					},
					"type": "primary",
					"url":  reportURL,
				},
			},
		},
	}

	return map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{
			"title": map[string]any{
				"tag":     "plain_text",
				"content": "🤖 " + title,
			},
			"template": "blue",
		},
		"elements": elements,
	}
}

// firstNLines returns the first n non-blank lines of s joined by "\n".
func firstNLines(s string, n int) string {
	if s == "" || n <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	kept := make([]string, 0, n)
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		kept = append(kept, t)
		if len(kept) >= n {
			break
		}
	}
	return strings.Join(kept, "\n")
}
