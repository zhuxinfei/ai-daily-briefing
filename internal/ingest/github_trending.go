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
// Two upstream shapes are served by the same adapter, picked by sniffing the
// response body rather than by configuration:
//
//   - the official HTML page https://github.com/trending — no key, no rate
//     limit, and the same ranking the ossinsight trends endpoint used to
//     proxy before that endpoint was retired (2026-09).
//   - a topone-style JSON proxy, e.g.
//     https://git-trending.justlikemaki.vip/topone/?since=daily
//
// Since is only meaningful for the HTML page, which exposes the window as a
// query parameter: daily | weekly | monthly.
type githubTrendingConfig struct {
	URL   string `json:"url"`
	Since string `json:"since"`
}

// githubTrendingRepo is the per-repo shape returned by the trending proxy.
// The upstream API is a small scraper whose schema is not officially
// documented; field tags try both snake_case and camelCase so a schema drift
// on the remote side is less likely to break us silently.
//
// Some fields are decoded but not read by this adapter — they are kept so the
// proxy's contract stays visible and drift keeps decoding instead of failing.
type githubTrendingRepo struct {
	Author       string `json:"author"`
	Name         string `json:"name"`
	FullName     string `json:"fullName"`
	FullNameSnk  string `json:"full_name"`
	URL          string `json:"url"`
	HTMLURL      string `json:"html_url"`
	Description  string `json:"description"`
	Language     string `json:"language"`
	Stars        int    `json:"stars"`
	StargazerCnt int    `json:"stargazers_count"`
	Forks        int    `json:"forks"`
	ForksCount   int    `json:"forks_count"`
	CurrentStars int    `json:"currentPeriodStars"`
	PushedAt     string `json:"pushed_at"`
	UpdatedAt    string `json:"updated_at"`
}

// githubTrendingEnvelope handles the case where the response is wrapped,
// e.g. { "repos": [...] } or { "data": [...] } instead of a bare array.
type githubTrendingEnvelope struct {
	Repos []githubTrendingRepo `json:"repos"`
	Data  []githubTrendingRepo `json:"data"`
	Items []githubTrendingRepo `json:"items"`
}

// trendingEntry is the normalized shape both parsers produce, and the only
// thing the item builder downstream sees.
//
// Rank and StarsDelta are not decoration: run.go reads both back out of
// metadata_json (repoMetadataSignals, formatRepoHotnessExtra) to rank the
// section and to shortlist candidates for the opensource coverage rescue.
type trendingEntry struct {
	FullName    string
	Description string
	Language    string
	StarsTotal  int // lifetime stargazers
	Forks       int
	StarsDelta  int // growth inside the trending window ("N stars today")
	Rank        int // 1-based position on the trending page
}

// githubTrendingSource pulls daily trending repos from github.com or a
// topone-style proxy.
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
	if isGitHubTrendingPage(target) {
		if since := strings.TrimSpace(s.cfg.Since); since != "" {
			target = withQueryParam(target, "since", since)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("github_trending: new request: %w", err)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json;q=0.9")
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

	entries, err := parseTrendingPayload(body)
	if err != nil {
		return nil, fmt.Errorf("github_trending: decode %s: %w", target, err)
	}

	now := time.Now().UTC()
	items := make([]*store.RawItem, 0, len(entries))
	for i, e := range entries {
		if e.FullName == "" {
			continue
		}
		rank := e.Rank
		if rank == 0 {
			rank = i + 1
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
			"rank":        rank,
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

// parseTrendingPayload sniffs the body shape so a single adapter can serve
// both the HTML page and the JSON proxy without a config flag to keep in sync.
func parseTrendingPayload(body []byte) ([]trendingEntry, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return nil, fmt.Errorf("empty body")
	}
	if strings.HasPrefix(trimmed, "<") {
		return parseTrendingHTML(body)
	}
	return parseTrendingJSON(body)
}

func parseTrendingJSON(body []byte) ([]trendingEntry, error) {
	repos, err := decodeTrendingRepos(body)
	if err != nil {
		return nil, err
	}
	out := make([]trendingEntry, 0, len(repos))
	for i := range repos {
		r := repos[i]
		full := repoFullName(&r)
		if full == "" {
			continue
		}
		out = append(out, trendingEntry{
			FullName:    full,
			Description: r.Description,
			Language:    r.Language,
			// The proxy reports either lifetime stars (stars/stargazers_count)
			// or window growth (currentPeriodStars) depending on which
			// scraper is behind it. Keep them in their own fields so the
			// ranker is never told a delta is a total.
			StarsTotal: firstNonZero(r.Stars, r.StargazerCnt),
			Forks:      firstNonZero(r.Forks, r.ForksCount),
			StarsDelta: r.CurrentStars,
			Rank:       i + 1,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no repos found in response")
	}
	return out, nil
}

// parseTrendingHTML reads github.com/trending. The page has no API and needs
// no key; it is the same ranking ossinsight used to proxy.
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
			Rank:        len(out) + 1,
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

// isGitHubTrendingPage reports whether the URL points at the official HTML
// trending page — the only endpoint that understands ?since=.
func isGitHubTrendingPage(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "github.com", "www.github.com":
		return strings.HasPrefix(strings.Trim(u.Path, "/"), "trending")
	}
	return false
}

func withQueryParam(rawURL, key, value string) string {
	sep := "?"
	if strings.Contains(rawURL, "?") {
		sep = "&"
	}
	return rawURL + sep + key + "=" + url.QueryEscape(value)
}

// decodeTrendingRepos handles the two common envelope shapes: a bare JSON
// array of repos, or an object wrapping the array under "repos"/"data"/"items".
func decodeTrendingRepos(body []byte) ([]githubTrendingRepo, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return nil, fmt.Errorf("empty body")
	}
	if strings.HasPrefix(trimmed, "[") {
		var arr []githubTrendingRepo
		if err := json.Unmarshal(body, &arr); err != nil {
			return nil, err
		}
		return arr, nil
	}
	var env githubTrendingEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, err
	}
	switch {
	case len(env.Repos) > 0:
		return env.Repos, nil
	case len(env.Data) > 0:
		return env.Data, nil
	case len(env.Items) > 0:
		return env.Items, nil
	}
	return nil, fmt.Errorf("no repos found in response")
}

func repoFullName(r *githubTrendingRepo) string {
	if r.FullName != "" {
		return r.FullName
	}
	if r.FullNameSnk != "" {
		return r.FullNameSnk
	}
	if r.Author != "" && r.Name != "" {
		return r.Author + "/" + r.Name
	}
	return ""
}

func splitOwner(full string) string {
	if i := strings.IndexByte(full, '/'); i > 0 {
		return full[:i]
	}
	return ""
}

func firstNonZero(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

func init() {
	Register("github_trending", Factory(newGitHubTrendingSource))
}
