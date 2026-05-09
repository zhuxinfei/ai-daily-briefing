// signal_strength.go — v1.0.1 Phase 4.2.
//
// 目的: 给每个 RawItem 计算一个 "signal_strength" 分数, 表示有多少个不同
// 来源(按 URL host 区分)在报道同一件事. 输出用于 rank 阶段加权, 让跨源
// 共振的大新闻更容易进入 top 30.
//
// 算法:
//  1. 提取每条 item 的 "标题关键词" (英文 4+ 大写词 + 中文 3+ 字片段).
//  2. 两两比较 Jaccard 相似度, >= 0.5 视为同一件事.
//  3. 并查集合并, 每组内统计 distinct URL host 数 → SignalStrength.
//
// 复杂度 O(N²). N ≈ 100-300, 单次 run <50ms, 无需优化.
//
// 注意:
//   - 只依赖 item.Title + item.URL, 不读 Content (避免正文干扰).
//   - SignalStrength 是 per-item 写回内存字段, 不持久化.
//   - 短标题 (<2 关键词) 保守给 1, 不参与合并, 避免误伤.
package ingest

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"briefing-v3/internal/store"
)

// signalTitleKeywordRe 抽取新闻标题里有区分度的"实体":
//   - [A-Z][A-Za-z]{3,}: 首字母大写的 4+ 字母词 (OpenAI / Anthropic / DeepMind)
//   - [A-Z]{2,}[-A-Za-z0-9]*: 全大写缩写 / 带版本号 (GPT-6 / GPT-4o / LLMs / API)
//   - [\p{Han}]{3,}: 中文 3+ 字连续片段
//
// 与 cmd/briefing/run.go:titleKeywordRe 的区别: 那里只用英文大写词 + 中文,
// 用于"与历史已推送去重" (阈值 0.6, 偏严); 这里是同一天多源共振合并 (阈值
// 0.5), 加上缩写识别让 GPT-6 / LLM / Claude-3.5 这类高频实体也能进桶.
var signalTitleKeywordRe = regexp.MustCompile(`[A-Z][A-Za-z]{3,}|[A-Z]{2,}[-A-Za-z0-9]*|[\p{Han}]{3,}`)

// extractSignalKeywords 返回 title 去重小写后的关键词 slice.
func extractSignalKeywords(title string) []string {
	matches := signalTitleKeywordRe.FindAllString(title, -1)
	seen := make(map[string]bool, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		k := strings.ToLower(m)
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// jaccardSimilarity 返回两个关键词集合的 Jaccard 系数, 0..1.
// 空集合返回 0 (避免 div-by-zero 也避免被误判为"完全一样").
func jaccardSimilarity(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	setA := make(map[string]bool, len(a))
	for _, k := range a {
		setA[k] = true
	}
	inter := 0
	union := len(setA)
	for _, k := range b {
		if setA[k] {
			inter++
		} else {
			union++
		}
	}
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// extractHost 从 URL 里抽 host. URL 为空或解析失败时返回 sourceKey, 避免
// 同一来源的两篇相似标题被误算成 2 个 distinct host.
func extractHost(rawURL string, sourceID int64) string {
	if rawURL == "" {
		return fallbackHost(sourceID)
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return fallbackHost(sourceID)
	}
	host := strings.ToLower(u.Host)
	// 去 www. 前缀, 防止 www.x.com 与 x.com 被算作两个 host.
	host = strings.TrimPrefix(host, "www.")
	return host
}

func fallbackHost(sourceID int64) string {
	// 保守: 给个唯一但可识别的 fallback, 防止多个空 URL 被合并成 host="".
	return "source#" + strconv.FormatInt(sourceID, 10)
}

// signalJaccardThreshold: 两条标题关键词 Jaccard >= 此阈值 → 同一件事.
// 0.5 是经验值 (titleOverlap 历史阈值 0.6 是"与历史已推送"去重用的,
// 更严; 这里是同一天内不同源之间合并, 放松到 0.5 能抓到大部分共振).
const signalJaccardThreshold = 0.5

// minKeywordsForGrouping: 少于此数量关键词的标题跳过合并 (signal=1),
// 避免短标题误合并 (例如两条都只有一个 "OpenAI" 关键词就被当一组).
const minKeywordsForGrouping = 2

// CalculateSignalStrength 遍历所有 items, 按标题相似度分组, 每组计算
// distinct host 数, 写回每个 item 的 SignalStrength 字段.
//
// 返回 distribution map: signal_strength → 有多少 items 是该值, 供调用
// 方日志用. 零 input / 全 nil 时返回空 map, 不报错.
func CalculateSignalStrength(items []*store.RawItem) map[int]int {
	dist := map[int]int{}
	if len(items) == 0 {
		return dist
	}

	// 预处理: 每个 item 抽关键词 + host. nil item 过滤掉但保留 index.
	n := len(items)
	kws := make([][]string, n)
	hosts := make([]string, n)
	validIdx := make([]int, 0, n)
	for i, it := range items {
		if it == nil {
			continue
		}
		kws[i] = extractSignalKeywords(it.Title)
		hosts[i] = extractHost(it.URL, it.SourceID)
		validIdx = append(validIdx, i)
	}

	// 并查集: parent[i] = i 初始.
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(x, y int) {
		px, py := find(x), find(y)
		if px != py {
			parent[px] = py
		}
	}

	// O(N²) 两两比较. 只合并关键词数 >= minKeywordsForGrouping 的条目.
	for ai := 0; ai < len(validIdx); ai++ {
		i := validIdx[ai]
		if len(kws[i]) < minKeywordsForGrouping {
			continue
		}
		for bi := ai + 1; bi < len(validIdx); bi++ {
			j := validIdx[bi]
			if len(kws[j]) < minKeywordsForGrouping {
				continue
			}
			if jaccardSimilarity(kws[i], kws[j]) >= signalJaccardThreshold {
				union(i, j)
			}
		}
	}

	// 每组统计 distinct host 数.
	groupHosts := make(map[int]map[string]bool)
	for _, i := range validIdx {
		root := find(i)
		if groupHosts[root] == nil {
			groupHosts[root] = map[string]bool{}
		}
		groupHosts[root][hosts[i]] = true
	}

	// 写回每个 item 的 SignalStrength.
	for _, i := range validIdx {
		it := items[i]
		root := find(i)
		ss := len(groupHosts[root])
		if ss < 1 {
			ss = 1
		}
		it.SignalStrength = ss
		dist[ss]++
	}

	return dist
}

// ---- Cross-mention count (v1.0.1 Phase 4.6) --------------------------

// ossinsightSourceType is the adapter type that represents GitHub trending
// repos. Items from this source get a CrossMentionCount computed below.
const ossinsightSourceType = "ossinsight"

// commonRepoWords are repo name tokens too generic to match on — matching
// them in other sources' content would produce too many false positives.
// Skip a repo's short-name pass if the short name equals any of these.
var commonRepoWords = map[string]bool{
	"agent": true, "agents": true, "ai": true, "ml": true,
	"bench": true, "benchmark": true, "demo": true, "sample": true,
	"example": true, "tool": true, "tools": true, "lib": true,
	"library": true, "code": true, "codes": true, "api": true,
	"docs": true, "doc": true, "test": true, "tests": true,
	"app": true, "cli": true, "ui": true, "core": true,
}

// repoShortNameRe extracts alphanumeric (with dashes/underscores) tokens
// from a full repo name like "NousResearch/hermes-agent" → "hermes-agent".
var repoShortNameRe = regexp.MustCompile(`[A-Za-z][A-Za-z0-9\-_]{3,}`)

// extractRepoMatchTerms returns a list of strings to search for when
// counting cross-source mentions of this repo.
//
// Strategy:
//   - Always include the full "owner/repo" (most specific, lowest FP rate)
//   - Include the repo short-name if: length >= 5, and not in commonRepoWords
//   - Drop terms shorter than 5 chars or too common
func extractRepoMatchTerms(title string) []string {
	t := strings.TrimSpace(title)
	if t == "" {
		return nil
	}
	out := []string{t} // full "owner/repo" (最精确, 几乎无 FP)
	short := ""
	if idx := strings.Index(t, "/"); idx > 0 && idx < len(t)-1 {
		short = strings.TrimSpace(t[idx+1:])
	}
	// 只加 dashed short name (e.g. "hermes-agent"), 不再加 brand 单词
	// (v1.0.1 Phase 4.6 修正: 之前加"hermes"作 brand 会让 hermes-webui
	// 等同名小项目借用原 Hermes 的讨论热度, 造成"同名项目蹭热度"假信号.
	// 短名精确度足够区分, brand 单词风险高于收益).
	if short != "" && len(short) >= 5 && !commonRepoWords[strings.ToLower(short)] {
		out = append(out, short)
	}
	return out
}

// CalculateCrossMentions 给每个 ossinsight (GitHub trending) item 计算
// CrossMentionCount: 这个 repo 名在非 ossinsight 源的 title+content 中被
// 提到的次数 (大小写不敏感, word-boundary).
//
// 用意: 让 rank 阶段能综合 (a) star 增长 (b) 圈内讨论热度 两个信号.
// Hermes / VibeVoice 这类"GitHub 新星 + 科技媒体热议"的 repo 分数上浮.
//
// 性能: 合并 non-ossinsight items 文本成一个大 haystack, 每个 repo 只
// 搜索一次 → 100 次 Contains, 毫秒级.
//
// In-memory only, 直接写回 items[i].CrossMentionCount.
func CalculateCrossMentions(items []*store.RawItem, sourceTypes map[int64]string) {
	if len(items) == 0 || len(sourceTypes) == 0 {
		return
	}
	// Build one big haystack from all non-ossinsight items.
	var haystack strings.Builder
	for _, it := range items {
		if it == nil {
			continue
		}
		if sourceTypes[it.SourceID] == ossinsightSourceType {
			continue // skip ossinsight's own items
		}
		haystack.WriteString(strings.ToLower(it.Title))
		haystack.WriteByte('\n')
		haystack.WriteString(strings.ToLower(it.Content))
		haystack.WriteByte('\n')
	}
	hay := haystack.String()
	if hay == "" {
		return
	}

	// For each ossinsight item, count matches.
	for _, it := range items {
		if it == nil || sourceTypes[it.SourceID] != ossinsightSourceType {
			continue
		}
		terms := extractRepoMatchTerms(it.Title)
		if len(terms) == 0 {
			continue
		}
		maxCount := 0
		for _, term := range terms {
			n := countMentions(hay, strings.ToLower(term))
			if n > maxCount {
				maxCount = n
			}
		}
		it.CrossMentionCount = maxCount
	}
}

// countMentions returns the number of substring occurrences of needle in
// haystack. needle must already be lowercased. Uses word-boundary-ish
// logic: surrounding chars must not be letters/digits (so "agent" won't
// match inside "agents" and "codex" won't match inside "codexing").
func countMentions(haystack, needle string) int {
	if needle == "" || len(needle) > len(haystack) {
		return 0
	}
	n := 0
	start := 0
	for {
		idx := strings.Index(haystack[start:], needle)
		if idx < 0 {
			break
		}
		abs := start + idx
		// Check boundaries.
		leftOK := abs == 0 || !isWordChar(haystack[abs-1])
		rightIdx := abs + len(needle)
		rightOK := rightIdx == len(haystack) || !isWordChar(haystack[rightIdx])
		if leftOK && rightOK {
			n++
		}
		start = abs + 1
	}
	return n
}

func isWordChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}
