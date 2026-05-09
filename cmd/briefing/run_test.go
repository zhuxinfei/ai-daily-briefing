package main

import (
	"context"
	"testing"
	"time"

	"briefing-v3/internal/gate"
	"briefing-v3/internal/publish"
	"briefing-v3/internal/store"
)

// TestExtractDateFromURL 验证 T18: filter 多角度时效性 — URL 路径日期解析
// 覆盖 4 种主流格式 (TechCrunch/Substack/Simon Willison/arxiv).
func TestExtractDateFromURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want time.Time
		ok   bool
	}{
		{
			name: "techcrunch_slash_format",
			url:  "https://techcrunch.com/2026/04/13/openai-bought-hiro/",
			want: time.Date(2026, 4, 13, 0, 0, 0, 0, time.UTC),
			ok:   true,
		},
		{
			name: "simonw_month_abbr",
			url:  "https://simonwillison.net/2026/Apr/14/cybersecurity/",
			want: time.Date(2026, 4, 14, 0, 0, 0, 0, time.UTC),
			ok:   true,
		},
		{
			name: "substack_dash",
			url:  "https://example.substack.com/p/2026-04-15-claude-update/",
			want: time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC),
			ok:   true,
		},
		{
			// 2026-04-16 修复: arxiv ID 不编码日号, 不再返回伪日期 (day=1
			// 会让 filter 误判当月中下旬论文为 "旧文" drop 掉). arxiv 的
			// RSS pubDate 本身可靠, 不走 URL sanity check.
			name: "arxiv_yymm_skipped",
			url:  "https://arxiv.org/abs/2604.11465",
			want: time.Time{},
			ok:   false,
		},
		{
			name: "arxiv_old_yymm_skipped",
			url:  "https://arxiv.org/abs/2401.99999",
			want: time.Time{},
			ok:   false,
		},
		{
			name: "no_date_in_url",
			url:  "https://github.com/google/magika",
			want: time.Time{},
			ok:   false,
		},
		{
			name: "invalid_month",
			url:  "https://x.com/2026/13/05/post/",
			want: time.Time{},
			ok:   false,
		},
		{
			name: "empty_url",
			url:  "",
			want: time.Time{},
			ok:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := extractDateFromURL(tc.url)
			if ok != tc.ok {
				t.Fatalf("ok=%v want=%v (url %q)", ok, tc.ok, tc.url)
			}
			if ok && !got.Equal(tc.want) {
				t.Errorf("got=%v want=%v", got, tc.want)
			}
		})
	}
}

// TestFilterByWindow_StrictPublishedAt 验证 T1: 零 PublishedAt 直接 drop,
// 不再用 FetchedAt 兜底.
func TestFilterByWindow_StrictPublishedAt(t *testing.T) {
	cutoff := time.Date(2026, 4, 14, 0, 0, 0, 0, time.UTC)
	items := []*store.RawItem{
		{ID: 1, Title: "fresh", URL: "https://x.com/fresh", PublishedAt: cutoff.Add(2 * time.Hour), FetchedAt: cutoff.Add(2 * time.Hour)},
		{ID: 2, Title: "stale", URL: "https://x.com/stale", PublishedAt: cutoff.Add(-25 * time.Hour), FetchedAt: cutoff.Add(2 * time.Hour)},
		{ID: 3, Title: "unknown_pub_with_fetch", URL: "https://x.com/unknown", PublishedAt: time.Time{}, FetchedAt: cutoff.Add(2 * time.Hour)},
	}
	out := filterByWindow(items, cutoff)
	if len(out) != 1 {
		t.Fatalf("expected 1 item passes (only 'fresh'), got %d: %v", len(out), out)
	}
	if out[0].Title != "fresh" {
		t.Errorf("expected 'fresh', got %q", out[0].Title)
	}
}

// TestFilterByWindow_URLDateSanityCheck 验证 T18: PublishedAt 看似新但 URL
// 路径日期老于 cutoff → drop (catches RSS 重发旧文).
func TestFilterByWindow_URLDateSanityCheck(t *testing.T) {
	cutoff := time.Date(2026, 4, 14, 0, 0, 0, 0, time.UTC)
	items := []*store.RawItem{
		{
			ID: 1, Title: "republished old article",
			URL:         "https://techcrunch.com/2024/12/05/old-news/",
			PublishedAt: cutoff.Add(2 * time.Hour),
			FetchedAt:   cutoff.Add(2 * time.Hour),
		},
		{
			ID: 2, Title: "real fresh article",
			URL:         "https://techcrunch.com/2026/04/14/new-news/",
			PublishedAt: cutoff.Add(2 * time.Hour),
			FetchedAt:   cutoff.Add(2 * time.Hour),
		},
	}
	out := filterByWindow(items, cutoff)
	if len(out) != 1 {
		t.Fatalf("URL date sanity should drop the 2024 article; got %d items: %v", len(out), out)
	}
	if out[0].Title != "real fresh article" {
		t.Errorf("expected 'real fresh article' to survive, got %q", out[0].Title)
	}
}

func TestGateFailureBlocksRun(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		target string
		report *gate.Report
		want   bool
	}{
		{
			name:   "auto_warn_is_non_fatal",
			target: "auto",
			report: &gate.Report{Pass: false, Warn: true, Warnings: []string{"section 覆盖不足"}},
			want:   false,
		},
		{
			name:   "prod_warn_is_non_fatal",
			target: "prod",
			report: &gate.Report{Pass: false, Warn: true, Warnings: []string{"section 覆盖不足"}},
			want:   false,
		},
		{
			name:   "auto_hard_fail_blocks",
			target: "auto",
			report: &gate.Report{Pass: false, Warn: false, Reasons: []string{"条目为零"}},
			want:   true,
		},
		{
			name:   "pass_never_blocks",
			target: "auto",
			report: &gate.Report{Pass: true},
			want:   false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := gateFailureBlocksRun(tc.target, tc.report); got != tc.want {
				t.Fatalf("gateFailureBlocksRun(%q) = %v, want %v", tc.target, got, tc.want)
			}
		})
	}
}

func TestGateFailureDetail(t *testing.T) {
	t.Parallel()

	if got := gateFailureDetail(&gate.Report{Pass: false, Warn: true, Warnings: []string{"条目不足", "section 覆盖不足"}}); got != "条目不足; section 覆盖不足" {
		t.Fatalf("unexpected warn detail: %q", got)
	}
	if got := gateFailureDetail(&gate.Report{Pass: false, Warn: false, Reasons: []string{"条目为零"}}); got != "条目为零" {
		t.Fatalf("unexpected fail detail: %q", got)
	}
}

func TestShouldPostGateAlert(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		report *gate.Report
		want   bool
	}{
		{
			name:   "nil_report",
			report: nil,
			want:   false,
		},
		{
			name:   "pass",
			report: &gate.Report{Pass: true},
			want:   false,
		},
		{
			name:   "warn",
			report: &gate.Report{Pass: false, Warn: true},
			want:   true,
		},
		{
			name:   "hard_fail",
			report: &gate.Report{Pass: false, Warn: false},
			want:   true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldPostGateAlert(tc.report); got != tc.want {
				t.Fatalf("shouldPostGateAlert() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProdPublishIssues(t *testing.T) {
	t.Run("complete_modules_and_public_link", func(t *testing.T) {
		origProbe := urlProbe
		urlProbe = func(ctx context.Context, method, rawURL string) (int, error) {
			return 200, nil
		}
		defer func() { urlProbe = origProbe }()

		rendered := &publish.RenderedIssue{
			Issue: &store.Issue{Summary: "1. 今日摘要"},
			Insight: &store.IssueInsight{
				IndustryMD: "1. 行业洞察",
				OurMD:      "1. 对我们的启发",
			},
			ReportURL: "https://briefing.example.com/2026/2026-04/2026-04-12/",
		}
		if issues := prodPublishIssues(context.Background(), rendered); len(issues) != 0 {
			t.Fatalf("prodPublishIssues() = %v, want empty", issues)
		}
	})

	t.Run("missing_modules_and_bad_link", func(t *testing.T) {
		rendered := &publish.RenderedIssue{
			Issue:     &store.Issue{},
			Insight:   &store.IssueInsight{},
			ReportURL: "file:///tmp/report.html",
		}
		issues := prodPublishIssues(context.Background(), rendered)
		// #2: checkPublicReportURL 降级为 warn-only 后不再计入 issues,
		// 只剩 3 条 (industry/our/summary 缺失).
		if len(issues) != 3 {
			t.Fatalf("prodPublishIssues() len = %d, want 3; issues=%v", len(issues), issues)
		}
	})
}
