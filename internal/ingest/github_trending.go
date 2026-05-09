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

// githubTrendingConfig is the JSON shape stored in Source.ConfigJSON for
// type "github_trending".
type githubTrendingConfig struct {
	// URL is a topone-style trending endpoint, e.g.
	// https://git-trending.justlikemaki.vip/topone/?since=daily
	URL string `json:"url"`
}

// githubTrendingRepo is the per-repo shape returned by the trending proxy.
// The upstream API is a small scraper whose schema is not officially
// documented; field tags try both snake_case and camelCase so a schema drift
// on the remote side is less likely to break us silently.
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

// githubTrendingSource pulls daily trending repos from a topone-style proxy.
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
		hc:  &http.Client{Timeout: 10 * time.Second},
	}, nil
}

func (s *githubTrendingSource) ID() int64    { return s.row.ID }
func (s *githubTrendingSource) Type() string { return s.row.Type }
func (s *githubTrendingSource) Name() string { return s.row.Name }

func (s *githubTrendingSource) Fetch(ctx context.Context) ([]*store.RawItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("github_trending: new request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "briefing-v3/0.1 (+github_trending)")

	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github_trending: fetch %s: %w", s.cfg.URL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("github_trending: unexpected status %d from %s", resp.StatusCode, s.cfg.URL)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("github_trending: read body: %w", err)
	}

	repos, err := decodeTrendingRepos(body)
	if err != nil {
		return nil, fmt.Errorf("github_trending: decode %s: %w", s.cfg.URL, err)
	}

	now := time.Now().UTC()
	items := make([]*store.RawItem, 0, len(repos))
	for _, r := range repos {
		repo := r
		full := repoFullName(&repo)
		if full == "" {
			continue
		}
		url := repoURL(&repo, full)
		title := repo.Name
		if title == "" {
			title = full
		}
		stars := repo.CurrentStars
		if stars == 0 {
			if repo.Stars != 0 {
				stars = repo.Stars
			} else if repo.StargazerCnt != 0 {
				stars = repo.StargazerCnt
			}
		}
		metaJSON, _ := json.Marshal(map[string]any{
			"language":             repo.Language,
			"stars":                stars,
			"forks":                firstNonZero(repo.Forks, repo.ForksCount),
			"current_period_stars": repo.CurrentStars,
		})

		published := parseMaybeTime(repo.PushedAt, repo.UpdatedAt)
		if published.IsZero() {
			published = now
		}

		items = append(items, &store.RawItem{
			DomainID:     s.row.DomainID,
			SourceID:     s.row.ID,
			ExternalID:   full,
			URL:          url,
			Title:        title,
			Author:       firstNonEmpty(repo.Author, splitOwner(full)),
			PublishedAt:  published,
			FetchedAt:    now,
			Content:      repo.Description,
			MetadataJSON: string(metaJSON),
		})
	}
	return items, nil
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

func repoURL(r *githubTrendingRepo, full string) string {
	if r.URL != "" {
		return r.URL
	}
	if r.HTMLURL != "" {
		return r.HTMLURL
	}
	if full != "" {
		return "https://github.com/" + full
	}
	return ""
}

func splitOwner(full string) string {
	if i := strings.IndexByte(full, '/'); i > 0 {
		return full[:i]
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
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

// parseMaybeTime tries a few common timestamp formats and returns the first
// one that parses, or the zero value if none do.
func parseMaybeTime(values ...string) time.Time {
	layouts := []string{
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02T15:04:05Z",
		"2006-01-02 15:04:05",
	}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		for _, layout := range layouts {
			if t, err := time.Parse(layout, v); err == nil {
				return t.UTC()
			}
		}
	}
	return time.Time{}
}

func init() {
	Register("github_trending", Factory(newGitHubTrendingSource))
}
