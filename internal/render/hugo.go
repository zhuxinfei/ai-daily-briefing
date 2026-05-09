// Package render — Hextra (Hugo) post writer.
//
// This file bridges briefing-v3's canonical markdown output (markdown.go)
// and the Hextra content tree at $HEXTRA_SITE_DIR. v1.0.0 enforces:
//
//   1. Three-level sidebar tree under content/cn/{year}/{yearMonth}/{date}.md
//      so Hextra renders a foldable 年 → 月 → 日 archive (otherwise the
//      sidebar runs off-screen after a few weeks).
//   2. Mandatory hero "新闻大字报" image at the top of every issue page.
//      Reads {workDir}/data/images/cards/{date}/header.png if present,
//      copies it into the Hextra static tree, and prepends a markdown
//      image reference. Missing header is logged but does not block the
//      write — the daily run is still published with text-only fallback.
//   3. Image scrub + relocate. The body produced by RenderMarkdown may
//      contain inline ![alt](url) references injected upstream by
//      infocard (item-N.png) and mediaextract (og:image hotlinks). We:
//        - VERIFY every external http(s) URL via HEAD: status 200 +
//          Content-Length in [5 KB, 50 MB]. mediaextract has its own
//          blacklist + multi-candidate scan to filter logos/banners,
//          and the HEAD check is a "minimum viable" guard so we never
//          publish a 404 / timeout / favicon-sized icon. Verified URLs
//          are kept verbatim so real article images, GitHub README
//          screenshots, arXiv figures, etc. survive end to end;
//        - COPY local PNGs from briefing-v3/data/images/cards/... into
//          {siteDir}/static/images/cards/{date}/ and rewrite the
//          markdown reference to a Hugo-friendly absolute path
//          /images/cards/{date}/<basename>;
//        - DELETE any reference whose target file does not exist (no
//          broken image icons leak through to the published page).
//
// All Hugo concerns live here. run.go / mediaextract / infocard / publish
// stay untouched.
package render

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"briefing-v3/internal/store"
)

// WriteHugoPost renders a full daily briefing and writes it as a
// Hextra-compatible markdown file under siteDir. The target path is
// deterministic and uses a three-level structure:
//
//	{siteDir}/content/cn/{YYYY}/{YYYY-MM}/{YYYY-MM-DD}.md
//
// Year and month directories are created on demand together with their
// _index.md scaffolds so the Hextra sidebar shows a collapsible
// 2026 → 2026-04 → 2026-04-11 AI资讯 tree.
//
// The function relies on RenderMarkdown to produce the canonical body,
// strips the markdown's leading "## AI资讯日报 YYYY/M/D" heading (Hextra
// generates its own H1 from frontmatter.title), prepends the hero
// header.png reference if available, then scrubs/relocates every
// remaining ![alt](url) image reference. WriteHugoPost finally writes
// the file with a hand-built Hextra frontmatter block and returns the
// absolute path of the written file.
func WriteHugoPost(
	siteDir string,
	issue *store.Issue,
	items []*store.IssueItem,
	insight *store.IssueInsight,
	sections []SectionMeta,
) (string, error) {
	if siteDir == "" {
		return "", fmt.Errorf("hugo: siteDir is empty")
	}
	if issue == nil {
		return "", fmt.Errorf("hugo: issue is nil")
	}

	body := RenderMarkdown(issue, items, insight, sections)

	// Strip the leading `## AI资讯日报 YYYY/M/D\n\n` line that markdown.go
	// emits as its own title — Hextra renders frontmatter.title as H1
	// so keeping the duplicate line would cause a double header.
	headLine := fmt.Sprintf("## AI资讯日报 %d/%d/%d",
		issue.IssueDate.Year(), int(issue.IssueDate.Month()), issue.IssueDate.Day())
	if strings.HasPrefix(body, headLine) {
		body = strings.TrimPrefix(body, headLine)
		body = strings.TrimLeft(body, "\n")
	}

	// --- date helpers ---------------------------------------------------
	date := issue.IssueDate
	year := date.Format("2006")
	yearMonth := date.Format("2006-01")
	dateStr := date.Format("2006-01-02")

	// --- hero header.png prepend ---------------------------------------
	// briefing-v3's infocard pass writes data/images/cards/{date}/header.png
	// relative to its working directory. We probe a few candidate roots so
	// the function works whether briefing is invoked from /root/briefing-v3
	// or from a systemd unit with a different WorkingDirectory.
	heroAbs := findExistingPath([]string{
		filepath.Join("data", "images", "cards", dateStr, "header.png"),
		filepath.Join("/root/briefing-v3/data/images/cards", dateStr, "header.png"),
	})
	if heroAbs != "" {
		// Copy into the Hextra static tree so Hugo picks it up at build.
		targetDir := filepath.Join(siteDir, "static", "images", "cards", dateStr)
		if err := os.MkdirAll(targetDir, 0o755); err == nil {
			targetPath := filepath.Join(targetDir, "header.png")
			if err := copyFile(heroAbs, targetPath); err == nil {
				heroLine := fmt.Sprintf("![新闻大字报 · %s](/images/cards/%s/header.png)\n\n",
					dateStr, dateStr)
				body = heroLine + body
			}
		}
	}

	// --- scrub & relocate every remaining ![alt](url) ------------------
	body = scrubAndRelocateImages(body, siteDir, dateStr)

	// --- weekly report back-link ----------------------------------------
	// 永远挂"本周周报"链接 (无论本周周报是否已生成). 周报未生成时点击
	// 会被 ai-daily-site/layouts/partials/custom/footer.html 的全局 JS
	// 拦截, 弹模态框提示, 不跳走. 周报生成后同链接自动 hit 真实周报页.
	_, isoWeek := date.ISOWeek()
	weeklyLink := weeklyBackLink(date)
	body += fmt.Sprintf("\n\n---\n\n> 本周周报：[第%d周综合分析](%s)\n", isoWeek, weeklyLink)

	// --- frontmatter ----------------------------------------------------
	linkTitle := fmt.Sprintf("%02d-%02d AI资讯", int(date.Month()), date.Day())
	title := fmt.Sprintf("AI资讯日报 %d/%d/%d",
		date.Year(), int(date.Month()), date.Day())
	description := truncateDescription(issue.Summary, 150)

	var fm strings.Builder
	fm.WriteString("---\n")
	fmt.Fprintf(&fm, "linkTitle: %q\n", linkTitle)
	fmt.Fprintf(&fm, "title: %q\n", title)
	fmt.Fprintf(&fm, "weight: %d\n", date.Day())
	fm.WriteString("breadcrumbs: false\n")
	fm.WriteString("comments: false\n")
	fmt.Fprintf(&fm, "description: %q\n", description)
	fm.WriteString("---\n\n")

	full := fm.String() + body

	// --- filesystem: three-level tree -----------------------------------
	yearDir := filepath.Join(siteDir, "content", "cn", year)
	monthDir := filepath.Join(yearDir, yearMonth)
	if err := os.MkdirAll(monthDir, 0o755); err != nil {
		return "", fmt.Errorf("hugo: mkdir %s: %w", monthDir, err)
	}
	if err := ensureIndexFile(yearDir, year, date.Year()); err != nil {
		return "", fmt.Errorf("hugo: ensure year index: %w", err)
	}
	if err := ensureIndexFile(monthDir, yearMonth, int(date.Month())); err != nil {
		return "", fmt.Errorf("hugo: ensure month index: %w", err)
	}
	outPath := filepath.Join(monthDir, dateStr+".md")
	// v1.0.1 Batch 2.15: atomic write (.tmp → rename) 防读到半写入文件.
	if err := atomicWriteFile(outPath, []byte(full), 0o644); err != nil {
		return "", fmt.Errorf("hugo: write %s: %w", outPath, err)
	}

	// v1.0.1: update homepage with latest 6 cards + today link.
	if err := updateHomepage(siteDir, date); err != nil {
		fmt.Printf("[WARN] updateHomepage: %v (continuing)\n", err)
	}

	return outPath, nil
}

// ensureIndexFile creates a minimal Hextra _index.md scaffold under dir
// if and only if one does not already exist. The title becomes the
// directory's display name in the sidebar; weight controls intra-level
// sort order. We never overwrite an existing _index.md, so a hand-tuned
// scaffold survives subsequent runs.
func ensureIndexFile(dir, title string, weight int) error {
	indexPath := filepath.Join(dir, "_index.md")
	if _, err := os.Stat(indexPath); err == nil {
		return nil // already exists, leave it alone
	}
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: %q\n", title)
	fmt.Fprintf(&b, "linkTitle: %q\n", title)
	fmt.Fprintf(&b, "weight: %d\n", weight)
	b.WriteString("breadcrumbs: false\n")
	b.WriteString("comments: false\n")
	b.WriteString("---\n")
	// v1.0.1 Batch 2.15: atomic write.
	return atomicWriteFile(indexPath, []byte(b.String()), 0o644)
}

// atomicWriteFile writes data to path atomically via a .tmp sibling file
// + os.Rename. On Linux/POSIX, same-filesystem rename is atomic so readers
// never observe a half-written state. v1.0.1 Batch 2.15.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// imageRefRe matches markdown image references: ![alt](url)
// alt can contain anything except ']'; url can contain anything except ')'.
var imageRefRe = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)

// scrubAndRelocateImages walks the body and rewrites every ![alt](url)
// reference according to the v1.0.0 hard rules:
//
//   - http(s)://... external image  → DROP (avoid logo/banner noise)
//   - already /images/...           → KEEP (Hugo absolute path)
//   - local PNG that exists on disk → COPY into siteDir/static/images/cards/{date}/
//                                     and rewrite to /images/cards/{date}/<base>
//   - local PNG that does NOT exist → DROP (no broken image icons)
//
// All scrub decisions happen in a single pass so the body never carries
// a half-resolved reference into the published page.
func scrubAndRelocateImages(body, siteDir, dateStr string) string {
	targetDir := filepath.Join(siteDir, "static", "images", "cards", dateStr)
	mkdirOnce := false

	out := imageRefRe.ReplaceAllStringFunc(body, func(match string) string {
		m := imageRefRe.FindStringSubmatch(match)
		if len(m) < 3 {
			return ""
		}
		alt := strings.TrimSpace(m[1])
		url := strings.TrimSpace(m[2])
		if url == "" {
			return ""
		}

		// v1.0.0 修正: drop infocard L2 per-item PIL cards. The user
		// only wants ONE hero "大字报" at the top of the page (which
		// WriteHugoPost auto-prepends BEFORE this scrub runs, using a
		// /images/cards/{date}/header.png absolute path that will hit
		// the next case below). Every other item-*.png under cards/
		// is dropped — per-item cards cluttered the previous run.
		baseName := filepath.Base(url)
		if strings.HasPrefix(baseName, "item-") && strings.Contains(url, "cards/") {
			return ""
		}

		// Already a Hugo absolute path → keep as-is.
		if strings.HasPrefix(url, "/images/") {
			return match
		}

		// External http(s) URL: keep IF it passes a minimum-viable HEAD
		// probe (200 + Content-Length in [5 KB, 50 MB]). mediaextract
		// already filtered logos/banners via blacklist + multi-candidate
		// scan, so we trust the URL semantically and only verify it can
		// actually load. Anything that fails the HEAD (404, timeout,
		// favicon-sized icon, oversized binary) is dropped to avoid a
		// broken image icon leaking into the published page.
		if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
			if !verifyExternalImage(url) {
				return ""
			}
			return match
		}

		// Local file path. Try to resolve to an absolute path that
		// actually exists. infocard writes:
		//   ../data/images/cards/{date}/item-N.png   (relative)
		// or absolute paths under /root/briefing-v3/data/.
		candidates := []string{
			url,
			filepath.Join("/root/briefing-v3", url),
			filepath.Join("/root/briefing-v3", strings.TrimPrefix(url, "../")),
		}
		// Strip an arbitrary number of leading "../" prefixes.
		stripped := url
		for strings.HasPrefix(stripped, "../") {
			stripped = strings.TrimPrefix(stripped, "../")
			candidates = append(candidates, filepath.Join("/root/briefing-v3", stripped))
		}
		absPath := findExistingPath(candidates)
		if absPath == "" {
			// File missing → drop the reference.
			return ""
		}

		if !mkdirOnce {
			if err := os.MkdirAll(targetDir, 0o755); err != nil {
				return ""
			}
			mkdirOnce = true
		}

		base := filepath.Base(absPath)
		targetPath := filepath.Join(targetDir, base)
		if err := copyFile(absPath, targetPath); err != nil {
			return ""
		}

		newURL := fmt.Sprintf("/images/cards/%s/%s", dateStr, base)
		if alt == "" {
			return fmt.Sprintf("![](%s)", newURL)
		}
		return fmt.Sprintf("![%s](%s)", alt, newURL)
	})

	// Collapse three or more consecutive blank lines created by dropped
	// references so the markdown stays tidy.
	out = regexp.MustCompile(`\n{3,}`).ReplaceAllString(out, "\n\n")
	return out
}

// findExistingPath returns the first candidate path that exists on disk,
// or "" if none of them do. The check is a single os.Stat per entry.
func findExistingPath(candidates []string) string {
	for _, p := range candidates {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// copyFile streams src into dst with mode 0644. dst's parent directory
// must already exist; callers handle MkdirAll.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// bannedImageHosts is the explicit drop-list for image URL hosts that
// produce semantically empty or AI-hallucinated illustrations. We do not
// trust an upstream filter for these — any URL whose lower-cased form
// contains one of these substrings is unconditionally dropped before
// the HEAD probe even runs.
//
// Pollinations is the v1.0.0 motivating case: mediaextract falls back
// to image.pollinations.ai when the source page has no usable hero, and
// Pollinations responds with abstract AI art that has no real semantic
// relation to the news item (the user described it as "无关图片 +
// 大量重复"). It is also slow enough that the 5s HEAD probe usually
// times out, but that is incidental — we drop it explicitly so the
// behaviour does not depend on Pollinations latency.
var bannedImageHosts = []string{
	"image.pollinations.ai",
	"pollinations.ai",
}

// verifyExternalImage HEAD-probes an external image URL and returns
// true only when:
//   - the URL host is NOT on bannedImageHosts,
//   - the request completes within 5 s,
//   - the status code is 2xx,
//   - the response advertises a Content-Length in [5 KB, 50 MB] OR no
//     Content-Length at all (chunked encoding — we have no choice but
//     to trust the upstream filter in that case).
//
// This is the minimum-viable guard against banned hosts, 404, timeout,
// favicon-sized icons, and runaway binaries. The semantic "is this image
// actually related to the item" decision lives in mediaextract upstream,
// which already enforces a blacklist + multi-candidate scan;
// verifyExternalImage only catches the cases mediaextract cannot.
func verifyExternalImage(url string) bool {
	lowerURL := strings.ToLower(url)
	for _, banned := range bannedImageHosts {
		if strings.Contains(lowerURL, banned) {
			return false
		}
	}
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "briefing-v3/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		var size int64
		if _, err := fmt.Sscanf(cl, "%d", &size); err == nil {
			if size < 5*1024 || size > 50*1024*1024 {
				return false
			}
		}
	}
	return true
}

// truncateDescription keeps at most maxRunes runes of s, trimming
// whitespace at both ends and collapsing any embedded newlines to a
// single space so the result is safe to embed inside a double-quoted
// YAML scalar. Double quotes are escaped via the caller's %q.
// updateHomepage rewrites two dynamic regions in the site's root _index.md:
//
//  1. LATEST_6_CARDS — the most recent 6 daily briefing cards
//  2. TODAY_LINK     — the "查看今日早报" button href
//
// It scans content/cn/{YYYY}/{YYYY-MM}/*.md (excluding _index.md and
// anything under blog/) to discover published dailies, sorts them by
// filename date descending, and takes the first 6.
//
// Fail-soft: any error is returned but the caller logs and continues,
// so a homepage update failure never blocks the daily pipeline.
func updateHomepage(siteDir string, latestDate time.Time) error {
	indexPath := filepath.Join(siteDir, "content", "cn", "_index.md")
	original, err := os.ReadFile(indexPath)
	if err != nil {
		return fmt.Errorf("read homepage: %w", err)
	}

	// Read basePath from hugo.yaml so links work on GitHub Pages subpaths.
	basePath := ""
	if hugoYAML, err := os.ReadFile(filepath.Join(siteDir, "hugo.yaml")); err == nil {
		if m := regexp.MustCompile(`(?m)^baseURL:\s*https?://[^/]+(/.+?)/?$`).FindSubmatch(hugoYAML); len(m) > 1 {
			basePath = string(m[1]) // e.g. "/ai-daily-site"
		}
	}

	// --- discover daily .md files ---
	contentDir := filepath.Join(siteDir, "content", "cn")
	var dailyFiles []string

	// Walk year directories (2026, 2027, ...)
	yearEntries, err := os.ReadDir(contentDir)
	if err != nil {
		return fmt.Errorf("read content dir: %w", err)
	}
	for _, ye := range yearEntries {
		if !ye.IsDir() || ye.Name() == "blog" || ye.Name() == "tags" {
			continue
		}
		yearPath := filepath.Join(contentDir, ye.Name())
		monthEntries, err := os.ReadDir(yearPath)
		if err != nil {
			continue
		}
		for _, me := range monthEntries {
			if !me.IsDir() {
				continue
			}
			monthPath := filepath.Join(yearPath, me.Name())
			fileEntries, err := os.ReadDir(monthPath)
			if err != nil {
				continue
			}
			for _, fe := range fileEntries {
				name := fe.Name()
				if fe.IsDir() || name == "_index.md" || !strings.HasSuffix(name, ".md") {
					continue
				}
				// Validate it looks like a date-named file: YYYY-MM-DD.md
				base := strings.TrimSuffix(name, ".md")
				if len(base) == 10 && base[4] == '-' && base[7] == '-' {
					dailyFiles = append(dailyFiles, filepath.Join(monthPath, name))
				}
			}
		}
	}

	// Sort by filename descending (newest first).
	sort.Slice(dailyFiles, func(i, j int) bool {
		return filepath.Base(dailyFiles[i]) > filepath.Base(dailyFiles[j])
	})

	// Take at most 6.
	if len(dailyFiles) > 6 {
		dailyFiles = dailyFiles[:6]
	}

	// --- parse frontmatter from each file ---
	type cardInfo struct {
		date  string // YYYY-MM-DD
		title string
		desc  string
		link  string // Hugo page path
	}

	fmTitleRe := regexp.MustCompile(`(?m)^title:\s*"?([^"\n]+)"?\s*$`)
	fmDescRe := regexp.MustCompile(`(?m)^description:\s*"?([^"\n]+)"?\s*$`)

	var cards []cardInfo
	for _, fpath := range dailyFiles {
		raw, err := os.ReadFile(fpath)
		if err != nil {
			continue
		}
		content := string(raw)
		dateStr := strings.TrimSuffix(filepath.Base(fpath), ".md")

		title := dateStr + " AI资讯"
		if m := fmTitleRe.FindStringSubmatch(content); len(m) > 1 {
			title = strings.TrimSpace(m[1])
		}
		desc := ""
		if m := fmDescRe.FindStringSubmatch(content); len(m) > 1 {
			desc = strings.TrimSpace(m[1])
		}

		// Build Hugo link: {basePath}/YYYY/YYYY-MM/YYYY-MM-DD/
		year := dateStr[:4]
		yearMonth := dateStr[:7]
		link := fmt.Sprintf("%s/%s/%s/%s/", basePath, year, yearMonth, dateStr)

		cards = append(cards, cardInfo{date: dateStr, title: title, desc: desc, link: link})
	}

	// --- generate cards block (CSS class based, dark mode + responsive) ---
	var cardsBlock strings.Builder
	if len(cards) > 0 {
		cardsBlock.WriteString("<div class=\"briefing-card-grid\">\n")
		for _, c := range cards {
			subtitle := c.desc
			if len([]rune(subtitle)) > 80 {
				subtitle = TruncateAtSentence(string([]rune(subtitle)), 80)
			}
			if subtitle == "" {
				subtitle = c.date
			}
			fmt.Fprintf(&cardsBlock,
				"<a href=%q class=\"briefing-card\">"+
					"<strong>%s</strong><br><small>%s</small></a>\n",
				c.link, c.title, subtitle)
		}
		cardsBlock.WriteString("</div>")
	} else {
		cardsBlock.WriteString("<p><em>暂无日报内容</em></p>")
	}

	// --- replace LATEST_6_CARDS region ---
	text := string(original)
	cardsRe := regexp.MustCompile(`(?s)<!-- LATEST_6_CARDS_START -->.*?<!-- LATEST_6_CARDS_END -->`)
	replacement := "<!-- LATEST_6_CARDS_START -->\n" + cardsBlock.String() + "\n<!-- LATEST_6_CARDS_END -->"
	text = cardsRe.ReplaceAllString(text, replacement)

	// --- replace TODAY_LINK (plain HTML with basePath) ---
	if len(cards) > 0 {
		todayRe := regexp.MustCompile(`(?s)<!-- TODAY_LINK_START -->.*?<!-- TODAY_LINK_END -->`)
		todayLink := fmt.Sprintf(
			"<!-- TODAY_LINK_START -->\n"+
				"<div style=\"text-align:center;margin-bottom:2rem;\">\n"+
				"<a href=%q style=\"display:inline-block;padding:0.75rem 2rem;background:#3b82f6;color:#fff;border-radius:0.5rem;font-weight:600;text-decoration:none;font-size:1.05rem;transition:background 0.2s;\">查看今日早报 →</a>\n"+
				"</div>\n"+
				"<!-- TODAY_LINK_END -->",
			cards[0].link)
		text = todayRe.ReplaceAllString(text, todayLink)
	}

	if text == string(original) {
		return nil // nothing changed
	}
	// v1.0.1 Batch 2.15: atomic write.
	return atomicWriteFile(indexPath, []byte(text), 0o644)
}

func truncateDescription(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	runes := []rune(s)
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
		s = string(runes)
	}
	return s
}

// weeklyBackLinkFallbackPathPrefix 是 BRIEFING_REPORT_URL_BASE 环境变量缺失
// 或格式错时的 fallback site path. 不能 fallback 成裸 host-relative
// `/blog/weekly/...`, 否则浏览器在 ylzsdafei.github.io 上会解析成顶级
// 路径, 撞 GitHub "Site not found" 通用 404 (2026-04-27 实测).
//
// 历史教训: 上次 fallback 写成 `/blog/weekly/...`, env 配错就直接 break
// 用户体验. 这里硬编码 site path 作为 last-resort 安全垫.
const weeklyBackLinkFallbackPathPrefix = "/ai-daily-site"

// weeklyBackLink 算给定日期所属 ISO 周的周报回链 URL.
//
// 优先用 BRIEFING_REPORT_URL_BASE 提取 site root 拼完整 URL, 跟
// weekly.go:227 同款写法保证两边对齐.
//
// 如果 env 缺失或没有 `{{` 占位符 (格式不对), fallback 到带 site path
// 前缀的 host-relative URL, 仍然能落到本仓库 Pages 域内, 触发 footer.html
// 的 JS 拦截; 绝不返回裸 `/blog/weekly/...`.
func weeklyBackLink(date time.Time) string {
	isoYear, isoWeek := date.ISOWeek()
	if base := os.Getenv("BRIEFING_REPORT_URL_BASE"); base != "" {
		if idx := strings.Index(base, "{{"); idx > 0 {
			siteRoot := strings.TrimRight(base[:idx], "/")
			return fmt.Sprintf("%s/blog/weekly/%d-w%02d/", siteRoot, isoYear, isoWeek)
		}
	}
	return fmt.Sprintf("%s/blog/weekly/%d-w%02d/",
		weeklyBackLinkFallbackPathPrefix, isoYear, isoWeek)
}
