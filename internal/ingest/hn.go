package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"briefing-v3/internal/store"
)

// hnConfig is the JSON shape stored in Source.ConfigJSON for type "hn_top".
//
// URL defaults to the HackerNews firebase topstories endpoint. TopN bounds
// how many IDs are hydrated to full items (each is one extra HTTP call).
//
// v1.0.1 Phase 4.4: MinPoints 过滤低分 HN 帖子. Front-page 一般 >=100, 偶
// 有爆款 200+ / 500+; 设 100 能有效剔除刚进 top30 但社区投票不足的试探帖.
// 0 = 不过滤 (默认, 保留 v1.0.0 行为).
type hnConfig struct {
	URL       string `json:"url"`
	TopN      int    `json:"top_n"`
	MinPoints int    `json:"min_points"`
}

// hnItem is a subset of the HackerNews firebase item schema. Only fields we
// actually use are decoded.
type hnItem struct {
	ID    int64  `json:"id"`
	Type  string `json:"type"`
	By    string `json:"by"`
	Time  int64  `json:"time"`
	Title string `json:"title"`
	URL   string `json:"url"`
	Text  string `json:"text"`
	Score int    `json:"score"`
}

const (
	hnItemURLTemplate = "https://hacker-news.firebaseio.com/v0/item/%d.json"
	hnFetchConcurrent = 10
)

// hnSource pulls HackerNews front page stories via the public firebase API.
type hnSource struct {
	row *store.Source
	cfg hnConfig
	hc  *http.Client
}

func newHNSource(row *store.Source) (Source, error) {
	var cfg hnConfig
	if strings.TrimSpace(row.ConfigJSON) != "" {
		if err := json.Unmarshal([]byte(row.ConfigJSON), &cfg); err != nil {
			return nil, fmt.Errorf("hn_top: parse ConfigJSON: %w", err)
		}
	}
	if cfg.URL == "" {
		cfg.URL = "https://hacker-news.firebaseio.com/v0/topstories.json"
	}
	if cfg.TopN <= 0 {
		cfg.TopN = 30
	}
	return &hnSource{
		row: row,
		cfg: cfg,
		hc:  &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func (s *hnSource) ID() int64    { return s.row.ID }
func (s *hnSource) Type() string { return s.row.Type }
func (s *hnSource) Name() string { return s.row.Name }

func (s *hnSource) Fetch(ctx context.Context) ([]*store.RawItem, error) {
	ids, err := s.fetchTopIDs(ctx)
	if err != nil {
		return nil, err
	}
	if len(ids) > s.cfg.TopN {
		ids = ids[:s.cfg.TopN]
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("hn_top: empty id list from %s", s.cfg.URL)
	}

	items := s.fetchItemsConcurrent(ctx, ids)

	now := time.Now().UTC()
	out := make([]*store.RawItem, 0, len(items))
	for _, it := range items {
		if it == nil {
			continue
		}
		// Only keep front-page "story" items.
		if it.Type != "" && it.Type != "story" {
			continue
		}
		// v1.0.1 Phase 4.4: 过滤低分 HN 帖子.
		if s.cfg.MinPoints > 0 && it.Score < s.cfg.MinPoints {
			continue
		}
		title := strings.TrimSpace(it.Title)
		if title == "" {
			continue
		}
		// Ask HN / Show HN items without an external URL are skipped to
		// avoid polluting the briefing with self-text threads.
		externalURL := strings.TrimSpace(it.URL)
		if externalURL == "" {
			continue
		}

		published := time.Unix(it.Time, 0).UTC()
		content := it.Text
		if content == "" {
			content = title
		}

		metaJSON, _ := json.Marshal(map[string]any{
			"score":       it.Score,
			"hn_item_url": "https://news.ycombinator.com/item?id=" + strconv.FormatInt(it.ID, 10),
		})

		out = append(out, &store.RawItem{
			DomainID:     s.row.DomainID,
			SourceID:     s.row.ID,
			ExternalID:   strconv.FormatInt(it.ID, 10),
			URL:          externalURL,
			Title:        title,
			Author:       it.By,
			PublishedAt:  published,
			FetchedAt:    now,
			Content:      content,
			MetadataJSON: string(metaJSON),
		})
	}
	return out, nil
}

// fetchTopIDs hits the topstories endpoint and returns its JSON array.
func (s *hnSource) fetchTopIDs(ctx context.Context) ([]int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("hn_top: new request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "briefing-v3/1.0 (+hn)")

	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hn_top: fetch %s: %w", s.cfg.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("hn_top: unexpected status %d from %s", resp.StatusCode, s.cfg.URL)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("hn_top: read top ids: %w", err)
	}
	var ids []int64
	if err := json.Unmarshal(body, &ids); err != nil {
		return nil, fmt.Errorf("hn_top: decode top ids: %w", err)
	}
	return ids, nil
}

// fetchItemsConcurrent hydrates the given item IDs in parallel with a bounded
// worker pool. Errors on individual items are dropped; we want partial
// results rather than one dead item killing the whole fetch.
func (s *hnSource) fetchItemsConcurrent(ctx context.Context, ids []int64) []*hnItem {
	out := make([]*hnItem, len(ids))
	sem := make(chan struct{}, hnFetchConcurrent)
	var wg sync.WaitGroup
	for i, id := range ids {
		i, id := i, id
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			item, err := s.fetchItem(ctx, id)
			if err == nil {
				out[i] = item
			}
		}()
	}
	wg.Wait()
	return out
}

func (s *hnSource) fetchItem(ctx context.Context, id int64) (*hnItem, error) {
	url := fmt.Sprintf(hnItemURLTemplate, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "briefing-v3/1.0 (+hn)")

	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("hn_top: item %d status %d", id, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var it hnItem
	if err := json.Unmarshal(body, &it); err != nil {
		return nil, err
	}
	return &it, nil
}

func init() {
	Register("hn_top", Factory(newHNSource))
}
