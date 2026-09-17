package ingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"briefing-v3/internal/store"
)

// fixtureTrendingHTML is a trimmed capture of the live github.com/trending
// page: three real <article class="Box-row"> blocks with the surrounding
// markup intact, so the selectors in parseTrendingHTML are exercised against
// the real shape rather than a hand-written approximation of it.
const fixtureTrendingHTML = "testdata/trending_github.html"

func newTrendingTestSource(t *testing.T, configJSON string) *githubTrendingSource {
	t.Helper()
	row := &store.Source{
		ID: 7, DomainID: "ai", Type: "github_trending",
		Name:       "GitHub Trending",
		ConfigJSON: configJSON,
	}
	src, err := newGitHubTrendingSource(row)
	if err != nil {
		t.Fatalf("newGitHubTrendingSource: %v", err)
	}
	return src.(*githubTrendingSource)
}

// TestGitHubTrendingSource_ParsesTrendingHTML 是本修复的核心回归: 官方
// trending 页必须解析出 repo 名/描述/语言/总星/涨星/排名, 且 rank 与
// stars_total 要写进 metadata — run.go 的排序与 opensource 兜底都读这两个键.
func TestGitHubTrendingSource_ParsesTrendingHTML(t *testing.T) {
	fixture, err := os.ReadFile(fixtureTrendingHTML)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	src := newTrendingTestSource(t, `{"url":"`+srv.URL+`/trending"}`)
	items, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 trending repos, got %d", len(items))
	}

	want := []struct {
		title      string
		url        string
		author     string
		rank       int
		starsTotal int
		starsDelta int
		forks      int
		language   string
	}{
		{"alibaba/open-code-review", "https://github.com/alibaba/open-code-review", "alibaba", 1, 31816, 3231, 2258, "Go"},
		{"cloudflare/security-audit-skill", "https://github.com/cloudflare/security-audit-skill", "cloudflare", 2, 7209, 927, 421, "JavaScript"},
		{"JustVugg/colibri", "https://github.com/JustVugg/colibri", "JustVugg", 3, 35029, 1546, 3679, "C"},
	}
	for i, w := range want {
		got := items[i]
		if got.Title != w.title {
			t.Errorf("[%d] Title = %q, want %q", i, got.Title, w.title)
		}
		if got.URL != w.url {
			t.Errorf("[%d] URL = %q, want %q", i, got.URL, w.url)
		}
		if got.Author != w.author {
			t.Errorf("[%d] Author = %q, want %q", i, got.Author, w.author)
		}
		if got.Content == "" {
			t.Errorf("[%d] Content is empty, expected the repo description", i)
		}
		var meta struct {
			Rank       int    `json:"rank"`
			StarsTotal int    `json:"stars_total"`
			Stars      int    `json:"stars"`
			Forks      int    `json:"forks"`
			Language   string `json:"language"`
		}
		if err := json.Unmarshal([]byte(got.MetadataJSON), &meta); err != nil {
			t.Fatalf("[%d] metadata is not valid JSON: %v", i, err)
		}
		if meta.Rank != w.rank {
			t.Errorf("[%d] metadata rank = %d, want %d", i, meta.Rank, w.rank)
		}
		if meta.StarsTotal != w.starsTotal {
			t.Errorf("[%d] metadata stars_total = %d, want %d", i, meta.StarsTotal, w.starsTotal)
		}
		if meta.Stars != w.starsDelta {
			t.Errorf("[%d] metadata stars (period delta) = %d, want %d", i, meta.Stars, w.starsDelta)
		}
		if meta.Forks != w.forks {
			t.Errorf("[%d] metadata forks = %d, want %d", i, meta.Forks, w.forks)
		}
		if meta.Language != w.language {
			t.Errorf("[%d] metadata language = %q, want %q", i, meta.Language, w.language)
		}
	}
}

// TestGitHubTrendingSource_StampsFetchTime — trending 是快照: repo 本身可能
// 是几年前的, 只有把 published_at 打成本次抓取时间, 它才不会被 24h 窗口
// 当成陈旧内容丢掉 (这正是本节长期空白的第二个隐患).
func TestGitHubTrendingSource_StampsFetchTime(t *testing.T) {
	fixture, err := os.ReadFile(fixtureTrendingHTML)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	before := time.Now().UTC().Add(-time.Minute)
	src := newTrendingTestSource(t, `{"url":"`+srv.URL+`/trending"}`)
	items, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	for i, it := range items {
		if it.PublishedAt.Before(before) {
			t.Errorf("[%d] PublishedAt = %v, expected it to be stamped at fetch time", i, it.PublishedAt)
		}
	}
}

// TestGitHubTrendingSource_AppendsSince 验证 ?since= 只拼给官方页面,
// JSON 代理不该被塞进它不认识的参数.
func TestGitHubTrendingSource_AppendsSince(t *testing.T) {
	fixture, err := os.ReadFile(fixtureTrendingHTML)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.RequestURI()
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	// httptest 的 host 是 127.0.0.1, 不满足 isGitHubTrendingPage, 因此这里
	// 直接验证判定函数本身, 而不是绕过它.
	if isGitHubTrendingPage(srv.URL + "/trending") {
		t.Errorf("isGitHubTrendingPage should be false for a non-github.com host")
	}
	_ = got

	src := newTrendingTestSource(t, `{"url":"https://github.com/trending","since":"daily"}`)
	if !isGitHubTrendingPage(src.cfg.URL) {
		t.Fatalf("isGitHubTrendingPage(%q) = false, want true", src.cfg.URL)
	}
	if url := withQueryParam(src.cfg.URL, "since", src.cfg.Since); url != "https://github.com/trending?since=daily" {
		t.Errorf("withQueryParam = %q", url)
	}
	if url := withQueryParam("https://github.com/trending?x=1", "since", "weekly"); url != "https://github.com/trending?x=1&since=weekly" {
		t.Errorf("withQueryParam with existing query = %q", url)
	}
}

// TestGitHubTrendingSource_JSONProxyStillWorks 确认这次改动没有砸掉原有的
// JSON 代理路径.
func TestGitHubTrendingSource_JSONProxyStillWorks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"repos":[
			{"full_name":"acme/one","description":"first","language":"Go","stars":4200,"forks":12,"currentPeriodStars":300}
		]}`))
	}))
	defer srv.Close()

	src := newTrendingTestSource(t, `{"url":"`+srv.URL+`/topone"}`)
	items, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 repo, got %d", len(items))
	}
	if items[0].Title != "acme/one" {
		t.Errorf("Title = %q", items[0].Title)
	}
	if !strings.Contains(items[0].MetadataJSON, `"stars_total":4200`) {
		t.Errorf("metadata = %s, want stars_total 4200", items[0].MetadataJSON)
	}
	if !strings.Contains(items[0].MetadataJSON, `"stars":300`) {
		t.Errorf("metadata = %s, want stars 300", items[0].MetadataJSON)
	}
}

// TestParseCount 覆盖真实页面上会出现的两种写法: 纯千分位数字, 以及带
// "stars today" 后缀的涨星数.
func TestParseCount(t *testing.T) {
	cases := map[string]int{
		"31,816":            31816,
		"3,231 stars today": 3231,
		"927 stars today":   927,
		"0":                 0,
		"":                  0,
		"no digits":         0,
		"1,234,567":         1234567,
	}
	for in, want := range cases {
		if got := parseCount(in); got != want {
			t.Errorf("parseCount(%q) = %d, want %d", in, got, want)
		}
	}
}

// TestGitHubTrendingSource_LivePage 打真实页面, 用来盯住这次换源最大的
// 残留风险: GitHub 改版后 article.Box-row / h2 a 这些选择器会静默匹配到 0 个,
// 而"0 个"和"页面挂了"在 CI 里长得一模一样.
//
// 默认跳过 (测试不该依赖外网), 需要时显式打开:
//
//	BRIEFING_LIVE_TRENDING=1 go test ./internal/ingest/ -run LivePage -v
func TestGitHubTrendingSource_LivePage(t *testing.T) {
	if os.Getenv("BRIEFING_LIVE_TRENDING") == "" {
		t.Skip("set BRIEFING_LIVE_TRENDING=1 to hit the live github.com/trending page")
	}

	src := newTrendingTestSource(t, `{"url":"https://github.com/trending","since":"daily"}`)
	items, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch live trending page: %v", err)
	}
	// 真实页面稳定给 25 条; 给一个宽松下界, 只用来发现"解析退化成 0/个位数".
	if len(items) < 10 {
		t.Fatalf("live page yielded only %d repos — selectors likely broke against a GitHub redesign", len(items))
	}
	for i, it := range items {
		if it.Title == "" || !strings.Contains(it.Title, "/") {
			t.Errorf("[%d] Title = %q, want owner/name", i, it.Title)
		}
		if !strings.HasPrefix(it.URL, "https://github.com/") {
			t.Errorf("[%d] URL = %q", i, it.URL)
		}
		if !strings.Contains(it.MetadataJSON, `"rank":`) {
			t.Errorf("[%d] metadata lacks rank: %s", i, it.MetadataJSON)
		}
		if !strings.Contains(it.MetadataJSON, `"stars_total":`) {
			t.Errorf("[%d] metadata lacks stars_total: %s", i, it.MetadataJSON)
		}
	}
	t.Logf("live page parsed %d repos; top: %s (%s)", len(items), items[0].Title, items[0].MetadataJSON)
}

// TestParseTrendingPayload_RejectsEmpty 确认空响应是错误而不是 0 条 —
// 上游返回空时必须让 pipeline 记 WARN, 而不是安静地发一个空板块.
func TestParseTrendingPayload_RejectsEmpty(t *testing.T) {
	if _, err := parseTrendingPayload([]byte("   ")); err == nil {
		t.Error("expected an error for an empty body")
	}
	if _, err := parseTrendingPayload([]byte("<html><body>no repos here</body></html>")); err == nil {
		t.Error("expected an error when the HTML contains no trending repos")
	}
	if _, err := parseTrendingPayload([]byte(`{"data":{"rows":[]}}`)); err == nil {
		t.Error("expected an error for a JSON envelope with no repos")
	}
}
