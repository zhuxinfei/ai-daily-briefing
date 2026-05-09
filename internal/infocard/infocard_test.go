package infocard

import (
	"strings"
	"testing"

	"briefing-v3/internal/store"
)

// TestRuleBasedHeader 验证 T15 fallback header 所有字段都填非空且字符数合理.
func TestRuleBasedHeader(t *testing.T) {
	items := []*store.IssueItem{
		{Section: "product_update", Seq: 1, Title: "Anthropic 发布 Claude Opus 4.6", BodyMD: "正文"},
		{Section: "research", Seq: 1, Title: "Qwen3 推理角色编排", BodyMD: "正文"},
		{Section: "industry", Seq: 1, Title: "教育部把 AI 列为必修课", BodyMD: "正文"},
		{Section: "opensource", Seq: 1, Title: "微软开源 MarkItDown", BodyMD: "正文"},
		{Section: "social", Seq: 1, Title: "Claude Code Routines 上线", BodyMD: "正文"},
	}
	summary := "今日 AI 重磅: Claude 双升级\n教育部 AI 入必修\n开源圈百花齐放"

	h := ruleBasedHeader(items, summary)

	if h.MainHeadline == "" {
		t.Error("MainHeadline 不应为空")
	}
	if h.LeadParagraph == "" {
		t.Error("LeadParagraph 不应为空")
	}
	if len(h.KeyNumbers) != 3 {
		t.Errorf("KeyNumbers 应有 3 条, 实际 %d", len(h.KeyNumbers))
	}
	if len(h.TopStories) != 5 {
		t.Errorf("TopStories 应等于 items 数 (5), 实际 %d", len(h.TopStories))
	}
	if h.FooterSlogan == "" {
		t.Error("FooterSlogan 不应为空")
	}
	for _, kn := range h.KeyNumbers {
		if kn.Value == "" || kn.Label == "" {
			t.Errorf("KeyNumber 字段不应为空: %+v", kn)
		}
	}
	for _, st := range h.TopStories {
		if st.Title == "" || st.Tag == "" {
			t.Errorf("TopStory 字段不应为空: %+v", st)
		}
	}
}

// TestRuleBasedHeader_EmptyItems 边界: items 空时仍应返回非空 header (用 default 填).
func TestRuleBasedHeader_EmptyItems(t *testing.T) {
	h := ruleBasedHeader(nil, "")
	if h.MainHeadline == "" {
		t.Error("即使 items 空, MainHeadline 也应有 default 值")
	}
	if h.LeadParagraph == "" {
		t.Error("LeadParagraph 应有 default")
	}
}

// TestRuleBasedCards 验证 T15 fallback cards 数量正确, 每张字段都填.
func TestRuleBasedCards(t *testing.T) {
	items := []*store.IssueItem{
		{Section: "product_update", Seq: 1, Title: "新模型 X 上线", BodyMD: "新模型支持 9 倍算力. 适合高端任务. 价格降低 20%."},
		{Section: "research", Seq: 2, Title: "新算法 Y", BodyMD: "提速 100 倍。开源。"},
	}
	cards := ruleBasedCards(items)
	if len(cards) != 2 {
		t.Fatalf("cards 数应等于 items, 实际 %d", len(cards))
	}
	for i, c := range cards {
		if c.MainTitle == "" {
			t.Errorf("card[%d] MainTitle 不应为空", i)
		}
		if c.HeroNumber == "" {
			t.Errorf("card[%d] HeroNumber 不应为空", i)
		}
		if c.HeroLabel == "" {
			t.Errorf("card[%d] HeroLabel 不应为空", i)
		}
		if len(c.KeyPoints) == 0 {
			t.Errorf("card[%d] KeyPoints 不应为空", i)
		}
		if c.BrandTag == "" {
			t.Errorf("card[%d] BrandTag 不应为空", i)
		}
	}
}

// TestStripMarkdownNoise 验证 noise 清理.
func TestStripMarkdownNoise(t *testing.T) {
	in := "**bold** and ![img](url) and [link](url) more `code` text"
	out := stripMarkdownNoise(in)
	for _, bad := range []string{"**", "![", "](", "`"} {
		if strings.Contains(out, bad) {
			t.Errorf("noise %q 没被清, output: %q", bad, out)
		}
	}
}

// TestExtractHeroNumber 验证从 body 提取关键数字, 没数字时给 default.
func TestExtractHeroNumber(t *testing.T) {
	cases := []struct {
		body     string
		wantNum  string
		wantText string
	}{
		{"提速 100 倍", "100", ""}, // 实际匹配 100, 至少 4-8 字
		{"涨星 23000", "23000", ""},
		{"新模型 24GB 显存", "24", ""},
		{"无数字纯文本", "重磅", "今日要闻"},
	}
	for _, tc := range cases {
		t.Run(tc.body, func(t *testing.T) {
			num, label := extractHeroNumber(tc.body)
			if num == "" {
				t.Errorf("HeroNumber 不应为空")
			}
			if !strings.Contains(num, tc.wantNum) {
				t.Errorf("got num=%q expect contains %q", num, tc.wantNum)
			}
			if tc.wantText != "" && label != tc.wantText {
				t.Errorf("got label=%q expect %q", label, tc.wantText)
			}
		})
	}
}
