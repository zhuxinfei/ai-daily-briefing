// min_score_test.go — v1.0.1 Phase 4.4 reddit/hn score filter tests.
//
// 这里只测 config 解析 + score 门槛逻辑. HTTP 交互不 mock (reddit.Fetch
// 真的去 reddit.com 的网络测试不稳定). 我们直接验证 newRedditSource /
// newHNSource 读取 MinScore/MinPoints 字段, 并通过一个 stub listing
// 模拟 Fetch 的过滤行为.
//
// Run with: go test ./internal/ingest/ -run MinScore -v

package ingest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"briefing-v3/internal/store"
)

func TestRedditMinScore_Parsing(t *testing.T) {
	row := &store.Source{
		ID:         1,
		DomainID:   "ai",
		Type:       "reddit_json",
		Name:       "r/ML",
		ConfigJSON: `{"url":"https://example.com/r/ML/hot.json","limit":10,"min_score":50}`,
	}
	src, err := newRedditSource(row)
	if err != nil {
		t.Fatalf("newRedditSource: %v", err)
	}
	rs := src.(*redditSource)
	if rs.cfg.MinScore != 50 {
		t.Errorf("expected MinScore=50, got %d", rs.cfg.MinScore)
	}
	if rs.cfg.Limit != 10 {
		t.Errorf("expected Limit=10, got %d", rs.cfg.Limit)
	}
}

func TestRedditMinScore_DefaultZero(t *testing.T) {
	// min_score 不在 config 里 → 默认 0 = 不过滤.
	row := &store.Source{
		ID:         2,
		DomainID:   "ai",
		Type:       "reddit_json",
		Name:       "r/LocalLLaMA",
		ConfigJSON: `{"url":"https://example.com/r/LocalLLaMA/hot.json","limit":25}`,
	}
	src, err := newRedditSource(row)
	if err != nil {
		t.Fatalf("newRedditSource: %v", err)
	}
	rs := src.(*redditSource)
	if rs.cfg.MinScore != 0 {
		t.Errorf("expected MinScore=0 by default, got %d", rs.cfg.MinScore)
	}
}

func TestRedditMinScore_Filters(t *testing.T) {
	// 用 httptest 起一个假 Reddit endpoint, 返回 3 个帖子 (score 10 / 60 / 100).
	// MinScore=50 → 只留 60 和 100.
	payload := redditListing{}
	payload.Data.Children = []redditChild{
		{Data: redditChildData{ID: "p1", Title: "low score post", URL: "https://x.com/1", Permalink: "/r/ML/p1", Score: 10, CreatedUTC: 1}},
		{Data: redditChildData{ID: "p2", Title: "mid score post", URL: "https://x.com/2", Permalink: "/r/ML/p2", Score: 60, CreatedUTC: 2}},
		{Data: redditChildData{ID: "p3", Title: "hi score post", URL: "https://x.com/3", Permalink: "/r/ML/p3", Score: 100, CreatedUTC: 3}},
	}
	body, _ := json.Marshal(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	row := &store.Source{
		ID:         3,
		DomainID:   "ai",
		Type:       "reddit_json",
		Name:       "r/test",
		ConfigJSON: `{"url":"` + srv.URL + `","limit":25,"min_score":50}`,
	}
	src, err := newRedditSource(row)
	if err != nil {
		t.Fatalf("newRedditSource: %v", err)
	}
	items, err := src.Fetch(t.Context())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("expected 2 items after MinScore=50 filter, got %d", len(items))
	}
	for _, it := range items {
		if it.ExternalID == "p1" {
			t.Errorf("p1 (score 10) should be filtered out")
		}
	}
}

func TestHNMinPoints_Parsing(t *testing.T) {
	row := &store.Source{
		ID:         10,
		DomainID:   "ai",
		Type:       "hn_top",
		Name:       "HN",
		ConfigJSON: `{"url":"https://example.com/topstories.json","top_n":30,"min_points":100}`,
	}
	src, err := newHNSource(row)
	if err != nil {
		t.Fatalf("newHNSource: %v", err)
	}
	hs := src.(*hnSource)
	if hs.cfg.MinPoints != 100 {
		t.Errorf("expected MinPoints=100, got %d", hs.cfg.MinPoints)
	}
	if hs.cfg.TopN != 30 {
		t.Errorf("expected TopN=30, got %d", hs.cfg.TopN)
	}
}

func TestHNMinPoints_DefaultZero(t *testing.T) {
	row := &store.Source{
		ID:         11,
		DomainID:   "ai",
		Type:       "hn_top",
		Name:       "HN no threshold",
		ConfigJSON: `{"url":"https://example.com/topstories.json","top_n":30}`,
	}
	src, err := newHNSource(row)
	if err != nil {
		t.Fatalf("newHNSource: %v", err)
	}
	hs := src.(*hnSource)
	if hs.cfg.MinPoints != 0 {
		t.Errorf("expected MinPoints=0 by default, got %d", hs.cfg.MinPoints)
	}
}
