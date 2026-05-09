// Package compose stitches classified RawItems plus LLM-generated
// section markdown into the final []*store.IssueItem used by the
// persistence and render layers.
//
// Pipeline position:
//
//	rank → classify → compose → render
//
// compose is the first stage that crosses back into the store.IssueItem
// shape. It does NOT persist anything — callers (cmd/briefing) must
// pass the returned IssueItems to store.Store.InsertIssueItems.
//
// v1.0.0: Compose() now degrades a failed section (e.g. summarizer
// returns a 502 from the upstream gateway) into a logged warning and
// continues with the remaining sections. The failed section ids are
// returned alongside the IssueItems so the caller can surface them in
// the Slack message / gate decision.
package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"briefing-v3/internal/generate"
	"briefing-v3/internal/store"
)

// sectionRetryRounds 是 compose 完一遍所有 section 后, 对仍失败的 section
// 再整段重试的轮数. v1.0.1 Phase 4.2.1 根因修复 (2026-04-14):
// A/B 测试中 social section 遇到 LLM 502 导致 gate hard fail; Summarize 自
// 带的 5 次 exp-backoff 重试可能因 ctx 取消或 upstream 持续失稳而不完全
// 生效. 加一轮 section 级外层重试 (先完成其他 section 给 upstream 喘息
// 几分钟, 然后再试失败的那条), 覆盖"局部时间窗抖动"场景. 用户原则"文字
// 必补齐", 这正是补的路径.
const sectionRetryRounds = 1

// sectionRetryWait 是两轮之间的等待时长. 30s 是经验值: 既让 upstream 有
// 时间缓过来, 又不至于把总耗时推过 orchestrator 06:00→07:30 的 90 min 预算.
// var (非 const) 是为了单元测试能短路等待.
var sectionRetryWait = 30 * time.Second

// SectionConfig mirrors the 'sections' block of config/ai.yaml. Only the
// fields compose actually consumes are declared here to keep compose
// decoupled from the YAML loader.
type SectionConfig struct {
	ID       string
	Title    string
	MinItems int
	MaxItems int
}

// Composer is the public interface of this package.
type Composer interface {
	// Compose turns the classified buckets into ordered IssueItems
	// ready for insertion. issueID is set on every returned IssueItem.
	// sectioned is the output of classify.Classifier.Classify.
	// sections is the ordered list of section configs (typically read
	// from config/ai.yaml).
	// summarizer is the LLM text generator from the generate package;
	// a nil summarizer returns an error (compose refuses to make up
	// body text without an LLM).
	//
	// Returns (items, failedSections, error). failedSections is the
	// list of section ids whose summarizer call raised an error (e.g.
	// 502 upstream) and which were skipped gracefully. A non-nil error
	// means compose itself failed setup (nil summarizer, no sections);
	// per-section failures never propagate as a hard error.
	Compose(
		ctx context.Context,
		issueID int64,
		sectioned map[string][]*store.RawItem,
		sections []SectionConfig,
		summarizer generate.Summarizer,
	) ([]*store.IssueItem, []string, error)
}

// New returns a default Composer. It is stateless so a single instance
// can be shared across goroutines.
func New() Composer {
	return &composer{}
}

type composer struct{}

// Compose walks sections in order, caps each bucket at MaxItems, asks
// the Summarizer to produce the section's markdown body, then splits
// that markdown into one IssueItem per numbered entry.
//
// v1.0.0 behaviour: a single section's summarizer failure is logged
// and its id is collected into failedSections; compose keeps going
// with the remaining sections. The returned items slice is whatever
// sections DID succeed; it can be empty if every section failed, in
// which case the caller's gate will flag it as a hard fail.
func (c *composer) Compose(
	ctx context.Context,
	issueID int64,
	sectioned map[string][]*store.RawItem,
	sections []SectionConfig,
	summarizer generate.Summarizer,
) ([]*store.IssueItem, []string, error) {
	if summarizer == nil {
		return nil, nil, errors.New("compose: Summarizer is required")
	}
	if len(sections) == 0 {
		return nil, nil, errors.New("compose: no sections configured")
	}

	var out []*store.IssueItem
	var failedSections []string

	for _, sec := range sections {
		items := sectioned[sec.ID]
		if len(items) == 0 {
			continue
		}

		// Cap at MaxItems. The items within a section are already
		// ranked by score (the classify step preserved rank order), so
		// simply taking the prefix keeps the strongest entries.
		if sec.MaxItems > 0 && len(items) > sec.MaxItems {
			items = items[:sec.MaxItems]
		}

		md, err := summarizer.Summarize(ctx, sec.Title, items)
		if err != nil {
			// v1.0.0 degradation: a single section's summarizer failure
			// (e.g. upstream 502) is logged and skipped. The pipeline
			// continues and the caller's gate downgrades to warn.
			log.Printf("[WARN] compose: summarize section %q failed: %v (继续, 整体结束后会重试)", sec.ID, err)
			failedSections = append(failedSections, sec.ID)
			continue
		}
		md = strings.TrimSpace(md)
		if md == "" {
			// Empty response is treated as a soft failure: the section
			// contributes nothing but is flagged so the gate can warn.
			log.Printf("[WARN] compose: summarize section %q returned empty output (继续, 整体结束后会重试)", sec.ID)
			failedSections = append(failedSections, sec.ID)
			continue
		}

		issueItems := splitMarkdownIntoIssueItems(md, sec.ID, issueID, items)
		out = append(out, issueItems...)
	}

	// v1.0.1 Phase 4.2.1 section 级二次重试 (2026-04-14 根因修复).
	// 经过 sectionRetryWait 等待后对还失败的 section 重跑 Summarize.
	// 目的是覆盖"局部时间窗 502 抖动", 避免依赖 orchestrator 重跑整个 run.
	// Summarize 内部已经有 5 次 exp-backoff, 这里再加一层补给.
	sectionByID := make(map[string]SectionConfig, len(sections))
	for _, sec := range sections {
		sectionByID[sec.ID] = sec
	}
	if len(failedSections) > 0 && sectionRetryRounds > 0 {
		for round := 1; round <= sectionRetryRounds; round++ {
			if len(failedSections) == 0 {
				break
			}
			log.Printf("[INFO] compose: retry round %d for %d failed section(s): %v (waiting %s)",
				round, len(failedSections), failedSections, sectionRetryWait)
			select {
			case <-ctx.Done():
				log.Printf("[WARN] compose: retry cancelled by ctx (%v), giving up", ctx.Err())
				return out, failedSections, nil
			case <-time.After(sectionRetryWait):
			}

			var stillFailed []string
			for _, secID := range failedSections {
				sec, ok := sectionByID[secID]
				if !ok {
					stillFailed = append(stillFailed, secID)
					continue
				}
				items := sectioned[secID]
				if len(items) == 0 {
					// Shouldn't happen (first pass would have skipped), but
					// guard against it: drop silently from retry list.
					continue
				}
				if sec.MaxItems > 0 && len(items) > sec.MaxItems {
					items = items[:sec.MaxItems]
				}
				md, err := summarizer.Summarize(ctx, sec.Title, items)
				if err != nil {
					log.Printf("[WARN] compose retry %d: section %q still failing: %v", round, secID, err)
					stillFailed = append(stillFailed, secID)
					continue
				}
				md = strings.TrimSpace(md)
				if md == "" {
					log.Printf("[WARN] compose retry %d: section %q still empty", round, secID)
					stillFailed = append(stillFailed, secID)
					continue
				}
				issueItems := splitMarkdownIntoIssueItems(md, secID, issueID, items)
				out = append(out, issueItems...)
				log.Printf("[INFO] compose retry %d: section %q recovered (%d items)", round, secID, len(issueItems))
			}
			failedSections = stillFailed
		}
	}

	// v1.0.1 Phase 4.5 (T14): 二次重试都失败 → 用 raw items 拼简版 markdown 兜底.
	// 用户原则: 早报"文字必补齐", 即使 LLM 持续不稳, 5 个 section 也要有内容.
	// 这一段不依赖任何 LLM, 100% 能产出 — 但内容朴素, 加 ⚠️ 标记让 reader
	// 看出本节降级了.
	if len(failedSections) > 0 {
		var stillFailedAfterFallback []string
		for _, secID := range failedSections {
			items := sectioned[secID]
			if len(items) == 0 {
				stillFailedAfterFallback = append(stillFailedAfterFallback, secID)
				continue
			}
			sec, ok := sectionByID[secID]
			if !ok {
				stillFailedAfterFallback = append(stillFailedAfterFallback, secID)
				continue
			}
			if sec.MaxItems > 0 && len(items) > sec.MaxItems {
				items = items[:sec.MaxItems]
			}
			// T14 fallback 两层: (1) 简化 LLM 调用 — 只给 title+URL, prompt
			// 小, 比 compose 正常 prompt 更容易过 LLM 502. 目的是拿到中文
			// 产出. (2) 如 LLM 也挂, 用 raw items 原样拼 (可能含英文 title).
			md := ""
			simpleItems := buildSimplifiedItems(items)
			if simpleMd, simpleErr := summarizer.Summarize(ctx, sec.Title, simpleItems); simpleErr == nil {
				simpleMd = strings.TrimSpace(simpleMd)
				if simpleMd != "" {
					md = simpleMd
					log.Printf("[INFO] compose fallback: section %q used simplified-LLM fallback (%d items)", secID, len(items))
				}
			} else {
				log.Printf("[WARN] compose fallback: section %q simplified-LLM also failed (%v), using raw-items", secID, simpleErr)
			}
			if md == "" {
				md = composeFallbackFromRawItems(items)
				log.Printf("[INFO] compose fallback: section %q used raw-items fallback (%d items, 原文可能含英文)", secID, len(items))
			}
			if strings.TrimSpace(md) == "" {
				stillFailedAfterFallback = append(stillFailedAfterFallback, secID)
				continue
			}
			issueItems := splitMarkdownIntoIssueItems(md, secID, issueID, items)
			out = append(out, issueItems...)
		}
		failedSections = stillFailedAfterFallback
	}

	return out, failedSections, nil
}

// buildSimplifiedItems 构建"极简版 RawItem 列表"给 T14 fallback 的简化 LLM
// 调用用 — 只保留 Title 和 URL, 把 Content 清空. Prompt 小很多, 触发 LLM 502
// 概率也低; 成功的话 LLM 仍会按系统 prompt 要求输出中文, 解决 fallback 时
// 研究论文 title 全是英文的问题.
func buildSimplifiedItems(items []*store.RawItem) []*store.RawItem {
	out := make([]*store.RawItem, 0, len(items))
	for _, it := range items {
		if it == nil || strings.TrimSpace(it.Title) == "" {
			continue
		}
		out = append(out, &store.RawItem{
			ID:           it.ID,
			DomainID:     it.DomainID,
			SourceID:     it.SourceID,
			ExternalID:   it.ExternalID,
			URL:          it.URL,
			Title:        it.Title,
			Author:       it.Author,
			PublishedAt:  it.PublishedAt,
			FetchedAt:    it.FetchedAt,
			Content:      "", // 清空, 让 prompt 变小
			MetadataJSON: "",
		})
	}
	return out
}

// composeFallbackFromRawItems 用 raw items 直接拼简版 markdown, 不调 LLM.
// 当 compose 多轮 retry 都失败时, 这是最后兜底, 保证 section 不为空.
// 输出格式跟 LLM 输出一致 (1. **title。** body), 但加 ⚠️ 提示标签.
func composeFallbackFromRawItems(items []*store.RawItem) string {
	var b strings.Builder
	emitted := 0
	for _, it := range items {
		if it == nil {
			continue
		}
		title := strings.TrimSpace(it.Title)
		if title == "" {
			continue
		}
		body := strings.TrimSpace(it.Content)
		// 截 body 到 200 字 (符 IssueItem 习惯).
		if rs := []rune(body); len(rs) > 200 {
			body = string(rs[:200]) + "..."
		}
		emitted++
		if emitted == 1 {
			b.WriteString("> ⚠️ 本节 AI 撰稿失败, 以下为原始候选条目\n\n")
		}
		b.WriteString(fmt.Sprintf("%d. **%s。**\n", emitted, title))
		if body != "" && body != title {
			b.WriteString(body)
			b.WriteString("\n")
		}
		if it.URL != "" {
			b.WriteString(fmt.Sprintf("[查看原文(briefing)](%s)\n", it.URL))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// numberedEntryStart matches "1. " at the beginning of a line, which is
// upstream's canonical entry delimiter for Step 1 summaries.
var numberedEntryStart = regexp.MustCompile(`(?m)^\s*(\d+)\.\s+`)

// splitMarkdownIntoIssueItems slices the LLM output into one IssueItem
// per "1. ", "2. ", ... entry. The first bolded fragment on each entry
// becomes the IssueItem.Title; the entire chunk becomes BodyMD.
//
// rawItems is the section's candidate items — used to populate the
// SourceURLsJSON / RawItemIDsJSON columns. We attach every candidate
// id/url to every produced IssueItem rather than trying to guess which
// ids the LLM quoted, because:
//
//  1. The LLM prompt allows mixing multiple candidates per entry, and
//  2. Downstream publishers only use these fields as evidence anchors,
//     not as a strict 1:1 join.
//
// This conservative mapping can be tightened in a later revision by
// parsing the (briefing) anchors out of the body markdown.
func splitMarkdownIntoIssueItems(md string, sectionID string, issueID int64, rawItems []*store.RawItem) []*store.IssueItem {
	// Pre-compute the source JSON blobs once per section; every
	// IssueItem in this section references the same candidate set.
	srcURLs := make([]string, 0, len(rawItems))
	srcIDs := make([]int64, 0, len(rawItems))
	for _, it := range rawItems {
		if it == nil {
			continue
		}
		if it.URL != "" {
			srcURLs = append(srcURLs, it.URL)
		}
		srcIDs = append(srcIDs, it.ID)
	}
	urlsJSON, _ := json.Marshal(srcURLs)
	idsJSON, _ := json.Marshal(srcIDs)

	// Find all "N. " offsets. If none, treat the whole blob as a single
	// anonymous entry (this is the degraded case where the LLM returned
	// prose instead of a list).
	starts := numberedEntryStart.FindAllStringIndex(md, -1)
	if len(starts) == 0 {
		return []*store.IssueItem{
			{
				IssueID:        issueID,
				Section:        sectionID,
				Seq:            1,
				Title:          extractTitle(md),
				BodyMD:         md,
				SourceURLsJSON: string(urlsJSON),
				RawItemIDsJSON: string(idsJSON),
			},
		}
	}

	items := make([]*store.IssueItem, 0, len(starts))
	for i, loc := range starts {
		begin := loc[0]
		var end int
		if i+1 < len(starts) {
			end = starts[i+1][0]
		} else {
			end = len(md)
		}
		chunk := strings.TrimSpace(md[begin:end])
		if chunk == "" {
			continue
		}
		title := extractTitle(chunk)
		items = append(items, &store.IssueItem{
			IssueID:        issueID,
			Section:        sectionID,
			Seq:            i + 1,
			Title:          title,
			BodyMD:         chunk,
			SourceURLsJSON: string(urlsJSON),
			RawItemIDsJSON: string(idsJSON),
		})
	}
	return items
}

// titleBoldRegex pulls the first **bolded** fragment out of an entry.
// Upstream's Step 1 prompt mandates "1. **title.** body" so this is a
// reliable extraction point.
var titleBoldRegex = regexp.MustCompile(`\*\*([^*]+?)\*\*`)

// titleFirstLineTrim strips leading "N. " and surrounding whitespace.
var titleFirstLineTrim = regexp.MustCompile(`^\s*\d+\.\s*`)

// extractTitle finds the entry's headline. Preference order:
//
//  1. First **bolded** fragment — the upstream canonical format.
//  2. The first non-empty line with its leading number stripped.
//  3. Empty string (caller should substitute a placeholder).
func extractTitle(chunk string) string {
	if m := titleBoldRegex.FindStringSubmatch(chunk); len(m) >= 2 {
		t := strings.TrimSpace(m[1])
		t = strings.TrimRight(t, "。.!?!?")
		if t != "" {
			return t
		}
	}

	for _, line := range strings.Split(chunk, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		trimmed = titleFirstLineTrim.ReplaceAllString(trimmed, "")
		trimmed = strings.Trim(trimmed, "*_` 　")
		if trimmed != "" {
			// Cap absurdly long lines so the column is usable.
			if rs := []rune(trimmed); len(rs) > 120 {
				trimmed = string(rs[:120])
			}
			return trimmed
		}
	}
	return ""
}
