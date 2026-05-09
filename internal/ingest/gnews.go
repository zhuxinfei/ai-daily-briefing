package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"

	"briefing-v3/internal/store"
)

// gnewsConfig is the JSON shape stored in Source.ConfigJSON for type
// "google_news". All fields map 1:1 onto the RSS search query string used
// by news.google.com, so Chinese-language briefings can bypass the GFW by
// reading Google News's RSS endpoint directly.
//
// v1.0.1 Phase 1.2: Queries adds fallback query terms — Fetch tries each
// in order and returns the first non-empty result. Guards against Google
// News returning empty for a specific keyword on any given day
// (实测 "大语言模型" 今日返回 0). Backward compat: if Queries is empty,
// the adapter falls back to the single Query field (existing config).
type gnewsConfig struct {
	Query   string   `json:"query"`
	Queries []string `json:"queries,omitempty"`
	HL      string   `json:"hl"`
	GL      string   `json:"gl"`
	CEID    string   `json:"ceid"`
	When    string   `json:"when"`
}

// gnewsSource queries Google News RSS search. It wraps gofeed in the same
// way rss.go does but constructs the URL on every fetch from the config.
type gnewsSource struct {
	row    *store.Source
	cfg    gnewsConfig
	hc     *http.Client
	parser *gofeed.Parser
}

func newGoogleNewsSource(row *store.Source) (Source, error) {
	var cfg gnewsConfig
	if strings.TrimSpace(row.ConfigJSON) == "" {
		return nil, fmt.Errorf("google_news: empty ConfigJSON for source %d", row.ID)
	}
	if err := json.Unmarshal([]byte(row.ConfigJSON), &cfg); err != nil {
		return nil, fmt.Errorf("google_news: parse ConfigJSON: %w", err)
	}
	// v1.0.1 Phase 1.2: 归一化 Queries — 保留 Query 作为 backward compat
	// 单 query 形式, 如果 Queries 为空则用 [Query], 否则用 Queries.
	if len(cfg.Queries) == 0 && strings.TrimSpace(cfg.Query) != "" {
		cfg.Queries = []string{cfg.Query}
	}
	// 去掉空字符串 query (YAML 不小心留的空项)
	cleaned := cfg.Queries[:0]
	for _, q := range cfg.Queries {
		if strings.TrimSpace(q) != "" {
			cleaned = append(cleaned, q)
		}
	}
	cfg.Queries = cleaned
	if len(cfg.Queries) == 0 {
		return nil, fmt.Errorf("google_news: ConfigJSON.query or .queries is required for source %d", row.ID)
	}
	// cfg.Query 保留, 向后兼容 (下游如日志 / MetadataJSON 可能用)
	if cfg.Query == "" {
		cfg.Query = cfg.Queries[0]
	}
	if cfg.HL == "" {
		cfg.HL = "en-US"
	}
	if cfg.GL == "" {
		cfg.GL = "US"
	}
	if cfg.CEID == "" {
		cfg.CEID = "US:en"
	}
	if cfg.When == "" {
		cfg.When = "1d"
	}

	hc := &http.Client{Timeout: 15 * time.Second}
	parser := gofeed.NewParser()
	parser.Client = hc
	parser.UserAgent = "briefing-v3/1.0 (+google_news)"
	return &gnewsSource{
		row:    row,
		cfg:    cfg,
		hc:     hc,
		parser: parser,
	}, nil
}

func (s *gnewsSource) ID() int64    { return s.row.ID }
func (s *gnewsSource) Type() string { return s.row.Type }
func (s *gnewsSource) Name() string { return s.row.Name }

// buildQueryURL assembles the fully escaped Google News RSS search URL
// for the given query term. Chinese queries MUST be percent-encoded;
// passing them raw results in garbled/empty feeds.
func (s *gnewsSource) buildQueryURL(query string) string {
	q := url.QueryEscape(query)
	if s.cfg.When != "" {
		q += "+when:" + url.QueryEscape(s.cfg.When)
	}
	return fmt.Sprintf(
		"https://news.google.com/rss/search?q=%s&hl=%s&gl=%s&ceid=%s",
		q,
		url.QueryEscape(s.cfg.HL),
		url.QueryEscape(s.cfg.GL),
		url.QueryEscape(s.cfg.CEID),
	)
}

// v1.0.1 Phase 1.2: fetchOneQuery tries exactly one query term and returns
// parsed items (可能为空). 不抛 err 给空 feed (空是合法结果).
func (s *gnewsSource) fetchOneQuery(ctx context.Context, query string) ([]*gofeed.Item, string, error) {
	feedURL := s.buildQueryURL(query)
	feed, err := s.parser.ParseURLWithContext(feedURL, ctx)
	if err != nil {
		return nil, feedURL, fmt.Errorf("google_news: parse %s: %w", feedURL, err)
	}
	return feed.Items, feedURL, nil
}

func (s *gnewsSource) Fetch(ctx context.Context) ([]*store.RawItem, error) {
	// v1.0.1 Phase 1.2: 按 Queries 顺序逐个尝试, 第一个非空的 query 结果就用.
	// 空 feed (0 items 但无 transport err) 视为 "该 query 今天无热门",
	// 尝试 fallback. 所有 query 都空才返回 "empty feed".
	var rawItems []*gofeed.Item
	var usedQuery string
	var feedURL string
	var lastErr error
	for _, q := range s.cfg.Queries {
		items, builtURL, err := s.fetchOneQuery(ctx, q)
		if err != nil {
			lastErr = err
			continue
		}
		if len(items) > 0 {
			rawItems = items
			usedQuery = q
			feedURL = builtURL
			break
		}
		// 空 feed, 下一个 query
	}
	if len(rawItems) == 0 {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, fmt.Errorf("google_news: all fallback queries returned empty (tried %v)", s.cfg.Queries)
	}

	now := time.Now().UTC()
	items := make([]*store.RawItem, 0, len(rawItems))
	for _, fi := range rawItems {
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

		title := strings.TrimSpace(fi.Title)
		if title == "" {
			continue
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

		content := fi.Description
		if content == "" {
			content = fi.Content
		}

		metaJSON, _ := json.Marshal(map[string]any{
			"query":       s.cfg.Query,
			"used_query":  usedQuery, // v1.0.1: 实际命中的 fallback query
			"all_queries": s.cfg.Queries,
			"hl":          s.cfg.HL,
			"gl":          s.cfg.GL,
			"ceid":        s.cfg.CEID,
			"when":        s.cfg.When,
			"feed_url":    feedURL,
			"source_pub":  firstNonEmptyString(feedItemSourceName(fi), ""),
		})

		items = append(items, &store.RawItem{
			DomainID:     s.row.DomainID,
			SourceID:     s.row.ID,
			ExternalID:   externalID,
			URL:          fi.Link,
			Title:        title,
			Author:       author,
			PublishedAt:  published,
			FetchedAt:    now,
			Content:      content,
			MetadataJSON: string(metaJSON),
		})
	}

	if len(items) == 0 {
		return nil, fmt.Errorf("google_news: empty feed for query %q", s.cfg.Query)
	}
	return items, nil
}

// feedItemSourceName returns the publisher name Google News attaches via
// the <source> element, or empty if gofeed did not expose it.
func feedItemSourceName(fi *gofeed.Item) string {
	if fi == nil {
		return ""
	}
	// gofeed exposes the <source> element via Extensions when present.
	if src, ok := fi.Extensions["source"]; ok {
		for _, list := range src {
			for _, ext := range list {
				if v := strings.TrimSpace(ext.Value); v != "" {
					return v
				}
			}
		}
	}
	return ""
}

// firstNonEmptyString mirrors firstNonEmpty (defined in github_trending.go)
// but with an unambiguous name to avoid collisions as the package grows.
func firstNonEmptyString(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func init() {
	Register("google_news", Factory(newGoogleNewsSource))
}
