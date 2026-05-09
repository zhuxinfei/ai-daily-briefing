// Package illustration generates one relevance-matched image per
// IssueItem. v1.0.0 uses the free public Pollinations.ai HTTP endpoint
// (no API key, no auth, no quota cap) with a multi-layer fallback:
//
//	1. Pollinations default model → image/png
//	2. Pollinations turbo model    → image/png (faster, slightly worse)
//	3. Picsum random seeded image  → placeholder if both fail
//
// Each image is downloaded once, stored under data/images/items/<date>/
// item-<N>.jpg and referenced from HTML by a relative path
// "../data/images/items/YYYY-MM-DD/item-N.jpg".
//
// Prompt generation: we feed the IssueItem's title + first 160 runes of
// body markdown through the LLM once per pipeline (batched) to produce
// a short English prompt suitable for an editorial illustration.
// Batching keeps the LLM cost at ONE call per run, not per item.
//
// NOTE: This package is intentionally standalone and does not import
// any other briefing-v3 internal package except store. The run.go
// orchestrator is the only caller and is responsible for wiring.
package illustration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"briefing-v3/internal/store"
)

// Config bundles all knobs the Renderer needs. All paths are resolved
// against the current working directory at call time; pass absolute
// paths if the caller expects a specific location.
type Config struct {
	OutputDir    string        // e.g. "data/images/items"
	Timeout      time.Duration // per-request timeout, default 40s
	Concurrency  int           // max parallel downloads, default 4
	Width        int           // default 800
	Height       int           // default 450
	PreferTurbo  bool          // use ?model=turbo first (faster)
	PicsumSeed   string        // base seed for picsum fallback, default "briefing"
}

func (c *Config) fillDefaults() {
	if c.OutputDir == "" {
		c.OutputDir = "data/images/items"
	}
	if c.Timeout == 0 {
		c.Timeout = 40 * time.Second
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 4
	}
	if c.Width <= 0 {
		c.Width = 800
	}
	if c.Height <= 0 {
		c.Height = 450
	}
	if c.PicsumSeed == "" {
		c.PicsumSeed = "briefing"
	}
}

// Result describes one downloaded illustration.
type Result struct {
	IssueItemID int64  // maps back to the IssueItem by id (or by Seq+Section if id=0)
	Section     string // for stable routing when ID is 0
	Seq         int
	LocalPath   string // absolute path on disk
	RelPath     string // path relative to docs/ for HTML embedding
	SourceKind  string // "pollinations" | "pollinations_turbo" | "picsum"
	Alt         string // alt text, in Chinese (item.Title trimmed)
}

// Renderer owns a shared HTTP client and orchestrates per-item
// illustration generation.
type Renderer struct {
	cfg Config
	hc  *http.Client
}

// New returns a Renderer bound to cfg.
func New(cfg Config) *Renderer {
	cfg.fillDefaults()
	return &Renderer{
		cfg: cfg,
		hc: &http.Client{
			Timeout: cfg.Timeout,
		},
	}
}

// GenerateForItems downloads one illustration per IssueItem concurrently
// and returns a map from (section, seq) tuple to the Result. The map key
// format is "<section>#<seq>" so callers can look up results without
// caring whether the IssueItem already has a stable database id.
//
// Prompts should already be in English and illustration-friendly;
// use BuildPrompts to produce them from the IssueItems. Callers can
// also supply their own slice if they want custom wording.
//
// GenerateForItems never returns a hard error for a single item:
// it always falls back to picsum so every item gets SOMETHING. The
// returned error is non-nil only for catastrophic failures (e.g.
// cannot mkdir OutputDir).
func (r *Renderer) GenerateForItems(
	ctx context.Context,
	date time.Time,
	items []*store.IssueItem,
	prompts []string,
) (map[string]*Result, error) {
	if len(items) == 0 {
		return nil, nil
	}
	if len(prompts) != len(items) {
		return nil, fmt.Errorf("illustration: prompts len %d != items len %d",
			len(prompts), len(items))
	}

	dateDir := filepath.Join(r.cfg.OutputDir, date.Format("2006-01-02"))
	if err := os.MkdirAll(dateDir, 0o755); err != nil {
		return nil, fmt.Errorf("illustration: mkdir %s: %w", dateDir, err)
	}

	results := make(map[string]*Result, len(items))
	var mu sync.Mutex

	sem := make(chan struct{}, r.cfg.Concurrency)
	var wg sync.WaitGroup

	for i, it := range items {
		if it == nil {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, item *store.IssueItem) {
			defer wg.Done()
			defer func() { <-sem }()

			prompt := prompts[idx]
			if strings.TrimSpace(prompt) == "" {
				// Fallback to the item title if prompt is empty.
				prompt = strings.TrimSpace(item.Title)
			}
			if prompt == "" {
				prompt = "ai news editorial illustration"
			}

			filename := fmt.Sprintf("item-%d.jpg", idx+1)
			fullPath := filepath.Join(dateDir, filename)
			relPath := fmt.Sprintf("../data/images/items/%s/%s",
				date.Format("2006-01-02"), filename)

			res := &Result{
				IssueItemID: item.ID,
				Section:     item.Section,
				Seq:         item.Seq,
				LocalPath:   fullPath,
				RelPath:     relPath,
				Alt:         strings.TrimSpace(item.Title),
			}

			// Layer 1: pollinations default (or turbo if preferred)
			kind := "pollinations"
			ok := r.downloadPollinations(ctx, prompt, fullPath, r.cfg.PreferTurbo, idx)
			if !ok {
				// Layer 2: other pollinations variant
				kind = "pollinations_turbo"
				if r.cfg.PreferTurbo {
					kind = "pollinations"
				}
				ok = r.downloadPollinations(ctx, prompt, fullPath, !r.cfg.PreferTurbo, idx)
			}
			if !ok {
				// Layer 3: picsum placeholder (never fails absent total
				// net outage).
				kind = "picsum"
				ok = r.downloadPicsum(ctx, fmt.Sprintf("%s-%s-%d",
					r.cfg.PicsumSeed, date.Format("20060102"), idx+1), fullPath)
			}
			if !ok {
				return
			}

			res.SourceKind = kind
			mu.Lock()
			results[fmt.Sprintf("%s#%d", item.Section, item.Seq)] = res
			mu.Unlock()
		}(i, it)
	}
	wg.Wait()
	return results, nil
}

// downloadPollinations tries one pollinations variant. Returns true on
// success (file saved, size > 1KB so we skip tiny error responses).
func (r *Renderer) downloadPollinations(parent context.Context, prompt, dest string, turbo bool, seedOffset int) bool {
	ctx, cancel := context.WithTimeout(parent, r.cfg.Timeout)
	defer cancel()

	// Pollinations accepts the prompt as a path segment. URL-encode it.
	encoded := url.PathEscape(prompt)
	// Deterministic seed per item so repeated runs produce the same
	// image for the same prompt (cache-friendly).
	seed := 1000 + seedOffset
	qs := fmt.Sprintf("?width=%d&height=%d&nologo=true&seed=%d",
		r.cfg.Width, r.cfg.Height, seed)
	if turbo {
		qs += "&model=turbo"
	}
	apiURL := "https://image.pollinations.ai/prompt/" + encoded + qs

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 briefing-v3/1.0")
	req.Header.Set("Accept", "image/avif,image/webp,image/png,image/*")
	req.Header.Set("Referer", "https://pollinations.ai/")

	resp, err := r.hc.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	return saveResponse(resp.Body, dest, 1024)
}

// downloadPicsum is the last-resort fallback. picsum is stable and
// always returns a valid JPEG, so this is the hard floor for "every
// item has an image".
func (r *Renderer) downloadPicsum(parent context.Context, seed, dest string) bool {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()

	apiURL := fmt.Sprintf("https://picsum.photos/seed/%s/%d/%d",
		url.PathEscape(seed), 800, 450)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "briefing-v3/1.0")

	resp, err := r.hc.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	return saveResponse(resp.Body, dest, 1024)
}

// saveResponse writes body to dest, enforcing a minimum size guard so
// we do not accept a tiny 429/500 HTML error page disguised as an image.
func saveResponse(body io.Reader, dest string, minBytes int) bool {
	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return false
	}
	n, err := io.Copy(f, body)
	cerr := f.Close()
	if err != nil || cerr != nil || int(n) < minBytes {
		_ = os.Remove(tmp)
		return false
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return false
	}
	return true
}

// BuildHotlinkURL returns a Pollinations image URL that can be embedded
// directly into an <img src=...> or markdown ![](url) without any
// local download. The browser of the reader then triggers Pollinations
// to render the image on demand (Pollinations caches on the URL).
//
// Use this in environments where you do NOT want briefing-v3 to own
// any local image bytes (the user's hard preference: "no local hosting,
// everything should be on-line fetched or generated").
//
// The prompt should be in ENGLISH for best generation quality, but
// Pollinations also accepts Chinese/mixed-language prompts. The seed
// parameter makes the output deterministic for a given (prompt, seed)
// pair so a regen with the same data returns the same image.
//
// Style: the function appends an infographic / diagram / flowchart
// style suffix so the output tends towards "explanatory illustration"
// rather than generic stock art, matching the user request for
// "图解/功能拆解/流程拆解" (diagrammatic feature/flow breakdowns).
//
// width/height default to 1200x675 (16:9 landscape, matches the
// existing HTML card layout). Seed must be unique per item to avoid
// all items getting the same picture.
func BuildHotlinkURL(prompt string, seed int, width, height int) string {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return ""
	}
	if width <= 0 {
		width = 1200
	}
	if height <= 0 {
		height = 675
	}
	// Append style tokens that push Pollinations towards infographic
	// output rather than generic stock illustration.
	styled := prompt + ", technical infographic diagram, architecture flowchart, clean vector illustration, educational style, no text overlay, modern minimal, wide aspect"

	encoded := url.PathEscape(styled)
	return fmt.Sprintf(
		"https://image.pollinations.ai/prompt/%s?width=%d&height=%d&nologo=true&seed=%d",
		encoded, width, height, seed,
	)
}

// BuildPrompts is a helper that produces one English illustration
// prompt per IssueItem from its title + a short body excerpt. This is
// a purely mechanical fallback that does not require an LLM call; it
// simply strips markdown, extracts the first strong-emphasized phrase,
// and appends "editorial illustration, modern, clean".
//
// Callers that want higher-quality prompts should build them via LLM
// and pass the resulting slice directly to GenerateForItems. BuildPrompts
// is the zero-cost baseline.
func BuildPrompts(items []*store.IssueItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		if it == nil {
			out[i] = "ai news illustration"
			continue
		}
		title := strings.TrimSpace(it.Title)
		// Strip Markdown emphasis and numbering so pollinations gets
		// clean natural text.
		title = strings.ReplaceAll(title, "**", "")
		title = strings.TrimLeft(title, "0123456789. ")
		if title == "" {
			title = "ai news"
		}
		out[i] = title + ", editorial illustration, modern clean vector style, no text"
	}
	return out
}

// Validate sanity-checks Config and returns an error if Required fields
// are bad. Not currently used by the main pipeline but handy for tests.
func (c *Config) Validate() error {
	if c.Width < 200 || c.Height < 200 {
		return errors.New("illustration: width/height too small (min 200)")
	}
	return nil
}
