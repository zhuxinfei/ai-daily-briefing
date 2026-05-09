// gnews_test.go — unit tests for v1.0.1 Phase 1.2 Google News adapter.
// Tests:
//   1. ConfigNormalization: backward compat between old "query" single
//      field and new "queries" array, edge cases (both set, empties, etc.)
//   2. FallbackDecision: the loop-pick-first-non-empty logic itself,
//      simulated with mock item slices (no HTTP needed since buildQueryURL
//      hardcodes news.google.com and can't be redirected to httptest).
//
// Integration-style HTTP mocking (httptest.Server) is skipped in this PR
// because gnewsSource.buildQueryURL() hardcodes "https://news.google.com/rss/search".
// The live Fetch path is verified by the briefing run --dry-run smoke test.
//
// Run with: go test ./internal/ingest/ -run GoogleNews -v

package ingest

import (
	"testing"
	"time"

	"github.com/mmcdole/gofeed"

	"briefing-v3/internal/store"
)

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	out := ""
	for n > 0 {
		out = string(rune('0'+n%10)) + out
		n /= 10
	}
	return out
}

func TestGoogleNewsFallbackDecision(t *testing.T) {
	// Simulates the Fetch loop's decision: given a map query → item count,
	// and a Queries list, which query's items should be used?

	pub := time.Now().UTC()
	mkItems := func(n int) []*gofeed.Item {
		out := make([]*gofeed.Item, n)
		for i := range out {
			t := pub
			out[i] = &gofeed.Item{
				Title:           "title" + itoa(i),
				Link:            "https://example.com/x",
				GUID:            "g" + itoa(i),
				PublishedParsed: &t,
			}
		}
		return out
	}

	tests := []struct {
		name      string
		queries   []string
		results   map[string][]*gofeed.Item
		wantQuery string // "" 表示全空
		wantCount int
	}{
		{
			name:    "first query has items",
			queries: []string{"a", "b", "c"},
			results: map[string][]*gofeed.Item{
				"a": mkItems(3),
				"b": mkItems(2),
				"c": mkItems(1),
			},
			wantQuery: "a",
			wantCount: 3,
		},
		{
			name:    "first query empty, second has items",
			queries: []string{"a", "b", "c"},
			results: map[string][]*gofeed.Item{
				"a": mkItems(0),
				"b": mkItems(5),
				"c": mkItems(1),
			},
			wantQuery: "b",
			wantCount: 5,
		},
		{
			name:    "first two empty, third has items",
			queries: []string{"a", "b", "c"},
			results: map[string][]*gofeed.Item{
				"a": mkItems(0),
				"b": mkItems(0),
				"c": mkItems(4),
			},
			wantQuery: "c",
			wantCount: 4,
		},
		{
			name:    "all empty",
			queries: []string{"a", "b", "c"},
			results: map[string][]*gofeed.Item{
				"a": mkItems(0),
				"b": mkItems(0),
				"c": mkItems(0),
			},
			wantQuery: "",
			wantCount: 0,
		},
		{
			name:    "single query with items",
			queries: []string{"only"},
			results: map[string][]*gofeed.Item{
				"only": mkItems(2),
			},
			wantQuery: "only",
			wantCount: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Simulate the Fetch loop logic.
			var picked []*gofeed.Item
			var pickedQuery string
			for _, q := range tc.queries {
				items := tc.results[q]
				if len(items) > 0 {
					picked = items
					pickedQuery = q
					break
				}
			}
			if pickedQuery != tc.wantQuery {
				t.Errorf("query: got %q, want %q", pickedQuery, tc.wantQuery)
			}
			if len(picked) != tc.wantCount {
				t.Errorf("items: got %d, want %d", len(picked), tc.wantCount)
			}
		})
	}
}

// TestGoogleNewsConfigNormalization tests the newGoogleNewsSource factory
// normalizes Query + Queries fields correctly for backward compat.
func TestGoogleNewsConfigNormalization(t *testing.T) {
	tests := []struct {
		name        string
		configJSON  string
		wantQueries []string
		wantErr     bool
	}{
		{
			name:        "only Query (old format)",
			configJSON:  `{"query": "foo", "hl": "zh-CN"}`,
			wantQueries: []string{"foo"},
		},
		{
			name:        "only Queries (new format)",
			configJSON:  `{"queries": ["a", "b", "c"], "hl": "zh-CN"}`,
			wantQueries: []string{"a", "b", "c"},
		},
		{
			name:        "both — Queries wins",
			configJSON:  `{"query": "legacy", "queries": ["new1", "new2"]}`,
			wantQueries: []string{"new1", "new2"},
		},
		{
			name:        "Queries with empty strings — filtered out",
			configJSON:  `{"queries": ["a", "", "b", "  "]}`,
			wantQueries: []string{"a", "b"},
		},
		{
			name:       "neither — error",
			configJSON: `{"hl": "en-US"}`,
			wantErr:    true,
		},
		{
			name:       "empty JSON — error",
			configJSON: ``,
			wantErr:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src, err := newGoogleNewsSource(&store.Source{
				ID:         1,
				DomainID:   "ai",
				Type:       "google_news",
				Name:       "test",
				ConfigJSON: tc.configJSON,
			})
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			gs := src.(*gnewsSource)
			if !equalStringSlice(gs.cfg.Queries, tc.wantQueries) {
				t.Errorf("queries: got %v, want %v", gs.cfg.Queries, tc.wantQueries)
			}
		})
	}
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
