package ingest

import (
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// TestParseAnthropicDateText 验证 T4 anthropic listing 卡片日期解析.
func TestParseAnthropicDateText(t *testing.T) {
	cases := []struct {
		input string
		want  time.Time
		ok    bool
	}{
		{"Apr 14, 2026", time.Date(2026, 4, 14, 0, 0, 0, 0, time.UTC), true},
		{"April 14, 2026", time.Date(2026, 4, 14, 0, 0, 0, 0, time.UTC), true},
		{"feb 5, 2026", time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC), true},
		{"Mar 18, 2026", time.Date(2026, 3, 18, 0, 0, 0, 0, time.UTC), true},
		{"junk text no date", time.Time{}, false},
		{"", time.Time{}, false},
		{"2026-04-14", time.Time{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, ok := parseAnthropicDateText(tc.input)
			if ok != tc.ok {
				t.Fatalf("ok=%v want=%v (input %q)", ok, tc.ok, tc.input)
			}
			if ok && !got.Equal(tc.want) {
				t.Errorf("got=%v want=%v", got, tc.want)
			}
		})
	}
}

// TestExtractAnthropicDate 验证 T4 在真实 anthropic listing HTML 结构上能拿到日期.
func TestExtractAnthropicDate(t *testing.T) {
	html := `
<div class="card">
  <a href="/news/test-slug">Test Title</a>
  <time class="PublicationList-module-scss-module__KxYrHG__date body-3">Apr 14, 2026</time>
</div>`
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		t.Fatalf("parse html: %v", err)
	}
	a := doc.Find(`a[href^="/news/"]`).First()
	got := extractAnthropicDate(a)
	want := time.Date(2026, 4, 14, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got=%v want=%v", got, want)
	}
}

func TestExtractAnthropicDate_MissingTime(t *testing.T) {
	html := `<div><a href="/news/test">no date here</a></div>`
	doc, _ := goquery.NewDocumentFromReader(strings.NewReader(html))
	a := doc.Find(`a[href^="/news/"]`).First()
	got := extractAnthropicDate(a)
	if !got.IsZero() {
		t.Errorf("expected zero time when no <time>, got %v", got)
	}
}
