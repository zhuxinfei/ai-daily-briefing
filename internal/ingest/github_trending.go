package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"briefing-v3/internal/store"
)

// githubTrendingConfig is the JSON shape stored in Source.ConfigJSON for
// type "github_trending".
//
// Since selects the window GitHub exposes as a query parameter:
// daily | weekly | monthly. Leaving it empty leaves GitHub its own default.
type githubTrendingConfig struct {
	URL   string `json:"url"`
	Since string `json:"since"`
}

// trendingEntry is one repo read off the trending page, before it becomes a
// RawItem. Keeping the scrape and the row-building apart is what lets the
// parser be tested against a captured page without a store.
//
// StarsTotal and StarsDelta are not decoration: run.go reads them back out of
// metadata_json (repoMetadataSignals, formatRepoHotnessExtra) to rank the
// section and to shortlist candidates for the opensource coverage rescue.
type trendingEntry struct {
	FullName    string
	Description string
	Language    string
	StarsTotal  int // lifetime stargazers
	Forks       int
	StarsDelta  int // growth inside the trending window ("N stars today")
}

// githubTrendingSource pulls the trending list from github.com.
type githubTrendingSource struct {
	row *store.Source
	cfg githubTrendingConfig
	hc  *http.Client
}

func newGitHubTrendingSource(row *store.Source) (Source, error) {
	var cfg githubTrendingConfig
	if strings.TrimSpace(row.ConfigJSON) == "" {
		return nil, fmt.Errorf("github_trending: empty ConfigJSON for source %d", row.ID)
	}
	if err := json.Unmarshal([]byte(row.ConfigJSON), &cfg); err != nil {
		return nil, fmt.Errorf("github_trending: parse ConfigJSON: %w", err)
	}
	if cfg.URL == "" {
		return nil, fmt.Errorf("github_trending: ConfigJSON.url is required for source %d", row.ID)
	}
	return &githubTrendingSource{
		row: row,
		cfg: cfg,
		// 20s: the trending page is ~700 KB of HTML, noticeably heavier than
		// the JSON endpoints the other adapters talk to.
		hc: &http.Client{Timeout: 20 * time.Second},
	}, nil
}

func (s *githubTrendingSource) ID() int64    { return s.row.ID }
func (s *githubTrendingSource) Type() string { return s.row.Type }
func (s *githubTrendingSource) Name() string { return s.row.Name }

func (s *githubTrendingSource) Fetch(ctx context.Context) ([]*store.RawItem, error) {
	target := s.cfg.URL
	if since := strings.TrimSpace(s.cfg.Since); since != "" {
		target = withQueryParam(target, "since", since)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("github_trending: new request: %w", err)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	// github.com serves the trend list to anonymous clients, but a browser-ish
	// UA keeps us off the bot-shaped response path.
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; briefing-v3/0.1; +github_trending)")

	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github_trending: fetch %s: %w", target, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("github_trending: unexpected status %d from %s", resp.StatusCode, target)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("github_trending: read body: %w", err)
	}

	entries, err := parseTrendingHTML(body)
	if err != nil {
		return nil, fmt.Errorf("github_trending: decode %s: %w", target, err)
	}

	now := time.Now().UTC()
	items := make([]*store.RawItem, 0, len(entries))
	for i, e := range entries {
		if e.FullName == "" {
			continue
		}
		content := strings.TrimSpace(e.Description)
		if content == "" && e.Language != "" {
			content = "语言: " + e.Language
		}
		metaJSON, _ := json.Marshal(map[string]any{
			"language":    e.Language,
			"stars":       e.StarsDelta, // period growth
			"stars_total": e.StarsTotal, // lifetime stars
			"forks":       e.Forks,
			"rank":        i + 1,
		})

		items = append(items, &store.RawItem{
			DomainID:   s.row.DomainID,
			SourceID:   s.row.ID,
			ExternalID: e.FullName,
			URL:        "https://github.com/" + e.FullName,
			Title:      e.FullName,
			Author:     splitOwner(e.FullName),
			// A trending list is a snapshot, not a publication: the repos on
			// it are frequently years old and their pushed_at has nothing to
			// do with why they are trending today. Stamping the fetch time is
			// what keeps them inside the run's lookback window instead of
			// being dropped as stale.
			PublishedAt:  now,
			FetchedAt:    now,
			Content:      content,
			MetadataJSON: string(metaJSON),
		})
	}
	return items, nil
}

// parseTrendingHTML reads github.com/trending. The page has no API and needs
// no key; it is the same ranking ossinsight used to proxy.
//
// Returning an error on an empty page rather than an empty slice is what makes
// a dead feed visible: ingestAll logs it as a source failure, and the section
// it feeds would otherwise just render blank.
func parseTrendingHTML(body []byte) ([]trendingEntry, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var out []trendingEntry
	doc.Find("article.Box-row").Each(func(_ int, art *goquery.Selection) {
		full := trendingRepoFullName(art)
		if full == "" {
			return
		}
		out = append(out, trendingEntry{
			FullName:    full,
			Description: strings.TrimSpace(art.Find("p.col-9").First().Text()),
			Language:    strings.TrimSpace(art.Find(`span[itemprop="programmingLanguage"]`).First().Text()),
			StarsTotal:  parseCount(art.Find(`a[href$="/stargazers"]`).First().Text()),
			Forks:       parseCount(art.Find(`a[href$="/forks"]`).First().Text()),
			StarsDelta:  parseCount(art.Find("span.float-sm-right").First().Text()),
		})
	})
	if len(out) == 0 {
		return nil, fmt.Errorf("no trending repos found in HTML")
	}
	return out, nil
}

// trendingRepoFullName pulls "owner/name" out of an article heading. The href
// is authoritative: the visible text splits the owner into a <span> and
// re-joins it with a literal " / ", which is far easier to mangle.
func trendingRepoFullName(art *goquery.Selection) string {
	var full string
	art.Find("h2 a[href]").EachWithBreak(func(_ int, a *goquery.Selection) bool {
		href, _ := a.Attr("href")
		href = strings.Trim(strings.TrimSpace(href), "/")
		if i := strings.IndexAny(href, "?#"); i >= 0 {
			href = href[:i]
		}
		if parts := strings.Split(href, "/"); len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			full = parts[0] + "/" + parts[1]
			return false
		}
		return true
	})
	return full
}

// parseCount reads the first number in a cell, tolerating thousands
// separators and any trailing label: "31,816" and "3,231 stars today" both
// parse to the integer a human would read. Anything unparseable is 0, which
// every downstream consumer already treats as "unknown".
func parseCount(s string) int {
	var b strings.Builder
	started := false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			started = true
		case r == ',' && started:
			// thousands separator — skip
		default:
			if started {
				n, _ := strconv.Atoi(b.String())
				return n
			}
		}
	}
	n, _ := strconv.Atoi(b.String())
	return n
}

// withQueryParam appends key=value to rawURL, choosing the separator from
// whether a query string is already present. Shared with the ossinsight
// adapter, which appends ?period= the same way.
func withQueryParam(rawURL, key, value string) string {
	sep := "?"
	if strings.Contains(rawURL, "?") {
		sep = "&"
	}
	return rawURL + sep + key + "=" + url.QueryEscape(value)
}

// splitOwner returns the "owner" half of an "owner/name" slug. Shared with the
// ossinsight adapter.
func splitOwner(full string) string {
	if i := strings.IndexByte(full, '/'); i > 0 {
		return full[:i]
	}
	return ""
}

func init() {
	Register("github_trending", Factory(newGitHubTrendingSource))
}
