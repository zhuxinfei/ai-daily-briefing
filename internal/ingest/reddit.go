package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"briefing-v3/internal/store"
)

// redditConfig is the JSON shape stored in Source.ConfigJSON for type
// "reddit_json". url is any subreddit listing endpoint, e.g.
// https://www.reddit.com/r/MachineLearning/hot.json. limit bounds how many
// of the returned children are converted to RawItem.
//
// v1.0.1 Phase 4.4: MinScore 过滤低质量 reddit 帖子. Reddit 的 "score" 是
// upvote-downvote 净值, r/MachineLearning / r/LocalLLaMA 的正经内容通常
// >=50, 偶尔新帖低分不代表必然水文 (默认 0 = 不过滤); 对信噪比高的订阅
// 源建议设置 50 起.
type redditConfig struct {
	URL      string `json:"url"`
	Limit    int    `json:"limit"`
	MinScore int    `json:"min_score"`
}

// redditListing partially models the /r/XXX/hot.json response.
type redditListing struct {
	Data struct {
		Children []redditChild `json:"children"`
	} `json:"data"`
}

type redditChild struct {
	Data redditChildData `json:"data"`
}

type redditChildData struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	URL         string  `json:"url"`
	Author      string  `json:"author"`
	CreatedUTC  float64 `json:"created_utc"`
	Permalink   string  `json:"permalink"`
	Selftext    string  `json:"selftext"`
	NumComments int     `json:"num_comments"`
	Score       int     `json:"score"`
	Subreddit   string  `json:"subreddit"`
	Stickied    bool    `json:"stickied"`
	Over18      bool    `json:"over_18"`
}

const (
	redditUserAgent         = "Mozilla/5.0 (compatible; briefing-v3/1.0)"
	redditSelftextSoftLimit = 5000
	redditSelftextKeepFirst = 2000
)

// redditSource is a very small JSON reader for Reddit listing endpoints.
// Reddit rate-limits aggressively by User-Agent, so the UA header is
// non-optional.
type redditSource struct {
	row *store.Source
	cfg redditConfig
	hc  *http.Client
}

func newRedditSource(row *store.Source) (Source, error) {
	var cfg redditConfig
	if strings.TrimSpace(row.ConfigJSON) == "" {
		return nil, fmt.Errorf("reddit_json: empty ConfigJSON for source %d", row.ID)
	}
	if err := json.Unmarshal([]byte(row.ConfigJSON), &cfg); err != nil {
		return nil, fmt.Errorf("reddit_json: parse ConfigJSON: %w", err)
	}
	if cfg.URL == "" {
		return nil, fmt.Errorf("reddit_json: ConfigJSON.url is required for source %d", row.ID)
	}
	if cfg.Limit <= 0 {
		cfg.Limit = 25
	}
	return &redditSource{
		row: row,
		cfg: cfg,
		hc:  &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func (s *redditSource) ID() int64    { return s.row.ID }
func (s *redditSource) Type() string { return s.row.Type }
func (s *redditSource) Name() string { return s.row.Name }

func (s *redditSource) Fetch(ctx context.Context) ([]*store.RawItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("reddit_json: new request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	// Reddit blocks requests lacking a custom UA with HTTP 429.
	req.Header.Set("User-Agent", redditUserAgent)

	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reddit_json: fetch %s: %w", s.cfg.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("reddit_json: unexpected status %d from %s", resp.StatusCode, s.cfg.URL)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reddit_json: read body: %w", err)
	}

	var listing redditListing
	if err := json.Unmarshal(body, &listing); err != nil {
		return nil, fmt.Errorf("reddit_json: decode %s: %w", s.cfg.URL, err)
	}

	now := time.Now().UTC()
	items := make([]*store.RawItem, 0, len(listing.Data.Children))
	for _, child := range listing.Data.Children {
		if len(items) >= s.cfg.Limit {
			break
		}
		d := child.Data
		id := strings.TrimSpace(d.ID)
		if id == "" {
			continue
		}
		// Skip pinned subreddit announcements and NSFW threads — neither
		// contribute meaningful AI news signal.
		if d.Stickied || d.Over18 {
			continue
		}
		// v1.0.1 Phase 4.4: 过滤低分帖, 提升 social section 信噪比.
		if s.cfg.MinScore > 0 && d.Score < s.cfg.MinScore {
			continue
		}
		title := strings.TrimSpace(d.Title)
		if title == "" {
			continue
		}

		// Discussion page URL (permalink) is preferred over linked-to URL
		// so downstream tooling always lands on the Reddit thread itself.
		permalink := d.Permalink
		discussionURL := d.URL
		if strings.HasPrefix(permalink, "/") {
			discussionURL = "https://www.reddit.com" + permalink
		}

		content := d.Selftext
		if content == "" {
			content = title
		}
		// Oversized selftexts are truncated to keep the store light.
		if len(content) > redditSelftextSoftLimit {
			content = content[:redditSelftextKeepFirst]
		}

		publishedAt := time.Unix(int64(d.CreatedUTC), 0).UTC()

		metaJSON, _ := json.Marshal(map[string]any{
			"subreddit":    d.Subreddit,
			"score":        d.Score,
			"num_comments": d.NumComments,
			"external_url": d.URL,
		})

		items = append(items, &store.RawItem{
			DomainID:     s.row.DomainID,
			SourceID:     s.row.ID,
			ExternalID:   id,
			URL:          discussionURL,
			Title:        title,
			Author:       d.Author,
			PublishedAt:  publishedAt,
			FetchedAt:    now,
			Content:      content,
			MetadataJSON: string(metaJSON),
		})
	}
	return items, nil
}

func init() {
	Register("reddit_json", Factory(newRedditSource))
}
