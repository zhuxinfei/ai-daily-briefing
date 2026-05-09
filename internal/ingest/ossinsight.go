package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"briefing-v3/internal/store"
)

// ossinsightConfig is the JSON shape stored in Source.ConfigJSON for type
// "ossinsight". It defaults to the public trends/repos endpoint.
//
// v1.0.1 Phase 4.5 (T3): Period 控制 trending 时间窗口, 直接使用 ossinsight
// 服务端的 ?period= 参数, 让"近期热门/涨星最快"语义由 API 端保证.
// 取值: past_24_hours | past_week | past_month. 默认 past_week.
// 用户原则: opensource section 看人气和影响力, 不限 repo 创建时间.
type ossinsightConfig struct {
	URL    string `json:"url"`
	Period string `json:"period"`
}

// ossinsightResponse models the shape returned by
// https://api.ossinsight.io/v1/trends/repos. The endpoint is a generic SQL
// proxy, so everything lives under data.rows[] and numeric fields come back
// as JSON strings.
type ossinsightResponse struct {
	Type string `json:"type"`
	Data struct {
		Rows []ossinsightRow `json:"rows"`
	} `json:"data"`
}

type ossinsightRow struct {
	RepoID            string `json:"repo_id"`
	RepoName          string `json:"repo_name"`
	PrimaryLanguage   string `json:"primary_language"`
	Description       string `json:"description"`
	Stars             string `json:"stars"`
	Forks             string `json:"forks"`
	PullRequests      string `json:"pull_requests"`
	Pushes            string `json:"pushes"`
	TotalScore        string `json:"total_score"`
	ContributorLogins string `json:"contributor_logins"`
	CollectionNames   string `json:"collection_names"`
}

// ossinsightSource pulls the current GitHub trending list via ossinsight.io.
// It replaces the flaky third-party topone scraper used by
// github_trending.go.
type ossinsightSource struct {
	row *store.Source
	cfg ossinsightConfig
	hc  *http.Client
}

func newOssInsightSource(row *store.Source) (Source, error) {
	var cfg ossinsightConfig
	if strings.TrimSpace(row.ConfigJSON) != "" {
		if err := json.Unmarshal([]byte(row.ConfigJSON), &cfg); err != nil {
			return nil, fmt.Errorf("ossinsight: parse ConfigJSON: %w", err)
		}
	}
	if cfg.URL == "" {
		cfg.URL = "https://api.ossinsight.io/v1/trends/repos"
	}
	if cfg.Period == "" {
		cfg.Period = "past_week"
	}
	return &ossinsightSource{
		row: row,
		cfg: cfg,
		hc:  &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func (s *ossinsightSource) ID() int64    { return s.row.ID }
func (s *ossinsightSource) Type() string { return s.row.Type }
func (s *ossinsightSource) Name() string { return s.row.Name }

func (s *ossinsightSource) Fetch(ctx context.Context) ([]*store.RawItem, error) {
	// v1.0.1 Phase 4.5 (T3): 用 ossinsight 服务端 period 参数控制时间窗口.
	url := s.cfg.URL
	if s.cfg.Period != "" {
		sep := "?"
		if strings.Contains(url, "?") {
			sep = "&"
		}
		url += sep + "period=" + s.cfg.Period
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("ossinsight: new request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "briefing-v3/1.0 (+ossinsight)")

	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ossinsight: fetch %s: %w", s.cfg.URL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ossinsight: unexpected status %d from %s", resp.StatusCode, s.cfg.URL)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ossinsight: read body: %w", err)
	}

	var parsed ossinsightResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("ossinsight: decode %s: %w", s.cfg.URL, err)
	}
	if len(parsed.Data.Rows) == 0 {
		return nil, fmt.Errorf("ossinsight: no rows in response from %s", s.cfg.URL)
	}

	now := time.Now().UTC()
	// v1.0.1 Phase 4.6: 加载 GitHub star 缓存, 给每个 repo 追加 stars_total
	// (总 star 数, 区别于 ossinsight 的 period 内增量). 缓存 24h TTL, 避免
	// 对同一 repo 反复查 GitHub API (anonymous 60/h).
	cache := loadGitHubStarsCache()
	items := make([]*store.RawItem, 0, len(parsed.Data.Rows))
	anonBudget := 50 // anonymous API 60/h, 留 10 给其他 (保守)
	for i := range parsed.Data.Rows {
		row := parsed.Data.Rows[i]
		full := strings.TrimSpace(row.RepoName)
		if full == "" {
			continue
		}
		externalID := strings.TrimSpace(row.RepoID)
		if externalID == "" {
			externalID = full
		}
		stars, _ := strconv.Atoi(strings.TrimSpace(row.Stars))
		forks, _ := strconv.Atoi(strings.TrimSpace(row.Forks))
		pushes, _ := strconv.Atoi(strings.TrimSpace(row.Pushes))
		totalScore, _ := strconv.ParseFloat(strings.TrimSpace(row.TotalScore), 64)

		// v1.0.1 Phase 4.6: 查/补 total stars. 优先走 cache; miss 时调 GitHub
		// API (预算内). 拿不到就 0, 不影响主流程.
		starsTotal := 0
		if cached, ok := cache[full]; ok && time.Since(cached.At) < 24*time.Hour {
			starsTotal = cached.Stars
		} else if anonBudget > 0 {
			if n, err := fetchGitHubStars(ctx, s.hc, full); err == nil {
				starsTotal = n
				cache[full] = githubRepoCacheEntry{Stars: n, At: now}
				anonBudget--
			}
		}

		metaJSON, _ := json.Marshal(map[string]any{
			"language":    row.PrimaryLanguage,
			"stars":       stars, // period 内增量 (stars+)
			"stars_total": starsTotal,
			"forks":       forks,
			"pushes":      pushes,
			"total_score": totalScore,
			"rank":        i + 1,
		})

		items = append(items, &store.RawItem{
			DomainID:     s.row.DomainID,
			SourceID:     s.row.ID,
			ExternalID:   externalID,
			URL:          "https://github.com/" + full,
			Title:        full,
			Author:       splitOwner(full),
			PublishedAt:  now, // ossinsight trend ranking is a daily snapshot
			FetchedAt:    now,
			Content:      row.Description,
			MetadataJSON: string(metaJSON),
		})
	}
	saveGitHubStarsCache(cache)
	return items, nil
}

// ----- GitHub stars cache (v1.0.1 Phase 4.6) -----

type githubRepoCacheEntry struct {
	Stars int       `json:"stars"`
	At    time.Time `json:"at"`
}

const githubStarsCachePath = "data/github_stars_cache.json"

func loadGitHubStarsCache() map[string]githubRepoCacheEntry {
	data, err := os.ReadFile(githubStarsCachePath)
	if err != nil {
		return make(map[string]githubRepoCacheEntry)
	}
	var m map[string]githubRepoCacheEntry
	if err := json.Unmarshal(data, &m); err != nil {
		return make(map[string]githubRepoCacheEntry)
	}
	return m
}

func saveGitHubStarsCache(m map[string]githubRepoCacheEntry) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(githubStarsCachePath, data, 0o644)
}

func fetchGitHubStars(ctx context.Context, hc *http.Client, slug string) (int, error) {
	url := "https://api.github.com/repos/" + slug
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "briefing-v3/1.0")
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("github api status %d", resp.StatusCode)
	}
	var data struct {
		StargazersCount int `json:"stargazers_count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return 0, err
	}
	return data.StargazersCount, nil
}

func init() {
	Register("ossinsight", Factory(newOssInsightSource))
}
