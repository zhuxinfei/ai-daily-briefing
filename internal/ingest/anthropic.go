package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"briefing-v3/internal/store"
)

// anthropicConfig is the JSON shape stored in Source.ConfigJSON for type
// "anthropic_news". It defaults to https://www.anthropic.com/news because
// Anthropic does not publish a public RSS feed for the news index.
type anthropicConfig struct {
	URL string `json:"url"`
}

// anthropicSource scrapes anthropic.com/news for recent announcement links.
// The page is a client-rendered Next.js app, but the server-side HTML
// includes anchor tags with href prefixes of "/news/{slug}" that we can
// enumerate and de-duplicate by slug.
type anthropicSource struct {
	row *store.Source
	cfg anthropicConfig
	hc  *http.Client
}

// anthropicNavSlugs are /news/* paths that appear on the site as navigation
// chrome rather than real posts. They must be filtered out.
var anthropicNavSlugs = map[string]bool{
	"":          true,
	"announcements": true,
	"product":      true,
	"research":     true,
	"policy":       true,
	"society":      true,
	"press":        true,
}

func newAnthropicSource(row *store.Source) (Source, error) {
	var cfg anthropicConfig
	if strings.TrimSpace(row.ConfigJSON) != "" {
		if err := json.Unmarshal([]byte(row.ConfigJSON), &cfg); err != nil {
			return nil, fmt.Errorf("anthropic_news: parse ConfigJSON: %w", err)
		}
	}
	if cfg.URL == "" {
		cfg.URL = "https://www.anthropic.com/news"
	}
	return &anthropicSource{
		row: row,
		cfg: cfg,
		hc:  &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func (s *anthropicSource) ID() int64    { return s.row.ID }
func (s *anthropicSource) Type() string { return s.row.Type }
func (s *anthropicSource) Name() string { return s.row.Name }

func (s *anthropicSource) Fetch(ctx context.Context) ([]*store.RawItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("anthropic_news: new request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 briefing-v3/1.0")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic_news: fetch %s: %w", s.cfg.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("anthropic_news: unexpected status %d from %s", resp.StatusCode, s.cfg.URL)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("anthropic_news: parse html: %w", err)
	}

	now := time.Now().UTC()
	seen := make(map[string]bool)
	items := make([]*store.RawItem, 0, 32)

	doc.Find(`a[href^="/news/"]`).Each(func(_ int, a *goquery.Selection) {
		href, _ := a.Attr("href")
		slug := extractAnthropicSlug(href)
		if slug == "" || anthropicNavSlugs[slug] {
			return
		}
		if seen[slug] {
			return
		}
		seen[slug] = true

		title := strings.TrimSpace(a.Text())
		if title == "" {
			// Fall back to nearest heading inside the link's ancestry.
			if h := nearestHeading(a); h != "" {
				title = h
			}
		}
		if title == "" {
			// Last resort: humanize slug.
			title = humanizeSlug(slug)
		}

		// v1.0.1 Phase 4.5 (T4): 解析卡片附近的 <time class="...date..."> 元素
		// 拿真实发布日期 (例如 "Apr 14, 2026"). 之前用 now 兜底, 让老 news
		// 当 24h 内. 解析失败 → IsZero 让 filter drop, 不抛错不阻塞 pipeline.
		published := extractAnthropicDate(a)

		items = append(items, &store.RawItem{
			DomainID:    s.row.DomainID,
			SourceID:    s.row.ID,
			ExternalID:  slug,
			URL:         "https://www.anthropic.com/news/" + slug,
			Title:       title,
			PublishedAt: published,
			FetchedAt:   now,
			Content:     "", // v1.1: follow link and scrape body/summary
		})
	})

	if len(items) == 0 {
		return nil, fmt.Errorf("anthropic_news: no /news/ links found at %s", s.cfg.URL)
	}
	return items, nil
}

// anthropicDateRe matches anthropic listing's date format like "Apr 14, 2026".
// They use 3-letter month + day + year on every news card.
var anthropicDateRe = regexp.MustCompile(`(?i)(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\s+(\d{1,2}),?\s+(\d{4})`)

var anthropicMonthMap = map[string]time.Month{
	"jan": time.January, "feb": time.February, "mar": time.March,
	"apr": time.April, "may": time.May, "jun": time.June,
	"jul": time.July, "aug": time.August, "sep": time.September,
	"oct": time.October, "nov": time.November, "dec": time.December,
}

// extractAnthropicDate finds the publication date for a news card by walking
// up the DOM from the <a> element looking for a <time> element with a class
// containing "date". Returns IsZero on failure (caller's filter will drop).
//
// Strategy: search up to 4 ancestor levels for a sibling/descendant <time>,
// extract the date text ("Apr 14, 2026"), parse via regex.
func extractAnthropicDate(a *goquery.Selection) time.Time {
	scope := a
	for i := 0; i < 4; i++ {
		var found string
		scope.Find("time").EachWithBreak(func(_ int, t *goquery.Selection) bool {
			cls, _ := t.Attr("class")
			if !strings.Contains(strings.ToLower(cls), "date") {
				return true
			}
			txt := strings.TrimSpace(t.Text())
			if txt != "" {
				found = txt
				return false
			}
			return true
		})
		if found != "" {
			if t, ok := parseAnthropicDateText(found); ok {
				return t
			}
		}
		parent := scope.Parent()
		if parent.Length() == 0 {
			break
		}
		scope = parent
	}
	return time.Time{}
}

// parseAnthropicDateText parses "Apr 14, 2026" → time.Time at 00:00 UTC.
func parseAnthropicDateText(s string) (time.Time, bool) {
	m := anthropicDateRe.FindStringSubmatch(s)
	if len(m) != 4 {
		return time.Time{}, false
	}
	mon, ok := anthropicMonthMap[strings.ToLower(m[1])[:3]]
	if !ok {
		return time.Time{}, false
	}
	day, _ := strconv.Atoi(m[2])
	year, _ := strconv.Atoi(m[3])
	if year < 2000 || year > 2100 || day < 1 || day > 31 {
		return time.Time{}, false
	}
	return time.Date(year, mon, day, 0, 0, 0, 0, time.UTC), true
}

// extractAnthropicSlug returns the trailing segment of /news/{slug}, or
// empty if the href is just "/news" or "/news/" (the index itself).
func extractAnthropicSlug(href string) string {
	// Strip query string and fragment.
	if i := strings.IndexAny(href, "?#"); i >= 0 {
		href = href[:i]
	}
	href = strings.TrimSuffix(href, "/")
	const prefix = "/news/"
	if !strings.HasPrefix(href, prefix) {
		return ""
	}
	slug := href[len(prefix):]
	// Reject nested paths like /news/category/product.
	if strings.ContainsRune(slug, '/') {
		return ""
	}
	return slug
}

// nearestHeading walks up the DOM looking for an h1/h2/h3 sibling or
// descendant whose text can serve as a fallback title.
func nearestHeading(a *goquery.Selection) string {
	// First check inside the anchor itself.
	if h := a.Find("h1,h2,h3,h4").First(); h.Length() > 0 {
		if t := strings.TrimSpace(h.Text()); t != "" {
			return t
		}
	}
	// Then walk up to 3 ancestors.
	cur := a
	for i := 0; i < 3; i++ {
		cur = cur.Parent()
		if cur.Length() == 0 {
			return ""
		}
		if h := cur.Find("h1,h2,h3,h4").First(); h.Length() > 0 {
			if t := strings.TrimSpace(h.Text()); t != "" {
				return t
			}
		}
	}
	return ""
}

// humanizeSlug turns "claude-sonnet-4-6" into "Claude Sonnet 4 6".
func humanizeSlug(slug string) string {
	parts := strings.Split(slug, "-")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

func init() {
	Register("anthropic_news", Factory(newAnthropicSource))
}
