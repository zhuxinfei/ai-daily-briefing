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

// hfPapersConfig is the JSON shape stored in Source.ConfigJSON for type
// "huggingface_papers". It defaults to https://huggingface.co/papers.
type hfPapersConfig struct {
	URL string `json:"url"`
}

// hfPapersSource scrapes HuggingFace Daily Papers as HTML because there is
// no public JSON feed. The page structure is: each paper is an <article>
// with <h3><a href="/papers/{arxiv_id}">Title</a></h3> plus a small block
// of metadata ("Submitted by X", upvote count, "N authors", comments).
type hfPapersSource struct {
	row *store.Source
	cfg hfPapersConfig
	hc  *http.Client
}

var (
	// hfPaperIDPattern matches the arxiv-style paper slug (2604.08377).
	hfPaperIDPattern = regexp.MustCompile(`^/papers/([0-9]+\.[0-9]+)$`)
	// hfNumberPattern extracts a standalone integer token from a string.
	hfNumberPattern = regexp.MustCompile(`^[0-9]+$`)
)

func newHFPapersSource(row *store.Source) (Source, error) {
	var cfg hfPapersConfig
	if strings.TrimSpace(row.ConfigJSON) != "" {
		if err := json.Unmarshal([]byte(row.ConfigJSON), &cfg); err != nil {
			return nil, fmt.Errorf("huggingface_papers: parse ConfigJSON: %w", err)
		}
	}
	if cfg.URL == "" {
		cfg.URL = "https://huggingface.co/papers"
	}
	return &hfPapersSource{
		row: row,
		cfg: cfg,
		hc:  &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func (s *hfPapersSource) ID() int64    { return s.row.ID }
func (s *hfPapersSource) Type() string { return s.row.Type }
func (s *hfPapersSource) Name() string { return s.row.Name }

func (s *hfPapersSource) Fetch(ctx context.Context) ([]*store.RawItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("huggingface_papers: new request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 briefing-v3/1.0")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("huggingface_papers: fetch %s: %w", s.cfg.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("huggingface_papers: unexpected status %d from %s", resp.StatusCode, s.cfg.URL)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("huggingface_papers: parse html: %w", err)
	}

	now := time.Now().UTC()
	seen := make(map[string]bool)
	items := make([]*store.RawItem, 0, 32)

	// Primary selector: each paper card is an <article> with one h3 > a.
	doc.Find("article").Each(func(_ int, art *goquery.Selection) {
		link := art.Find(`h3 a[href^="/papers/"]`).First()
		if link.Length() == 0 {
			return
		}
		href, _ := link.Attr("href")
		id := extractHFPaperID(href)
		if id == "" || seen[id] {
			return
		}
		title := strings.TrimSpace(link.Text())
		if title == "" {
			return
		}
		seen[id] = true

		submittedBy, upvotes, numComments := extractHFCardMeta(art)

		metaJSON, _ := json.Marshal(map[string]any{
			"arxiv_id":     id,
			"upvotes":      upvotes,
			"num_comments": numComments,
		})

		items = append(items, &store.RawItem{
			DomainID:     s.row.DomainID,
			SourceID:     s.row.ID,
			ExternalID:   id,
			URL:          "https://huggingface.co/papers/" + id,
			Title:        title,
			Author:       submittedBy,
			PublishedAt:  now,
			FetchedAt:    now,
			Content:      title, // abstract lives on the detail page; v1.1 can enrich
			MetadataJSON: string(metaJSON),
		})
	})

	// Fallback: if the article selector produced nothing (e.g. HF changed
	// the DOM), sweep every /papers/{id} link on the page.
	if len(items) == 0 {
		doc.Find(`a[href^="/papers/"]`).Each(func(_ int, a *goquery.Selection) {
			href, _ := a.Attr("href")
			id := extractHFPaperID(href)
			if id == "" || seen[id] {
				return
			}
			title := strings.TrimSpace(a.Text())
			if title == "" || len(title) < 10 {
				return
			}
			seen[id] = true
			items = append(items, &store.RawItem{
				DomainID:    s.row.DomainID,
				SourceID:    s.row.ID,
				ExternalID:  id,
				URL:         "https://huggingface.co/papers/" + id,
				Title:       title,
				PublishedAt: now,
				FetchedAt:   now,
				Content:     title,
			})
		})
	}

	if len(items) == 0 {
		return nil, fmt.Errorf("huggingface_papers: no paper cards found at %s", s.cfg.URL)
	}
	return items, nil
}

// extractHFPaperID returns the arxiv-style ID from an href like
// "/papers/2604.08377", or empty if the href is a listing page or
// anchor-tagged variant like "/papers/2604.08377#community".
func extractHFPaperID(href string) string {
	// Strip trailing fragment.
	if i := strings.IndexByte(href, '#'); i >= 0 {
		href = href[:i]
	}
	m := hfPaperIDPattern.FindStringSubmatch(href)
	if len(m) == 2 {
		return m[1]
	}
	return ""
}

// extractHFCardMeta does a best-effort scan over the article's text nodes
// to pull out submitter, upvote count and comment count. The HF template
// emits them in a stable order:
//
//	Submitted by
//	<username>
//	<upvote-count>
//	<paper title>     <- already captured via h3
//	·
//	<n> authors
//	<num_comments>
//
// If the layout drifts, missing values are silently left empty rather
// than breaking the whole ingest.
func extractHFCardMeta(art *goquery.Selection) (submittedBy string, upvotes int, numComments int) {
	texts := make([]string, 0, 16)
	art.Contents().Each(func(_ int, sel *goquery.Selection) {
		collectStrippedText(sel, &texts)
	})

	sawSubmitted := false
	for i, t := range texts {
		if strings.EqualFold(t, "Submitted by") {
			sawSubmitted = true
			if i+1 < len(texts) {
				submittedBy = texts[i+1]
			}
			if i+2 < len(texts) && hfNumberPattern.MatchString(texts[i+2]) {
				upvotes, _ = strconv.Atoi(texts[i+2])
			}
			break
		}
	}
	if !sawSubmitted {
		return
	}

	// Comment count is typically the last standalone integer in the card.
	for i := len(texts) - 1; i >= 0; i-- {
		if hfNumberPattern.MatchString(texts[i]) {
			v, _ := strconv.Atoi(texts[i])
			if v != upvotes { // avoid mistaking upvotes for comments
				numComments = v
				break
			}
		}
	}
	return
}

// collectStrippedText walks the selection tree and appends each non-empty
// stripped text node to out.
func collectStrippedText(sel *goquery.Selection, out *[]string) {
	sel.Contents().Each(func(_ int, child *goquery.Selection) {
		if goquery.NodeName(child) == "#text" {
			txt := strings.TrimSpace(child.Text())
			if txt != "" {
				*out = append(*out, txt)
			}
			return
		}
		collectStrippedText(child, out)
	})
}

func init() {
	Register("huggingface_papers", Factory(newHFPapersSource))
}
