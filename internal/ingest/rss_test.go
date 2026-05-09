// rss_test.go — unit tests for v1.0.1 Phase 1.1 RSS adapter:
// filterItemsByAge helper that drops stale items to cap feeds returning
// full historical archive (OpenAI News 935 / smol.ai 611 at adapter time).
//
// Design: filter logic is a pure function on []*gofeed.Item so these tests
// do not need an httptest.Server. The Fetch() integration path is verified
// via the existing briefing run --dry-run smoke test.
//
// Run with: go test ./internal/ingest/ -run FilterItemsByAge -v

package ingest

import (
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
)

func TestFilterItemsByAge(t *testing.T) {
	now := time.Now().UTC()
	cutoff := now.Add(-48 * time.Hour)

	mkWithPub := func(offsetHours int) *gofeed.Item {
		t := now.Add(time.Duration(offsetHours) * time.Hour)
		return &gofeed.Item{Title: "pub", PublishedParsed: &t}
	}
	mkWithUpd := func(offsetHours int) *gofeed.Item {
		t := now.Add(time.Duration(offsetHours) * time.Hour)
		return &gofeed.Item{Title: "upd", UpdatedParsed: &t}
	}
	mkNilDate := func() *gofeed.Item {
		return &gofeed.Item{Title: "no-date", PublishedParsed: nil, UpdatedParsed: nil}
	}

	tests := []struct {
		name      string
		items     []*gofeed.Item
		wantCount int
	}{
		{
			name:      "all fresh within cutoff",
			items:     []*gofeed.Item{mkWithPub(-1), mkWithPub(-10), mkWithPub(-30)},
			wantCount: 3,
		},
		{
			name:      "mixed ages — fresh kept, stale dropped",
			items:     []*gofeed.Item{mkWithPub(-1), mkWithPub(-50), mkWithPub(-100), mkWithPub(-10)},
			wantCount: 2, // -1h and -10h survive; -50h and -100h dropped
		},
		{
			name:      "all stale — all dropped",
			items:     []*gofeed.Item{mkWithPub(-100), mkWithPub(-1000)},
			wantCount: 0,
		},
		{
			name:      "exactly at cutoff boundary — kept (Before=strict)",
			items:     []*gofeed.Item{{Title: "at-cutoff", PublishedParsed: &cutoff}},
			wantCount: 1,
		},
		{
			name:      "item with nil PublishedParsed but UpdatedParsed set — uses Updated",
			items:     []*gofeed.Item{mkWithUpd(-1), mkWithUpd(-100)},
			wantCount: 1, // -1h kept, -100h dropped
		},
		{
			name:      "item with both nil — kept (conservative, downstream re-checks)",
			items:     []*gofeed.Item{mkNilDate(), mkNilDate()},
			wantCount: 2,
		},
		{
			name:      "nil item in slice — skipped cleanly",
			items:     []*gofeed.Item{nil, mkWithPub(-1), nil},
			wantCount: 1,
		},
		{
			name:      "empty input",
			items:     []*gofeed.Item{},
			wantCount: 0,
		},
		{
			name: "simulates OpenAI News scenario — 5 fresh + 5 years-old stale",
			items: []*gofeed.Item{
				mkWithPub(-1), mkWithPub(-2), mkWithPub(-3), mkWithPub(-4), mkWithPub(-5),
				// 3 years ago = -26280h
				mkWithPub(-26280), mkWithPub(-26280), mkWithPub(-26280), mkWithPub(-26280), mkWithPub(-26280),
			},
			wantCount: 5,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := filterItemsByAge(tc.items, cutoff)
			if len(got) != tc.wantCount {
				t.Errorf("filterItemsByAge: got %d items, want %d (titles: %v)",
					len(got), tc.wantCount, itemTitles(got))
			}
		})
	}
}

// itemTitles is a small helper for readable test failure messages.
func itemTitles(items []*gofeed.Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		if it == nil {
			continue
		}
		out = append(out, it.Title)
	}
	return out
}
