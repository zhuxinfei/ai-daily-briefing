package generate

import (
	_ "embed"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// BannedPattern describes a regex pattern that, if matched in insight output,
// causes validation to fail. Ported from slack-notify.js INSIGHT_BANNED_PATTERNS.
type BannedPattern struct {
	Pattern *regexp.Regexp
	Reason  string
}

// bannedPatterns mirrors INSIGHT_BANNED_PATTERNS in slack-notify.js (rows 18-34).
// These reject operational / ops-leakage language from the LLM output.
var bannedPatterns = []BannedPattern{
	{Pattern: regexp.MustCompile(`(?i)webhook`), Reason: "包含发布通道细节"},
	{Pattern: regexp.MustCompile(`(?i)\bcron\b`), Reason: "包含调度实现细节"},
	{Pattern: regexp.MustCompile(`(?i)\bschedule\b`), Reason: "包含调度实现细节"},
	{Pattern: regexp.MustCompile(`(?i)GitHub Actions`), Reason: "包含运维平台细节"},
	{Pattern: regexp.MustCompile(`缓存`), Reason: "包含缓存或排障细节"},
	{Pattern: regexp.MustCompile(`轮询`), Reason: "包含调度策略细节"},
	{Pattern: regexp.MustCompile(`幂等`), Reason: "包含工程实现细节"},
	{Pattern: regexp.MustCompile(`北京时间`), Reason: "包含排障时间戳"},
	{Pattern: regexp.MustCompile(`\b\d{1,2}:\d{2}(?::\d{2})?\b`), Reason: "包含具体运行时间"},
	{Pattern: regexp.MustCompile(`推送链路`), Reason: "包含内部投递链路表述"},
	{Pattern: regexp.MustCompile(`测试频道`), Reason: "包含内部频道信息"},
	{Pattern: regexp.MustCompile(`正式频道`), Reason: "包含内部频道信息"},
	{Pattern: regexp.MustCompile(`告警`), Reason: "包含内部监控表述"},
	{Pattern: regexp.MustCompile(`补发`), Reason: "包含内部操作表述"},
	{Pattern: regexp.MustCompile(`本地设备`), Reason: "包含排障上下文"},
}

// ValidationResult describes the outcome of validating an insight/compose
// string.
//
// Hard contract: OK is false iff at least one entry is in Reasons. Callers
// that implement retry loops (see openai.go GenerateInsight) short-circuit
// when OK==true, so only truly load-bearing checks should contribute to
// Reasons. Soft / nice-to-have contract violations (e.g. missing glossary
// annotations, missing mermaid block) must be appended to Warnings instead
// so they surface in logs without forcing a retry.
type ValidationResult struct {
	OK          bool
	Reasons     []string
	Warnings    []string
	IndustryRaw string
	OurRaw      string
}

// splitInsightRegex splits on the "💭 对我们的启发" header, optionally eating leading
// '#' markers. Mirrors the JS split regex.
var splitInsightRegex = regexp.MustCompile(`(?:#{1,6}\s*)?💭\s*对我们的启发[^）]*[）)]?\s*`)

// industryHeaderRegex strips the "📊 行业洞察" header from the industry chunk.
var industryHeaderRegex = regexp.MustCompile(`(?:#{1,6}\s*)?📊\s*行业洞察[^）]*[）)]?\s*\n*`)

// leadingHeaderRegex strips leading markdown header symbols at the start of any line.
var leadingHeaderRegex = regexp.MustCompile(`(?m)^#{1,6}\s+`)

// trailingHeaderRegex strips any trailing '##' markers left dangling at the end.
var trailingHeaderRegex = regexp.MustCompile(`\s*#{1,6}\s*$`)

// numberedItemRegex counts "1.", "2." style lines. Mirrors JS countNumberedItems.
var numberedItemRegex = regexp.MustCompile(`(?m)^\d+\.`)
var numberedItemCaptureRegex = regexp.MustCompile(`(?m)^(\d+)\.`)

// mermaidBlockRegex matches a fenced ```mermaid ... ``` block anywhere in raw.
// Used by the structural contract check in ValidateInsight — mermaid is a
// soft requirement (relationship diagram), so missing fence only contributes
// to Warnings, never to Reasons.
var mermaidBlockRegex = regexp.MustCompile("(?s)```mermaid\\s[\\s\\S]*?```")

// ParseInsightSections splits the LLM output into its "industry insight" and
// "for us" halves. It mirrors parseInsightSections() in slack-notify.js
// (rows 82-98), including the '##' cleanup that was added recently.
func ParseInsightSections(raw string) (industry, our string) {
	if raw == "" {
		return "", ""
	}

	parts := splitInsightRegex.Split(raw, 2)

	var industryRaw, ourRaw string
	if len(parts) > 0 {
		industryRaw = parts[0]
	}
	if len(parts) > 1 {
		ourRaw = parts[1]
	}

	// Clean industry side: strip leading "📊 行业洞察..." header,
	// strip leading '## ' per-line, strip trailing '##'.
	industryRaw = industryHeaderRegex.ReplaceAllString(industryRaw, "")
	industryRaw = leadingHeaderRegex.ReplaceAllString(industryRaw, "")
	industryRaw = trailingHeaderRegex.ReplaceAllString(industryRaw, "")
	industryRaw = strings.TrimSpace(industryRaw)

	// Clean our side: same header stripping (but no "📊 行业洞察" prefix).
	ourRaw = leadingHeaderRegex.ReplaceAllString(ourRaw, "")
	ourRaw = trailingHeaderRegex.ReplaceAllString(ourRaw, "")
	ourRaw = strings.TrimSpace(ourRaw)

	return industryRaw, ourRaw
}

// countNumberedItems mirrors the JS countNumberedItems: count lines beginning
// with "\d+.".
func countNumberedItems(text string) int {
	return len(numberedItemRegex.FindAllString(text, -1))
}

func hasSequentialNumbering(text string) bool {
	matches := numberedItemCaptureRegex.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return false
	}
	expected := 1
	for _, m := range matches {
		if len(m) < 2 {
			return false
		}
		if itoa(expected) != m[1] {
			return false
		}
		expected++
	}
	return true
}

// ValidateInsight checks that the LLM output contains both sections, that
// each section has the expected bullet count, and that no banned pattern
// appears. Mirrors validateInsightOutput() in slack-notify.js (rows 104-132).
//
// Beyond the JS port it also runs two v1.0.1 checks:
//   - annotation coverage (jargon without nearby 括号注释) → Warnings
//   - mermaid block presence → Warnings
//
// 注释覆盖保持软要求：它很重要，但不能因为个别名词漏注释就让整期日报
// 直接失败，避免“好内容因局部表述问题整期报废”。
func ValidateInsight(raw string) ValidationResult {
	industryRaw, ourRaw := ParseInsightSections(raw)

	var reasons []string

	if industryRaw == "" || ourRaw == "" {
		reasons = append(reasons, "缺少\"行业洞察\"或\"对我们的启发\"模块")
	}

	industryCount := countNumberedItems(industryRaw)
	ourCount := countNumberedItems(ourRaw)

	if industryCount < 3 || industryCount > 4 {
		reasons = append(reasons,
			"行业洞察条数异常（当前 "+itoa(industryCount)+" 条）")
	}
	if ourCount < 2 || ourCount > 3 {
		reasons = append(reasons,
			"对我们的启发条数异常（当前 "+itoa(ourCount)+" 条）")
	}
	if !hasSequentialNumbering(industryRaw) {
		reasons = append(reasons, "行业洞察编号不连续或未从 1 开始")
	}
	if !hasSequentialNumbering(ourRaw) {
		reasons = append(reasons, "对我们的启发编号不连续或未从 1 开始")
	}
	if industryCount > 0 && strings.Count(industryRaw, "【洞察】") < industryCount {
		reasons = append(reasons, "行业洞察缺少对应的【洞察】判断行")
	}

	for _, bp := range bannedPatterns {
		if bp.Pattern.MatchString(raw) {
			reasons = append(reasons, bp.Reason)
		}
	}

	warnings := checkAnnotationCoverage(raw)

	if !mermaidBlockRegex.MatchString(raw) {
		warnings = append(warnings, "missing mermaid relationship diagram")
	}

	return ValidationResult{
		OK:          len(reasons) == 0,
		Reasons:     reasons,
		Warnings:    warnings,
		IndustryRaw: industryRaw,
		OurRaw:      ourRaw,
	}
}

// ValidateCompose runs the same soft annotation-coverage check on a single
// section's markdown body produced by the Summarize pipeline (summarize.go).
//
// As of Batch 3 the Compose stage has no hard structural contract beyond
// "body must be non-empty" (that is enforced directly inside Summarize).
// So this function's job is to surface non-annotated jargon so the retry
// loop (see Batch 2 item 2.4 / D5 decision) can decide whether to retry.
//
// Callers should treat len(Reasons)==0 as "keep this output" and log
// len(Warnings)>0 without blocking the pipeline.
func ValidateCompose(sectionMD string) ValidationResult {
	warnings := checkAnnotationCoverage(sectionMD)

	var reasons []string
	if strings.TrimSpace(sectionMD) == "" {
		reasons = append(reasons, "compose 输出为空")
	}

	return ValidationResult{
		OK:       len(reasons) == 0,
		Reasons:  reasons,
		Warnings: warnings,
	}
}

// itoa is a tiny int->string helper to avoid pulling strconv just for this.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// ---------------------------------------------------------------------------
// Glossary + annotation coverage
// ---------------------------------------------------------------------------

//go:embed glossary.yaml
var glossaryYAML []byte

// Glossary is the parsed form of glossary.yaml. `WellKnown` are terms so
// universally recognised that an un-annotated occurrence is fine; `Jargon`
// are terms that MUST be followed (within annotationWindow runes) by a
// parenthesised Chinese-or-ASCII explanation.
type Glossary struct {
	WellKnown []string `yaml:"well_known"`
	Jargon    []string `yaml:"jargon"`
}

// annotationWindow is how many runes on either side of a jargon hit we
// scan looking for a nearby （注释） / (annotation). 30 runes either way
// was chosen because a realistic annotation pattern like
// "RAG（检索增强生成，先查资料再回答）" places its closing brace within
// roughly 20 runes of the term.
const annotationWindow = 30

var (
	glossaryOnce sync.Once
	glossaryData *Glossary
	glossaryErr  error

	// Precompiled once `loadGlossary` succeeds. Patterns and terms are
	// sorted longest-first so "HuggingFace" matches before "Hugging"
	// would (prevents partial-prefix false negatives). Both slices share
	// the same index.
	jargonPatterns []*regexp.Regexp
	jargonTerms    []string
)

// loadGlossary lazily parses the embedded glossary.yaml and compiles the
// jargon-detection regexes. Safe for concurrent callers.
func loadGlossary() (*Glossary, error) {
	glossaryOnce.Do(func() {
		var g Glossary
		if err := yaml.Unmarshal(glossaryYAML, &g); err != nil {
			glossaryErr = fmt.Errorf("generate: parse glossary.yaml: %w", err)
			return
		}
		glossaryData = &g

		// Sort jargon longest-first so multi-word terms win over prefixes.
		terms := make([]string, 0, len(g.Jargon))
		for _, t := range g.Jargon {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			terms = append(terms, t)
		}
		sort.Slice(terms, func(i, j int) bool {
			return len([]rune(terms[i])) > len([]rune(terms[j]))
		})

		jargonPatterns = make([]*regexp.Regexp, 0, len(terms))
		jargonTerms = make([]string, 0, len(terms))
		for _, t := range terms {
			// ASCII identifiers get \b boundaries (prevents "RAG" from firing
			// inside "RAGE"). CJK terms rely on literal match since Go regexp
			// \b doesn't treat CJK as word chars.
			escaped := regexp.QuoteMeta(t)
			var re *regexp.Regexp
			if isASCIIIdent(t) {
				re = regexp.MustCompile(`(?i)\b` + escaped + `\b`)
			} else {
				re = regexp.MustCompile(escaped)
			}
			jargonPatterns = append(jargonPatterns, re)
			jargonTerms = append(jargonTerms, t)
		}
	})
	return glossaryData, glossaryErr
}

// isASCIIIdent reports whether the term contains only ASCII letters, digits,
// hyphen, or space — in which case \b word boundaries work correctly.
func isASCIIIdent(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}

// hasNearbyAnnotation reports whether a （...） or (...) clause appears
// within ±annotationWindow runes of the match at [matchStart, matchEnd)
// inside rawRunes. Both full-width （）and half-width () are accepted
// since editors routinely mix them.
func hasNearbyAnnotation(rawRunes []rune, matchStart, matchEnd int) bool {
	lo := matchStart - annotationWindow
	if lo < 0 {
		lo = 0
	}
	hi := matchEnd + annotationWindow
	if hi > len(rawRunes) {
		hi = len(rawRunes)
	}
	window := string(rawRunes[lo:hi])

	// We want an OPEN paren followed by a CLOSE paren (mixed or matched).
	// A bare "(" with no ")" within the window is not an annotation.
	openIdx := strings.IndexAny(window, "（(")
	if openIdx < 0 {
		return false
	}
	after := window[openIdx:]
	closeIdx := strings.IndexAny(after, "）)")
	return closeIdx > 0 // >0 so we require at least one char inside
}

// checkAnnotationCoverage walks the text, finds every jargon hit, and
// returns a warning for each hit whose ±annotationWindow runes have no
// parenthesised annotation. Terms in WellKnown are skipped entirely.
//
// Returns nil (not an empty slice) when no warnings — callers can then
// `append` safely.
func checkAnnotationCoverage(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	g, err := loadGlossary()
	if err != nil || g == nil {
		// Glossary failed to load — fail quiet. This is soft validation;
		// we never want a broken embed to block production.
		return nil
	}

	// Build a set for well_known so we can short-circuit if by accident
	// the same term is in both lists (shouldn't happen, but defensive).
	wk := make(map[string]struct{}, len(g.WellKnown))
	for _, t := range g.WellKnown {
		wk[strings.ToLower(strings.TrimSpace(t))] = struct{}{}
	}

	runes := []rune(text)

	// Track first reported offset per term to avoid flooding Warnings
	// with the same jargon reported 5 times for the same paragraph.
	reported := make(map[string]bool)
	var warnings []string

	for i, re := range jargonPatterns {
		actualTerm := jargonTerms[i]
		if _, skip := wk[strings.ToLower(actualTerm)]; skip {
			continue
		}
		if reported[actualTerm] {
			continue
		}

		loc := re.FindStringIndex(text)
		if loc == nil {
			continue
		}
		// Convert byte offsets to rune offsets for ±window math.
		matchStartRune := len([]rune(text[:loc[0]]))
		matchEndRune := len([]rune(text[:loc[1]]))

		if hasNearbyAnnotation(runes, matchStartRune, matchEndRune) {
			continue
		}

		warnings = append(warnings,
			fmt.Sprintf("jargon %q 缺少括号注释（读者可能看不懂）", actualTerm))
		reported[actualTerm] = true
	}

	return warnings
}
