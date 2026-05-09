// Package mediaextract pulls the primary hero image (and optional
// video) from a source article URL by inspecting Open Graph / Twitter
// Card meta tags and falling back to the first in-body <img> tag.
//
// The output is a hotlink URL — briefing-v3 never downloads the bytes,
// just embeds the URL in the generated HTML. That is deliberate:
//
//   - zero storage cost on our side
//   - zero bandwidth cost on our side (CDN serves it)
//   - content stays 100% faithful to the source article
//   - no copyright exposure from hosting someone else's image bytes
//
// Used by the run pipeline: for every IssueItem we call ExtractBatch
// against the source URLs persisted in IssueItem.SourceURLsJSON. The
// first successful extraction wins; if every URL fails (no meta, no
// <img>, network timeout) the caller falls back to the illustration
// package to synthesise an AI image.
package mediaextract

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Media is the single result of parsing one source URL. Either field
// may be empty; callers should prefer ImageURL but accept VideoURL
// as a bonus if present (the HTML template embeds it via <video>).
type Media struct {
	SourceURL string // the original article URL we fetched
	ImageURL  string // og:image / twitter:image / first <img>
	VideoURL  string // og:video / twitter:player:stream / first <video>
	AltText   string // og:title or <title>, used as alt/caption
	Fetched   bool   // true if the HTTP GET succeeded, false if skipped
}

// HasImage reports whether m carries a usable image URL.
func (m *Media) HasImage() bool {
	return m != nil && strings.TrimSpace(m.ImageURL) != ""
}

// HasVideo reports whether m carries a usable video URL.
func (m *Media) HasVideo() bool {
	return m != nil && strings.TrimSpace(m.VideoURL) != ""
}

// Extractor owns a shared HTTP client + header defaults. Use New and
// reuse the instance across a whole pipeline run to benefit from
// connection pooling.
type Extractor struct {
	client      *http.Client
	timeout     time.Duration
	userAgent   string
	maxBodySize int64
}

// New constructs an Extractor with sane defaults: 10s per request,
// 2 MB max body to parse, browser-like user agent.
func New() *Extractor {
	return &Extractor{
		client: &http.Client{
			Timeout: 12 * time.Second,
		},
		timeout:     12 * time.Second,
		userAgent:   "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) briefing-v3/1.0",
		maxBodySize: 2 * 1024 * 1024,
	}
}

// Extract fetches a single URL and parses its HTML for Open Graph /
// Twitter Card meta tags. Returns a Media (possibly empty) and an
// error only for catastrophic failures (the happy path for "no image
// found" is a Media with zero fields + nil error).
func (e *Extractor) Extract(parent context.Context, articleURL string) (*Media, error) {
	articleURL = strings.TrimSpace(articleURL)
	if articleURL == "" {
		return &Media{}, errors.New("mediaextract: empty url")
	}

	// Skip protocols we don't understand so we don't waste a connection.
	if !(strings.HasPrefix(articleURL, "http://") || strings.HasPrefix(articleURL, "https://")) {
		return &Media{SourceURL: articleURL}, nil
	}

	ctx, cancel := context.WithTimeout(parent, e.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, articleURL, nil)
	if err != nil {
		return &Media{SourceURL: articleURL}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", e.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

	resp, err := e.client.Do(req)
	if err != nil {
		return &Media{SourceURL: articleURL}, fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return &Media{SourceURL: articleURL}, fmt.Errorf("http %d", resp.StatusCode)
	}

	// Only read up to maxBodySize so a huge page doesn't stall us.
	body, err := io.ReadAll(io.LimitReader(resp.Body, e.maxBodySize))
	if err != nil {
		return &Media{SourceURL: articleURL}, fmt.Errorf("read body: %w", err)
	}

	m := parseHTML(articleURL, string(body))
	m.Fetched = true
	return m, nil
}

// ExtractBatch runs Extract across urls concurrently, bounded by the
// concurrency argument (8 is a reasonable default). Results are
// returned in the same order as the input; failed fetches are still
// present as an empty Media with SourceURL set.
func (e *Extractor) ExtractBatch(parent context.Context, urls []string, concurrency int) []*Media {
	if concurrency <= 0 {
		concurrency = 8
	}
	if len(urls) == 0 {
		return nil
	}
	out := make([]*Media, len(urls))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, u := range urls {
		u := strings.TrimSpace(u)
		if u == "" {
			out[i] = &Media{}
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, target string) {
			defer wg.Done()
			defer func() { <-sem }()
			m, _ := e.Extract(parent, target)
			if m == nil {
				m = &Media{SourceURL: target}
			}
			out[idx] = m
		}(i, u)
	}
	wg.Wait()
	return out
}

// PickFirstImage walks a slice of source URLs and returns the first
// Media that actually has an ImageURL, or nil if none do. Callers use
// this to decide whether to fall back to AI illustration.
func (e *Extractor) PickFirstImage(parent context.Context, urls []string) *Media {
	results := e.ExtractBatch(parent, urls, 6)
	for _, m := range results {
		if m.HasImage() {
			return m
		}
	}
	return nil
}

// ---------------- HTML parsing (regex-only, no third-party deps) ----------------

// We avoid pulling in golang.org/x/net/html because we only care about
// five specific meta tags. A purpose-built set of regexes keeps the
// package standalone and trivially auditable.

var (
	// <meta property="og:image" content="..." /> variants (attribute order
	// flexible, quotes may be single/double).
	metaOGImageRe = buildMetaRe([]string{"og:image", "og:image:secure_url", "og:image:url"})
	metaOGVideoRe = buildMetaRe([]string{"og:video", "og:video:secure_url", "og:video:url"})
	metaTwImageRe = buildMetaRe([]string{"twitter:image", "twitter:image:src"})
	metaTwVideoRe = buildMetaRe([]string{"twitter:player:stream", "twitter:player"})
	metaOGTitleRe = buildMetaRe([]string{"og:title"})

	// Fallback: first <img src="..."> that is not tiny / not a tracker /
	// not inline base64. We allow .avif .webp .jpg .jpeg .png .gif.
	imgTagRe = regexp.MustCompile(`(?i)<img\b[^>]*?\bsrc\s*=\s*["']([^"']+\.(?:avif|webp|jpe?g|png|gif))(?:\?[^"']*)?["'][^>]*>`)

	// Fallback: first <video src="..."> tag.
	videoTagRe = regexp.MustCompile(`(?i)<video\b[^>]*?\bsrc\s*=\s*["']([^"']+\.(?:mp4|webm|mov|m3u8))(?:\?[^"']*)?["'][^>]*>`)

	// <title>…</title>
	titleTagRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
)

// buildMetaRe compiles a case-insensitive regex that matches an HTML
// meta tag whose property OR name attribute equals any of keys and
// extracts the content attribute. Handles attribute-order variants:
//
//	<meta property="og:image" content="...">
//	<meta content="..." property="og:image">
//	<meta name="twitter:image" content="...">
func buildMetaRe(keys []string) *regexp.Regexp {
	escaped := make([]string, len(keys))
	for i, k := range keys {
		escaped[i] = regexp.QuoteMeta(k)
	}
	key := strings.Join(escaped, "|")
	pattern := fmt.Sprintf(
		`(?is)<meta\b[^>]*?(?:(?:property|name)\s*=\s*["'](?:%s)["'][^>]*?\bcontent\s*=\s*["']([^"']+)["']|\bcontent\s*=\s*["']([^"']+)["'][^>]*?(?:property|name)\s*=\s*["'](?:%s)["'])[^>]*>`,
		key, key,
	)
	return regexp.MustCompile(pattern)
}

// parseHTML extracts the hero image / video / title from body using
// the regex set above. It never fails: if nothing matches, fields stay
// empty and the Media is returned anyway.
//
// Note: og:image and twitter:image are ALSO passed through
// looksLikeTracker. Many news sites set og:image to a global site
// banner or logo, so trusting og:image blindly gives us the same
// logo repeated across every item in the issue.
func parseHTML(baseURL, body string) *Media {
	m := &Media{SourceURL: baseURL}

	// Limit the search region to <head> if possible — meta tags live
	// there and searching 2 MB of body for each regex is wasteful.
	head := body
	if idx := strings.Index(strings.ToLower(body), "</head>"); idx > 0 {
		head = body[:idx]
	}

	// --- Open Graph image (filtered) ---
	if m.ImageURL == "" {
		if match := firstMatch(metaOGImageRe, head); match != "" {
			candidate := resolveURL(baseURL, match)
			if candidate != "" && !looksLikeTracker(candidate) {
				m.ImageURL = candidate
			}
		}
	}
	// --- Twitter card image (filtered) ---
	if m.ImageURL == "" {
		if match := firstMatch(metaTwImageRe, head); match != "" {
			candidate := resolveURL(baseURL, match)
			if candidate != "" && !looksLikeTracker(candidate) {
				m.ImageURL = candidate
			}
		}
	}
	// --- Open Graph video ---
	if m.VideoURL == "" {
		if match := firstMatch(metaOGVideoRe, head); match != "" {
			m.VideoURL = resolveURL(baseURL, match)
		}
	}
	// --- Twitter card video ---
	if m.VideoURL == "" {
		if match := firstMatch(metaTwVideoRe, head); match != "" {
			m.VideoURL = resolveURL(baseURL, match)
		}
	}

	// --- Alt / title ---
	if match := firstMatch(metaOGTitleRe, head); match != "" {
		m.AltText = decodeEntities(match)
	} else if t := titleTagRe.FindStringSubmatch(head); len(t) > 1 {
		m.AltText = strings.TrimSpace(decodeEntities(stripTags(t[1])))
	}

	// --- Fallback: scan all <img> tags and return the first that
	// clears the tracker/logo filter. Many news sites put the logo
	// in the first <img> position (masthead) so we must keep
	// looking past the first match.
	if m.ImageURL == "" {
		for _, match := range imgTagRe.FindAllStringSubmatch(body, 20) {
			if len(match) < 2 {
				continue
			}
			candidate := resolveURL(baseURL, match[1])
			if candidate == "" {
				continue
			}
			if looksLikeTracker(candidate) {
				continue
			}
			m.ImageURL = candidate
			break
		}
	}
	// --- Fallback: first <video> tag ---
	if m.VideoURL == "" {
		if tagMatch := videoTagRe.FindStringSubmatch(body); len(tagMatch) > 1 {
			m.VideoURL = resolveURL(baseURL, tagMatch[1])
		}
	}

	return m
}

// firstMatch returns the first captured group across all submatches of
// the compound regex built by buildMetaRe, which has two alternation
// branches each with its own capture. The loop below walks every
// submatch and returns the first non-empty one.
func firstMatch(re *regexp.Regexp, text string) string {
	all := re.FindAllStringSubmatch(text, -1)
	for _, m := range all {
		for i := 1; i < len(m); i++ {
			if strings.TrimSpace(m[i]) != "" {
				return strings.TrimSpace(m[i])
			}
		}
	}
	return ""
}

// resolveURL converts a possibly-relative href into an absolute URL
// using base. Returns href unchanged if base cannot be parsed.
func resolveURL(base, href string) string {
	href = strings.TrimSpace(href)
	if href == "" {
		return ""
	}
	if strings.HasPrefix(href, "data:") || strings.HasPrefix(href, "blob:") {
		return ""
	}
	bu, err := url.Parse(base)
	if err != nil {
		return href
	}
	ru, err := url.Parse(href)
	if err != nil {
		return href
	}
	return bu.ResolveReference(ru).String()
}

// looksLikeTracker is a best-effort heuristic to skip images that are
// clearly NOT content-bearing illustrations: logos, banners, tracking
// pixels, spacers, avatars, thumbnails, favicons, sprites, UI chrome,
// license badges, and site-wide composite brand images.
//
// The list is aggressive because the cost of a false positive (missing
// a real image) is much lower than the cost of a false negative
// (plastering 20 article cards with the same site logo).
//
// Maintenance note: when a real-world false negative slips through,
// add its exact URL fragment here BEFORE touching the structure. This
// list is the single source of truth — looksLikeTracker is called from
// parseHTML AND from enrichItemsWithMedia, so one addition fixes every
// caller at once.
func looksLikeTracker(raw string) bool {
	lower := strings.ToLower(raw)

	// Raw filename / path markers that almost always indicate UI chrome
	// instead of an article hero illustration.
	needles := []string{
		// tracking
		"pixel.gif", "pixel.png", "track", "beacon", "analytics",
		"blank.gif", "spacer.gif", "1x1", "transparent.gif",
		// logos & brand (incl. plural form that earlier version missed,
		// e.g. the-decoder's "openai_logos_wall_money.png")
		"/logo", "logo.", "logo-", "logo_", "brand/",
		"logos.", "logos-", "logos_", "_logos", "-logos", "/logos",
		"wall_money", "logos_wall", "brand_wall",
		"favicon", "apple-touch", "site-icon",
		// license & copyright badges (arxiv icons/licenses/by-nc-sa-4.0.png
		// was repeated 8× in the 2026-04-10 run)
		"/icons/licenses", "licenses/by-", "/cc-by", "creativecommons.",
		"by-nc-sa", "by-nc-nd", "by-sa-", "by-nd-",
		// arxiv static chrome
		"arxiv.org/icons", "arxiv.org/favicons", "arxiv.org/static",
		// generic site chrome icon folders
		"/icons/", "/badges/", "/emblems/",
		// avatars & profiles
		"avatar", "profile-pic", "gravatar", "user-photo", "user_photo",
		// navigation & layout chrome
		"header-", "header.", "footer-", "footer.", "navbar", "nav-",
		"sidebar-", "menu-",
		"/skin/", "/theme/", "/static/logo", "/assets/logo",
		"/images/logo", "/images/icon", "/common/",
		// sprite sheets & social share buttons
		"sprite", "social-", "share-button",
		// obvious thumbnails (low-res)
		"_thumb", "-thumb", "thumbnail", "_small", "-small",
		"_tiny", "-tiny", "icon-", "-icon.", "_icon.",
		// very small dimension hints (in path or query)
		"16x16", "32x32", "48x48", "64x64", "80x80", "96x96",
		// Google content CDN low-res thumbnail params (=s0-w300, =w100, etc.)
		"=s0-", "=w100", "=w150", "=w200", "=w300", "=h100", "=h150",
		// ads
		"ad.gif", "advert", "banner-ad", "promo-",
		// common placeholder/default patterns
		"placeholder", "default-image", "default_image", "no-image",
	}
	for _, needle := range needles {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// stripTags drops any HTML tags inside text (used to clean <title>
// content that might contain nested emphasis tags).
var tagStripRe = regexp.MustCompile(`<[^>]+>`)

func stripTags(s string) string {
	return tagStripRe.ReplaceAllString(s, "")
}

// decodeEntities replaces the handful of HTML entities we may see in
// og:title / <title>. A full decoder is overkill; the common ones are
// enough to make the alt text look sane in a tooltip.
var entities = map[string]string{
	"&amp;":  "&",
	"&lt;":   "<",
	"&gt;":   ">",
	"&quot;": "\"",
	"&apos;": "'",
	"&#39;":  "'",
	"&nbsp;": " ",
}

func decodeEntities(s string) string {
	for k, v := range entities {
		s = strings.ReplaceAll(s, k, v)
	}
	return s
}
