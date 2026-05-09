// signal_strength_test.go — unit tests for v1.0.1 Phase 4.2.
//
// Run with: go test ./internal/ingest/ -run SignalStrength -v

package ingest

import (
	"strings"
	"testing"

	"briefing-v3/internal/store"
)

func TestExtractSignalKeywords(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  []string // lowercased, order from regex match
	}{
		{"english proper nouns + version", "OpenAI releases GPT-6 with breakthrough capabilities",
			[]string{"openai", "gpt-6"}},
		{"chinese phrase", "阿里通义千问开源新版本",
			[]string{"阿里通义千问开源新版本"}},
		{"short english dropped", "A big day",
			nil},
		{"dedup", "Claude Claude Claude",
			[]string{"claude"}},
		{"mixed", "阿里发布 Claude 对标模型",
			[]string{"claude", "阿里发布", "对标模型"}},
		{"acronym variants", "LLM providers compare GPT-4o and Claude-3.5",
			[]string{"llm", "gpt-4o", "claude"}},
		{"empty", "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractSignalKeywords(tc.title)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v (len %d), want %v (len %d)", got, len(got), tc.want, len(tc.want))
			}
			// set-equality check (order may differ due to regex ordering)
			gotSet := make(map[string]bool, len(got))
			for _, k := range got {
				gotSet[k] = true
			}
			for _, w := range tc.want {
				if !gotSet[w] {
					t.Errorf("missing keyword %q in %v", w, got)
				}
			}
		})
	}
}

func TestJaccardSimilarity(t *testing.T) {
	tests := []struct {
		name string
		a, b []string
		want float64
	}{
		{"identical", []string{"a", "b", "c"}, []string{"a", "b", "c"}, 1.0},
		{"disjoint", []string{"a", "b"}, []string{"c", "d"}, 0.0},
		{"half overlap", []string{"a", "b"}, []string{"b", "c"}, 1.0 / 3.0},
		{"subset", []string{"a"}, []string{"a", "b"}, 0.5},
		{"empty a", nil, []string{"a"}, 0.0},
		{"empty b", []string{"a"}, nil, 0.0},
		{"both empty", nil, nil, 0.0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := jaccardSimilarity(tc.a, tc.b)
			if !floatClose(got, tc.want, 1e-9) {
				t.Errorf("jaccardSimilarity(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestExtractHost(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		sourceID int64
		want     string
	}{
		{"plain", "https://openai.com/news/xyz", 42, "openai.com"},
		{"www stripped", "https://www.techcrunch.com/article/1", 10, "techcrunch.com"},
		{"empty url → fallback", "", 99, "source#99"},
		{"malformed → fallback", "::not a url::", 7, "source#7"},
		{"uppercase host lowered", "HTTPS://TECHCRUNCH.COM/x", 1, "techcrunch.com"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractHost(tc.url, tc.sourceID)
			if got != tc.want {
				t.Errorf("extractHost(%q, %d) = %q, want %q", tc.url, tc.sourceID, got, tc.want)
			}
		})
	}
}

func TestCalculateSignalStrength_MultiSource(t *testing.T) {
	// 场景: 3 个源报道同一件事 ("OpenAI发布 GPT-6"), 1 个源报道别的事.
	// 期望: 前 3 条 signal=3, 最后 1 条 signal=1.
	items := []*store.RawItem{
		{ID: 1, URL: "https://openai.com/gpt6", SourceID: 1, Title: "OpenAI releases GPT-6 with breakthrough performance"},
		{ID: 2, URL: "https://techcrunch.com/openai-gpt6", SourceID: 2, Title: "GPT-6 launch from OpenAI breakthrough capabilities"},
		{ID: 3, URL: "https://www.theverge.com/openai", SourceID: 3, Title: "OpenAI unveils GPT-6 breakthrough release"},
		{ID: 4, URL: "https://anthropic.com/claude5", SourceID: 4, Title: "Anthropic launches Claude 5 Sonnet"},
	}
	dist := CalculateSignalStrength(items)
	if items[0].SignalStrength != 3 || items[1].SignalStrength != 3 || items[2].SignalStrength != 3 {
		t.Errorf("expected items 1-3 all ss=3, got %d %d %d",
			items[0].SignalStrength, items[1].SignalStrength, items[2].SignalStrength)
	}
	if items[3].SignalStrength != 1 {
		t.Errorf("expected item 4 ss=1, got %d", items[3].SignalStrength)
	}
	if dist[3] != 3 || dist[1] != 1 {
		t.Errorf("distribution wrong: got %v, want {3:3, 1:1}", dist)
	}
}

func TestCalculateSignalStrength_SameSourceMultipleItems(t *testing.T) {
	// 同一个 source 的 2 篇相似标题不应该算 2 个 distinct host.
	items := []*store.RawItem{
		{ID: 1, URL: "https://openai.com/a", SourceID: 1, Title: "OpenAI launches GPT-6 breakthrough model"},
		{ID: 2, URL: "https://openai.com/b", SourceID: 1, Title: "OpenAI GPT-6 breakthrough launches today"},
	}
	CalculateSignalStrength(items)
	if items[0].SignalStrength != 1 {
		t.Errorf("same-host siblings should count as 1 distinct host, got ss=%d", items[0].SignalStrength)
	}
}

func TestCalculateSignalStrength_ShortTitleFallsBackToOne(t *testing.T) {
	// 关键词不足 minKeywordsForGrouping (2) 的条目不参与合并.
	items := []*store.RawItem{
		{ID: 1, URL: "https://x.com/", SourceID: 1, Title: "Hello"},               // 0 kw
		{ID: 2, URL: "https://y.com/", SourceID: 2, Title: "OpenAI"},              // 1 kw
		{ID: 3, URL: "https://z.com/", SourceID: 3, Title: "Big Launch"},          // 1 kw ("Launch" >=4)
		{ID: 4, URL: "https://a.com/", SourceID: 4, Title: "OpenAI Claude GPT-6"}, // 3 kw
	}
	CalculateSignalStrength(items)
	for i, it := range items[:3] {
		if it.SignalStrength != 1 {
			t.Errorf("item %d short-title: expected ss=1, got %d", i, it.SignalStrength)
		}
	}
	if items[3].SignalStrength != 1 {
		t.Errorf("item 4 solo: expected ss=1, got %d", items[3].SignalStrength)
	}
}

func TestCalculateSignalStrength_Empty(t *testing.T) {
	dist := CalculateSignalStrength(nil)
	if len(dist) != 0 {
		t.Errorf("nil items: expected empty dist, got %v", dist)
	}
	dist = CalculateSignalStrength([]*store.RawItem{})
	if len(dist) != 0 {
		t.Errorf("empty items: expected empty dist, got %v", dist)
	}
}

func TestCalculateSignalStrength_NilSkipped(t *testing.T) {
	items := []*store.RawItem{
		nil,
		{ID: 1, URL: "https://x.com/", SourceID: 1, Title: "OpenAI releases GPT-6 breakthrough model"},
		nil,
	}
	CalculateSignalStrength(items)
	if items[1].SignalStrength != 1 {
		t.Errorf("single non-nil item: expected ss=1, got %d", items[1].SignalStrength)
	}
}

func floatClose(a, b, eps float64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff < eps
}

// ---- v1.0.1 Phase 4.6 CrossMention tests ----

func TestExtractRepoMatchTerms(t *testing.T) {
	cases := []struct {
		in       string
		wantAll  []string // all expected terms
		wantDrop []string // strings that must NOT appear
	}{
		// v1.0.1 Phase 4.6 修正: 不再返回 brand 单词 (如 "hermes"), 只 full + dashed
		{"NousResearch/hermes-agent", []string{"NousResearch/hermes-agent", "hermes-agent"}, []string{"hermes"}},
		{"microsoft/VibeVoice", []string{"microsoft/VibeVoice", "VibeVoice"}, nil},
		{"google/magika", []string{"google/magika", "magika"}, nil},
		{"openai/codex-plugin-cc", []string{"openai/codex-plugin-cc", "codex-plugin-cc"}, []string{"codex"}},
		// short-name agent/ai 应被过滤 (太通用)
		{"facebook/agent", []string{"facebook/agent"}, []string{"agent"}},
		{"x/ai", []string{"x/ai"}, []string{"ai"}},
		{"", nil, nil},
	}
	for _, tc := range cases {
		got := extractRepoMatchTerms(tc.in)
		for _, want := range tc.wantAll {
			found := false
			for _, g := range got {
				if g == want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%q: expected term %q, got %v", tc.in, want, got)
			}
		}
		for _, drop := range tc.wantDrop {
			for _, g := range got {
				if g == drop {
					t.Errorf("%q: should NOT contain %q, got %v", tc.in, drop, got)
				}
			}
		}
	}
}

func TestCountMentionsWordBoundary(t *testing.T) {
	cases := []struct {
		hay, needle string
		want        int
	}{
		{"hermes is great, hermes-agent rocks", "hermes", 2}, // 2 次: 单独 + hermes-agent 里的 hermes (两边都是非字母)
		{"Agent framework and agents everywhere", "agent", 1}, // "Agent " 匹配, "agents" 不匹配 (右边是 s)
		{"no match here", "xyz", 0},
		{"repeated VibeVoice VibeVoice VibeVoice end", "vibevoice", 3},
		{"prefix/hermes-agent is good", "hermes-agent", 1},
	}
	for _, tc := range cases {
		got := countMentions(strings.ToLower(tc.hay), strings.ToLower(tc.needle))
		if got != tc.want {
			t.Errorf("hay=%q needle=%q got=%d want=%d", tc.hay, tc.needle, got, tc.want)
		}
	}
}

func TestCalculateCrossMentions(t *testing.T) {
	// Setup: ossinsight has Hermes + Magika; news sources talk about Hermes 3 times, Magika 1 time
	items := []*store.RawItem{
		// ossinsight (source_id=100)
		{ID: 1, SourceID: 100, Title: "NousResearch/hermes-agent", Content: "The agent that grows with you"},
		{ID: 2, SourceID: 100, Title: "google/magika", Content: "AI file type detector"},
		{ID: 3, SourceID: 100, Title: "microsoft/VibeVoice", Content: "Voice AI"},
		// news (source_id=200) — 注意: 为了测试 full "owner/repo" 和 dashed short name
	// 都能匹配, news 里需含这些形式. v1.0.1 Phase 4.6 修正后不再匹配 "Hermes" brand.
	{ID: 10, SourceID: 200, Title: "NousResearch/hermes-agent adds voice", Content: "The hermes-agent repo is now trending"},
		{ID: 11, SourceID: 200, Title: "Best AI tools", Content: "Notable: hermes-agent, VibeVoice, magika"},
		{ID: 12, SourceID: 200, Title: "magika goes viral", Content: "magika can detect file types quickly"},
	}
	sourceTypes := map[int64]string{
		100: "ossinsight",
		200: "rss",
	}
	CalculateCrossMentions(items, sourceTypes)

	// Verify counts (只 full + dashed 匹配, brand "Hermes" 不再算)
	checks := map[int64]int{
		1: 2, // hermes-agent: 2 次 (id=10 full + id=11 "hermes-agent" 在 title/content; id=10 content "hermes-agent" 也算一次); 容差 ±1
		2: 2, // magika: 2 次 (id=11 "magika" + id=12 title+content)
		3: 2, // VibeVoice: 2 次 (id=11 一次, 但大小写/上下文算 1-2, 容差 ±1)
	}
	for id, want := range checks {
		for _, it := range items {
			if it.ID == id {
				if it.CrossMentionCount < want-1 || it.CrossMentionCount > want+1 {
					// 允许 ±1 容差: 因为 "owner/repo" 可能多算一次
					t.Errorf("item id=%d got CrossMentionCount=%d, want ~%d", id, it.CrossMentionCount, want)
				}
				break
			}
		}
	}
	// Non-ossinsight items should stay 0
	for _, it := range items {
		if it.SourceID == 200 && it.CrossMentionCount != 0 {
			t.Errorf("non-ossinsight item id=%d should have 0 mentions, got %d", it.ID, it.CrossMentionCount)
		}
	}
}
