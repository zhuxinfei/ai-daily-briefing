package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"

	"briefing-v3/internal/store"
)

// rssConfig is the JSON shape stored in Source.ConfigJSON for type "rss".
// Any feed URL that gofeed can parse (RSS 1.0/2.0, Atom, JSON Feed) is
// acceptable. smol.ai publishes a standard RSS 2.0 feed at
// https://news.smol.ai/rss.xml.
//
// v1.0.1 Phase 1.1: MaxAgeHours caps items by pubDate age, guards against
// feeds returning full historical archive (OpenAI News 935 items / smol.ai
// 611 items both go back years; most other feeds self-limit). Zero value
// = no filter (backward compat).
type rssConfig struct {
	URL         string `json:"url"`
	MaxAgeHours int    `json:"max_age_hours,omitempty"`
}

// rssSource is a generic RSS/Atom/JSON feed adapter backed by gofeed.
type rssSource struct {
	row    *store.Source
	cfg    rssConfig
	hc     *http.Client
	parser *gofeed.Parser
}

func newRSSSource(row *store.Source) (Source, error) {
	var cfg rssConfig
	if strings.TrimSpace(row.ConfigJSON) == "" {
		return nil, fmt.Errorf("rss: empty ConfigJSON for source %d", row.ID)
	}
	if err := json.Unmarshal([]byte(row.ConfigJSON), &cfg); err != nil {
		return nil, fmt.Errorf("rss: parse ConfigJSON: %w", err)
	}
	if cfg.URL == "" {
		return nil, fmt.Errorf("rss: ConfigJSON.url is required for source %d", row.ID)
	}
	hc := &http.Client{Timeout: 10 * time.Second}
	parser := gofeed.NewParser()
	parser.Client = hc
	parser.UserAgent = "briefing-v3/0.1 (+rss)"
	return &rssSource{
		row:    row,
		cfg:    cfg,
		hc:     hc,
		parser: parser,
	}, nil
}

func (s *rssSource) ID() int64    { return s.row.ID }
func (s *rssSource) Type() string { return s.row.Type }
func (s *rssSource) Name() string { return s.row.Name }

func (s *rssSource) Fetch(ctx context.Context) ([]*store.RawItem, error) {
	feed, err := s.parser.ParseURLWithContext(s.cfg.URL, ctx)
	if err != nil {
		return nil, fmt.Errorf("rss: parse %s: %w", s.cfg.URL, err)
	}

	now := time.Now().UTC()
	// v1.0.1 Phase 1.1: 按 pubDate 年龄过滤, 挡掉全量历史 feed (OpenAI News /
	// smol.ai 返回多年前的老条目). MaxAgeHours=0 时不过滤 (保持默认行为,
	// 其他 feed 自限"近几天"就不需要设这个).
	items := feed.Items
	if s.cfg.MaxAgeHours > 0 {
		cutoff := now.Add(-time.Duration(s.cfg.MaxAgeHours) * time.Hour)
		items = filterItemsByAge(items, cutoff)
	}
	out := make([]*store.RawItem, 0, len(items))
	for _, fi := range items {
		if fi == nil {
			continue
		}
		externalID := strings.TrimSpace(fi.GUID)
		if externalID == "" {
			externalID = strings.TrimSpace(fi.Link)
		}
		if externalID == "" {
			continue
		}

		content := fi.Content
		if content == "" {
			content = fi.Description
		}

		published := time.Time{}
		if fi.PublishedParsed != nil {
			published = fi.PublishedParsed.UTC()
		} else if fi.UpdatedParsed != nil {
			published = fi.UpdatedParsed.UTC()
		}
		if published.IsZero() {
			published = now
		}

		author := ""
		if fi.Author != nil {
			author = fi.Author.Name
		}
		if author == "" && len(fi.Authors) > 0 && fi.Authors[0] != nil {
			author = fi.Authors[0].Name
		}

		metaJSON, _ := json.Marshal(map[string]any{
			"feed_title": feed.Title,
			"categories": fi.Categories,
		})

		out = append(out, &store.RawItem{
			DomainID:     s.row.DomainID,
			SourceID:     s.row.ID,
			ExternalID:   externalID,
			URL:          fi.Link,
			Title:        fi.Title,
			Author:       author,
			PublishedAt:  published,
			FetchedAt:    now,
			Content:      content,
			MetadataJSON: string(metaJSON),
		})
	}
	return out, nil
}

// filterItemsByAge returns the subset of items whose PublishedParsed (or
// UpdatedParsed as fallback) is within cutoff. Items with neither date
// are KEPT (we don't want to drop entries whose publisher omitted
// pubDate — downstream filter stage will re-check against the 24h window).
// v1.0.1 Phase 1.1 helper: kept as standalone function for table-driven
// unit testing without spinning up httptest.Server.
func filterItemsByAge(items []*gofeed.Item, cutoff time.Time) []*gofeed.Item {
	out := make([]*gofeed.Item, 0, len(items))
	for _, it := range items {
		if it == nil {
			continue
		}
		if it.PublishedParsed == nil && it.UpdatedParsed == nil {
			// No date known → keep. Conservative: rather let downstream
			// 24h filter drop it than drop here based on assumption.
			out = append(out, it)
			continue
		}
		var pub time.Time
		if it.PublishedParsed != nil {
			pub = *it.PublishedParsed
		} else if it.UpdatedParsed != nil {
			pub = *it.UpdatedParsed
		}
		if pub.Before(cutoff) {
			continue
		}
		out = append(out, it)
	}
	return out
}

func init() {
	Register("rss", Factory(newRSSSource))
}
