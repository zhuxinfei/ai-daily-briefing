package render

import (
	"strings"
	"testing"
	"time"
)

// 2026-04-27 实测 daily 底部周报链接缺 site path 前缀, 浏览器解析到
// ylzsdafei.github.io 顶级域撞 GitHub Site not found. 这组测试守门:
// 永远不准 weeklyBackLink 返回裸 host-relative `/blog/weekly/...`,
// 防止 hugo.go 重构时悄悄 regress.

func TestWeeklyBackLink_FullURLFromEnv(t *testing.T) {
	t.Setenv("BRIEFING_REPORT_URL_BASE",
		"https://example.com/site/{{YEAR}}/{{YEARMONTH}}/{{DATE}}/")

	date := time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC) // ISO W18
	got := weeklyBackLink(date)
	want := "https://example.com/site/blog/weekly/2026-w18/"
	if got != want {
		t.Errorf("env 正常时应该用 site root 拼完整 URL\nwant: %q\ngot:  %q", want, got)
	}
}

func TestWeeklyBackLink_FallbackHasSitePathPrefix(t *testing.T) {
	t.Setenv("BRIEFING_REPORT_URL_BASE", "")

	date := time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC)
	got := weeklyBackLink(date)

	// fallback 必须带 site path 前缀, 不能是裸 host-relative `/blog/weekly/...`.
	// 否则浏览器解析到 ylzsdafei.github.io 顶级域 → GitHub Site not found.
	if strings.HasPrefix(got, "/blog/") {
		t.Errorf("fallback 不能返回裸 host-relative %q (会撞 GitHub 顶级 404)", got)
	}
	if !strings.HasPrefix(got, weeklyBackLinkFallbackPathPrefix+"/") &&
		!strings.HasPrefix(got, "https://") {
		t.Errorf("fallback 必须以 %q 或 https:// 开头, got %q",
			weeklyBackLinkFallbackPathPrefix, got)
	}
	wantSuffix := "/blog/weekly/2026-w18/"
	if !strings.HasSuffix(got, wantSuffix) {
		t.Errorf("fallback URL 末段必须包含 %q, got %q", wantSuffix, got)
	}
}

func TestWeeklyBackLink_NoBareHostRelativeOnMalformedEnv(t *testing.T) {
	// env 设了但格式错 (缺 `{{` 占位符), 不能 fallback 到裸 host-relative.
	t.Setenv("BRIEFING_REPORT_URL_BASE", "https://example.com/site/no-placeholder")

	date := time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC)
	got := weeklyBackLink(date)
	if strings.HasPrefix(got, "/blog/") {
		t.Errorf("env 格式错时不能 fallback 到裸 host-relative %q", got)
	}
	// 应当走 fallback site path 分支.
	if !strings.HasPrefix(got, weeklyBackLinkFallbackPathPrefix+"/") {
		t.Errorf("env 格式错时应 fallback 到 %q, got %q",
			weeklyBackLinkFallbackPathPrefix, got)
	}
}

func TestWeeklyBackLink_ISOWeekBoundary(t *testing.T) {
	t.Setenv("BRIEFING_REPORT_URL_BASE",
		"https://example.com/site/{{YEAR}}/{{YEARMONTH}}/{{DATE}}/")

	cases := []struct {
		name string
		date time.Time
		want string
	}{
		{
			name: "周一_W18第一天",
			date: time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC),
			want: "https://example.com/site/blog/weekly/2026-w18/",
		},
		{
			name: "周日_W17最后一天",
			date: time.Date(2026, 4, 26, 0, 0, 0, 0, time.UTC),
			want: "https://example.com/site/blog/weekly/2026-w17/",
		},
		{
			name: "ISO跨年_2026-W01",
			date: time.Date(2025, 12, 30, 0, 0, 0, 0, time.UTC), // ISO 2026-W01-Tue
			want: "https://example.com/site/blog/weekly/2026-w01/",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := weeklyBackLink(c.date)
			if got != c.want {
				t.Errorf("want %q, got %q", c.want, got)
			}
		})
	}
}
