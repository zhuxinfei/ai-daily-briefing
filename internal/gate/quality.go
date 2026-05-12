// Package gate implements the briefing-v3 hard quality gate.
//
// The gate is the last line of defence before an Issue is published to
// a user-facing channel: if any rule fails, the pipeline MUST refuse to
// push the issue to the production Slack channel (and may alternately
// route it to the test channel plus an operator alert). Nothing is ever
// silently degraded — every failure reason is returned so callers can
// log and display it.
//
// Rules are intentionally dumb and side-effect free. They operate on
// the already-assembled Issue / IssueItem / IssueInsight tuples; they
// do not re-fetch or re-parse anything.
package gate

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"

	"briefing-v3/internal/store"
)

// Config captures the numeric thresholds and banned pattern list that
// drive a single Gate instance. Values correspond 1:1 with the `gate:`
// block in config/ai.yaml, plus BannedPatterns which defaults to
// DefaultBannedPatterns() when Config is built by the pipeline.
type Config struct {
	MinItems               int
	MinSectionsWithContent int
	MinInsightChars        int
	MinIndustryBullets     int
	MaxIndustryBullets     int
	MinTakeawayBullets     int
	MaxTakeawayBullets     int
	MinSourceDomains       int
	BannedPatterns         []*regexp.Regexp
}

// Report is the outcome of a single gate check.
//
// v1.0.0 introduces a tri-state outcome:
//
//   - Pass == true                       → green, safe for prod promotion
//   - Pass == false && Warn == true      → yellow, emit to test channel
//     with a "质量待审" header
//   - Pass == false && Warn == false     → red, hard fail, skip prod and
//     post an alert to the test channel
//
// Hard-fail conditions (Pass=false, Warn=false):
//   - issue is nil, or
//   - items == 0, or
//   - 0 sections with content, or
//   - insight is nil, or
//   - every configured section failed compose (FailedSections covers all)
//
// Soft-warn conditions (Pass=false, Warn=true) trigger when none of the
// hard-fail conditions apply but any of the numeric thresholds below
// the configured min (items, sections, insight chars, bullet counts,
// source domains), or when FailedSections is non-empty.
//
// All counters are filled in regardless of outcome so callers can
// display them side-by-side with the minimum thresholds.
type Report struct {
	Pass              bool
	Warn              bool     // true when Pass=false but only soft warnings triggered
	Reasons           []string // hard-fail reasons
	Warnings          []string // soft-warn reasons
	ItemCount         int
	SectionCount      int
	InsightChars      int
	IndustryBullets   int
	TakeawayBullets   int
	SourceDomainCount int
	BannedHits        []string
	FailedSections    []string // sections compose degraded on (from run.go)
}

// Gate holds a resolved Config and exposes Check to callers.
type Gate struct {
	cfg Config
}

// New constructs a Gate from cfg. If cfg.BannedPatterns is nil the
// defaults from DefaultBannedPatterns() are used.
func New(cfg Config) *Gate {
	if cfg.BannedPatterns == nil {
		cfg.BannedPatterns = DefaultBannedPatterns()
	}
	return &Gate{cfg: cfg}
}

// DefaultBannedPatterns returns the canonical set of regexes that
// reject ops-leakage wording in the LLM output. Kept in sync with
// internal/generate/validator.go bannedPatterns — duplicated here so
// the gate package can stand alone without importing generate.
func DefaultBannedPatterns() []*regexp.Regexp {
	return []*regexp.Regexp{
		regexp.MustCompile(`(?i)webhook`),
		regexp.MustCompile(`(?i)\bcron\b`),
		regexp.MustCompile(`(?i)\bschedule\b`),
		regexp.MustCompile(`(?i)GitHub Actions`),
		regexp.MustCompile(`缓存`),
		regexp.MustCompile(`轮询`),
		regexp.MustCompile(`幂等`),
		regexp.MustCompile(`北京时间`),
		regexp.MustCompile(`\b\d{1,2}:\d{2}(?::\d{2})?\b`),
		regexp.MustCompile(`推送链路`),
		regexp.MustCompile(`测试频道`),
		regexp.MustCompile(`正式频道`),
		regexp.MustCompile(`告警`),
		regexp.MustCompile(`补发`),
		regexp.MustCompile(`本地设备`),
	}
}

// Check evaluates all gate rules against the supplied issue bundle and
// returns a Report.
//
// failedSections is the list of section ids that compose gracefully
// degraded on (e.g. upstream 502). totalSections is the total number of
// sections configured in config/ai.yaml — needed to detect the "every
// section failed" hard-fail case. Pass both as empty/zero if the
// caller does not care about compose degradation.
//
// The issue parameter is accepted for forward compatibility (e.g. rules
// that key on IssueDate or Status) but is currently only used to
// early-return on nil input.
func (g *Gate) Check(
	issue *store.Issue,
	items []*store.IssueItem,
	insight *store.IssueInsight,
	failedSections []string,
	totalSections int,
) *Report {
	r := &Report{
		Pass:           true,
		Reasons:        []string{},
		Warnings:       []string{},
		BannedHits:     []string{},
		FailedSections: append([]string(nil), failedSections...),
	}

	// ----- Hard fail conditions (pipeline is structurally broken) -----

	if issue == nil {
		r.Pass = false
		r.Reasons = append(r.Reasons, "issue 为空")
		return r
	}

	// Items count.
	r.ItemCount = len(items)

	// Section coverage: how many distinct non-empty sections are present.
	seen := make(map[string]struct{}, 8)
	for _, it := range items {
		if it == nil {
			continue
		}
		if strings.TrimSpace(it.Section) == "" {
			continue
		}
		if strings.TrimSpace(it.BodyMD) == "" && strings.TrimSpace(it.Title) == "" {
			continue
		}
		seen[it.Section] = struct{}{}
	}
	r.SectionCount = len(seen)

	// Insight presence (hard fail if completely missing).
	if insight == nil {
		r.Reasons = append(r.Reasons, "缺少洞察")
	}

	// Structural hard-fail checks: zero items, zero sections, or every
	// configured section failed to compose.
	if r.ItemCount == 0 {
		r.Reasons = append(r.Reasons, "条目为零")
	}
	if r.SectionCount == 0 {
		r.Reasons = append(r.Reasons, "无有效 section 内容")
	}
	// v1.0.1 Bug B 修复: 任一 section 撰稿失败即 hard fail.
	// 用户规则 "文字必齐 / 零降级": 5 个 section 任一缺失都不允许推 prod,
	// 必须通过 orchestrator 重试 + briefing repair 补齐才能发出.
	// (旧 v1.0.0 逻辑仅在"所有 section 都挂了"才 hard fail, 这是设计错误.)
	if len(failedSections) > 0 {
		r.Reasons = append(r.Reasons, "section 撰稿失败: "+strings.Join(failedSections, ","))
	}

	// Insight numeric checks: these are fatal if insight is present
	// but the content itself is malformed (bullet counts wildly out of
	// range or banned patterns). Length / bullet-count soft-warns are
	// handled in the soft-warn section below.
	if insight != nil {
		industryMD := strings.TrimSpace(insight.IndustryMD)
		ourMD := strings.TrimSpace(insight.OurMD)
		combined := industryMD + ourMD
		r.InsightChars = countRunes(combined)
		r.IndustryBullets = countNumberedBullets(industryMD)
		r.TakeawayBullets = countNumberedBullets(ourMD)

		for _, re := range g.cfg.BannedPatterns {
			if re == nil {
				continue
			}
			if re.MatchString(industryMD) || re.MatchString(ourMD) {
				r.BannedHits = append(r.BannedHits, re.String())
			}
		}
		if len(r.BannedHits) > 0 {
			r.Reasons = append(r.Reasons, "洞察包含禁用词")
		}
	}

	// Source diversity — count regardless of hard/soft classification.
	domains := extractDomains(items)
	r.SourceDomainCount = len(domains)

	// ----- Soft warn conditions (pipeline is producing but degraded) -----

	// Only consider soft warnings when no hard-fail was recorded, so we
	// don't muddle a fatal outcome with non-load-bearing warnings.
	if len(r.Reasons) == 0 {
		if r.ItemCount < g.cfg.MinItems {
			r.Warnings = append(r.Warnings, "条目不足")
		}
		if r.SectionCount < g.cfg.MinSectionsWithContent {
			r.Warnings = append(r.Warnings, "section 覆盖不足")
		}
		if insight != nil {
			if r.InsightChars < g.cfg.MinInsightChars {
				r.Warnings = append(r.Warnings, "洞察字数不足")
			}
			if r.IndustryBullets < g.cfg.MinIndustryBullets || r.IndustryBullets > g.cfg.MaxIndustryBullets {
				r.Warnings = append(r.Warnings, "行业洞察条数超出范围")
			}
			if r.TakeawayBullets < g.cfg.MinTakeawayBullets || r.TakeawayBullets > g.cfg.MaxTakeawayBullets {
				r.Warnings = append(r.Warnings, "启发条数超出范围")
			}
		}
		if r.SourceDomainCount < g.cfg.MinSourceDomains {
			r.Warnings = append(r.Warnings, "源多样性不足")
		}
		// v1.0.1: failedSections 已在上方 hard-fail 段处理, 此处不再涉及.
	}

	// ----- Finalize -----

	if len(r.Reasons) > 0 {
		r.Pass = false
		r.Warn = false
	} else if len(r.Warnings) > 0 {
		r.Pass = false
		r.Warn = true
	} else {
		r.Pass = true
		r.Warn = false
	}
	return r
}

// ----- helpers -----

var gateNumberedRe = regexp.MustCompile(`(?m)^\s*\d+\.`)

// countNumberedBullets counts lines that begin with "N." (optionally
// preceded by whitespace). Used to validate bullet counts in the
// insight sections.
func countNumberedBullets(text string) int {
	if text == "" {
		return 0
	}
	return len(gateNumberedRe.FindAllString(text, -1))
}

// countRunes returns the number of Unicode code points in s. Used for
// CJK-aware "字数" (character count) limits — byte-length would
// dramatically under-count Chinese content.
func countRunes(s string) int {
	return len([]rune(s))
}

// extractDomains walks every item's SourceURLsJSON (a JSON array of
// strings) and returns a de-duplicated slice of hostnames. Items with
// malformed JSON or invalid URLs are skipped silently; the gate only
// cares about the unique-domain count, not about error propagation.
func extractDomains(items []*store.IssueItem) []string {
	seen := make(map[string]struct{}, 16)
	for _, it := range items {
		if it == nil {
			continue
		}
		raw := strings.TrimSpace(it.SourceURLsJSON)
		if raw == "" {
			continue
		}
		var urls []string
		if err := json.Unmarshal([]byte(raw), &urls); err != nil {
			continue
		}
		for _, u := range urls {
			host := hostFromURL(u)
			if host == "" {
				continue
			}
			seen[host] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	return out
}

// hostFromURL returns the lowercased hostname for raw, stripped of any
// leading "www.". Returns an empty string on parse failure or when the
// URL has no host.
func hostFromURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	host = strings.TrimPrefix(host, "www.")
	return host
}
