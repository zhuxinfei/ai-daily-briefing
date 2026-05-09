// Package rank implements the Step 1 LLM quality scoring pass for
// briefing-v3. It takes 80-200 RawItems ingested from all sources and uses
// an OpenAI-compatible LLM to assign each item a 0-10 quality score plus a
// short reason. Items below MinScore are dropped and only the top TopN
// items (by score descending) are returned downstream to classify/compose.
//
// This replaces the upstream "human editor manually picks items" step with
// a deterministic LLM gate. Its output quality directly decides whether
// the final issue can match ai.hubtoday.app.
package rank

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"briefing-v3/internal/store"
)

// Config parameterizes the LLM ranker. BaseURL, APIKey and Model are
// required; the other fields have sane defaults filled in by fillDefaults.
type Config struct {
	BaseURL    string
	APIKey     string
	Model      string
	BatchSize  int           // items per LLM request, default 20
	MinScore   float64       // drop items with score < MinScore, default 6.0
	TopN       int           // return at most TopN items, default 30
	MaxRetries int           // per-batch retries, default 3
	Timeout    time.Duration // per-request timeout, default 120s
	// RetryBackoffSeconds is the backoff sequence for per-batch retries.
	// Each element is the number of seconds to sleep before the next attempt.
	// The length of this slice determines MaxRetries (overrides MaxRetries
	// when non-empty). Default: [10, 30, 90].
	RetryBackoffSeconds []int
	// PerCategoryQuota caps how many top-scoring items the ranker will
	// return from each source category (news / blog / paper / project /
	// community). When set, rank does two passes:
	//
	//   1. group items by source category (using SourceCategories) and
	//      keep at most PerCategoryQuota[category] items per group,
	//   2. merge the groups, sort by score descending, apply TopN.
	//
	// This prevents a single-category source explosion (e.g. arxiv
	// dumping 20+ papers into the top 30) from starving other sections
	// downstream. If PerCategoryQuota is nil or empty, rank falls back
	// to pure global top-N behaviour (v0 default).
	PerCategoryQuota map[string]int
}

func (c *Config) fillDefaults() {
	if c.BatchSize <= 0 {
		c.BatchSize = 20
	}
	if c.MinScore == 0 {
		c.MinScore = 6.0
	}
	if c.TopN <= 0 {
		c.TopN = 30
	}
	if c.MaxRetries <= 0 {
		c.MaxRetries = 3
	}
	if c.Timeout <= 0 {
		c.Timeout = 120 * time.Second
	}
	if len(c.RetryBackoffSeconds) == 0 {
		c.RetryBackoffSeconds = []int{10, 30, 90}
	}
}

// RankedItem carries the original RawItem plus the LLM's verdict.
//
// v1.0.1 Phase 4.1: WeightedScore = Score × sourcePriorityWeight.
// v1.0.1 Phase 4.2: WeightedScore also multiplied by signalBoost, where
// signalBoost = 1 + 0.2*(signalStrength-1), capped at 2.0 (ss >= 6).
//
// Full formula (used as sort key):
//
//	WeightedScore = Score × priorityWeight × signalBoost
//
// With:
//
//	priorityWeight = 0.5 + priority/10.0   (priority 0-10)
//	signalBoost    = min(1 + 0.2*(ss-1), 2.0)   (ss = distinct sources)
//
// Score (raw LLM 0-10) and SignalStrength are preserved for debug logging.
type RankedItem struct {
	Item           *store.RawItem
	Score          float64 // raw LLM 0-10
	WeightedScore  float64 // Score × priorityWeight × signalBoost (used for sort)
	SignalStrength int     // v1.0.1 Phase 4.2: distinct source hosts on same story
	Reason         string
}

// Ranker is the public interface: score a batch of RawItems and return a
// ranked-and-filtered subset.
//
// v1.0.0: Rank accepts a sourceCategories lookup so it can enforce
// the per-category quota configured via Config.PerCategoryQuota. The
// map is sourceID → category ("news"/"blog"/"paper"/"project"/"community").
// Items whose source is absent from the map are treated as an "unknown"
// category that is not subject to any quota.
//
// v1.0.1 Phase 4.1: sourcePriorities (sourceID → priority 0-10) is also
// accepted for weighted scoring. Nil / empty map = no weighting (all
// items treated as priority=5, neutral weight 1.0). Missing source in
// an otherwise-populated map → priority=0 (降权, signals config drift).
type Ranker interface {
	Rank(
		ctx context.Context,
		items []*store.RawItem,
		sourceCategories map[int64]string,
		sourcePriorities map[int64]int,
	) ([]*RankedItem, error)
}

// New constructs a Ranker backed by an OpenAI-compatible chat/completions
// endpoint. Returns an error if required fields are missing.
func New(cfg Config) (Ranker, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("rank: Config.BaseURL is required")
	}
	if cfg.APIKey == "" {
		return nil, errors.New("rank: Config.APIKey is required")
	}
	if cfg.Model == "" {
		return nil, errors.New("rank: Config.Model is required")
	}
	cfg.fillDefaults()
	return &llmRanker{
		cfg: cfg,
		hc:  &http.Client{},
	}, nil
}

// rankSystemPrompt is the rubric the LLM uses to assign 0-10 scores.
// Tuned to reward top AI lab releases and penalize low-value noise.
const rankSystemPrompt = `你是 AI 日报运营编辑。你的任务是对今天采集到的候选条目打分，筛选出最值得进入日报的 top 30 条。

评分标准 (0-10):
- 10: 顶级 AI 公司 (OpenAI/Anthropic/Google/Meta/NVIDIA/DeepSeek/xAI/Microsoft) 的重大发布/更新
- 9: 热门开源项目 (>5k star) 的重大进展 / 重要学术突破 / 重量级行业事件
- 8: 知名 AI 工具的重要更新 / 重要学术论文 / 重量级分析评论
- 7: AI 行业政策/商业/伦理事件 / 重要社区讨论 / 个人 AI 大 V 深度博客
- 6: 次要但相关的 AI 新闻
- 5 以下: 低价值 / 重复 / 广告 / 非 AI 相关 / 噪音

注意:
- 纯 arxiv 论文默认 7 分，除非标题明显突破性 (如 "state-of-the-art", "breakthrough")
- Reddit/HN 讨论默认 6-7 分，除非评论数或 score 特别高
- 重复话题只给最高分的那条高分，其他降级

opensource/GitHub trending 专项 (v1.0.1 Phase 4.6):
- 某些 item 后会附带热度信号, 如 "stars_total=41890 stars+=10051 trending#1 mentions=8". 四个维度:
  * stars_total 是 GitHub 上该 repo 的总 star 数 (绝对热度, 最硬的信号)
  * stars+= 是本周涨幅 (近期热度, 说明正在上升)
  * trending# 是 ossinsight 排名 (越小越靠前)
  * mentions= 是该 repo 在非 ossinsight 源 (科技媒体/博客/论坛) 被提及次数
- **硬门槛 (v1.0.1 Phase 4.6)**: stars_total 是主导信号:
  * stars_total < 1000 → 一律给 6 分以下 (不管 description 多亮眼), 不入 opensource top.
    "业务价值"不能替代"实际热度" — 冷门小项目哪怕概念好, 在日报上也不合适
  * stars_total 1000-5000 → 7-8 分, 看 stars+ / description / mentions 综合决定
  * stars_total >= 5000 且 (stars+ >= 500 或 mentions >= 3) → 9-10 分必选
    (已成名项目 + 近期仍在涨 或 圈内讨论)
- 如果 stars_total 缺失 (GitHub API 拿不到), 退回用 stars+ 判断: stars+ < 500 一律降级
- mentions 只是辅助信号, 不能单独抬高分数 (避免同名小项目借热度信号上位)
- 目标: 已成名且近期活跃的项目入选 (Hermes / VibeVoice 类), 避免 Libretto
  (309 stars_total, description 亮眼) 这种冷门项目占位

输出严格 JSON 数组 (无其他文字):
[{"id": 原 id (int), "score": 0-10, "reason": "20 字内理由"}, ...]`

// rankUserPromptTemplate formats one batch's worth of RawItems into the
// user message. %s is the joined item lines.
const rankUserPromptTemplate = `以下是今日候选条目，请按评分标准给每一条打分：

%s

只输出 JSON 数组。`

// llmRanker is the concrete Ranker implementation.
type llmRanker struct {
	cfg Config
	hc  *http.Client
}

// chatMessage / chatRequest / chatResponse mirror the structs in
// internal/generate/openai.go — duplicated here to keep rank decoupled
// from the generate package.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
		Index   int         `json:"index"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// httpStatusError wraps an HTTP status code so callers can distinguish
// permanent errors (401/403) from transient ones (429/502/503).
type httpStatusError struct {
	Code int
	Err  error
}

func (e *httpStatusError) Error() string { return e.Err.Error() }
func (e *httpStatusError) Unwrap() error { return e.Err }

// isPermanentHTTPError returns true for HTTP status codes that indicate
// the request will never succeed (bad credentials, forbidden), so retrying
// is pointless.
func isPermanentHTTPError(err error) bool {
	var he *httpStatusError
	if errors.As(err, &he) {
		return he.Code == 401 || he.Code == 403
	}
	return false
}

// rankVerdict matches one element of the LLM-emitted JSON array.
type rankVerdict struct {
	ID     int64   `json:"id"`
	Score  float64 `json:"score"`
	Reason string  `json:"reason"`
}

// Rank splits items into batches, scores each batch via LLM, merges the
// verdicts, optionally applies a per-category quota, then sorts by score
// desc and returns the top N items above MinScore. Items for which no
// verdict is returned are silently dropped (they are treated as "not
// interesting enough to be scored").
func (r *llmRanker) Rank(
	ctx context.Context,
	items []*store.RawItem,
	sourceCategories map[int64]string,
	sourcePriorities map[int64]int,
) ([]*RankedItem, error) {
	if len(items) == 0 {
		return nil, nil
	}

	// Index by ID so we can look items up after LLM responds.
	byID := make(map[int64]*store.RawItem, len(items))
	for _, it := range items {
		if it == nil {
			continue
		}
		byID[it.ID] = it
	}

	// Batch items to stay under token limits.
	var allVerdicts []rankVerdict
	totalBatches := (len(items) + r.cfg.BatchSize - 1) / r.cfg.BatchSize
	successCount := 0
	var batchErrors []string
	for start := 0; start < len(items); start += r.cfg.BatchSize {
		end := start + r.cfg.BatchSize
		if end > len(items) {
			end = len(items)
		}
		batch := items[start:end]
		batchNum := start/r.cfg.BatchSize + 1

		verdicts, err := r.rankBatchWithRetry(ctx, batch)
		if err != nil {
			log.Printf("[WARN] rank: batch %d/%d failed: %v", batchNum, totalBatches, err)
			batchErrors = append(batchErrors, fmt.Sprintf("batch %d: %v", batchNum, err))
			continue
		}
		successCount++
		allVerdicts = append(allVerdicts, verdicts...)
	}

	log.Printf("[rank] %d/%d batches succeeded", successCount, totalBatches)

	if len(allVerdicts) == 0 {
		return nil, fmt.Errorf("rank: all %d batches failed: %s", totalBatches, strings.Join(batchErrors, "; "))
	}

	// Merge verdicts with RawItems. If multiple verdicts reference the
	// same ID, keep the highest score.
	best := make(map[int64]rankVerdict)
	for _, v := range allVerdicts {
		if _, ok := byID[v.ID]; !ok {
			continue
		}
		if prev, ok := best[v.ID]; !ok || v.Score > prev.Score {
			best[v.ID] = v
		}
	}

	ranked := make([]*RankedItem, 0, len(best))
	for id, v := range best {
		if v.Score < r.cfg.MinScore {
			continue
		}
		// v1.0.1 Phase 4.1: priority weight; Phase 4.2: signal-strength boost.
		item := byID[id]
		pw := priorityWeight(sourcePriorities, item.SourceID)
		sb := signalBoost(item.SignalStrength)
		ranked = append(ranked, &RankedItem{
			Item:           item,
			Score:          v.Score,
			WeightedScore:  v.Score * pw * sb,
			SignalStrength: item.SignalStrength,
			Reason:         strings.TrimSpace(v.Reason),
		})
	}

	// Sort by WeightedScore desc (v1.0.1 Phase 4.1), tie-break by raw Score
	// then by RawItem ID for determinism.
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].WeightedScore != ranked[j].WeightedScore {
			return ranked[i].WeightedScore > ranked[j].WeightedScore
		}
		if ranked[i].Score != ranked[j].Score {
			return ranked[i].Score > ranked[j].Score
		}
		return ranked[i].Item.ID < ranked[j].Item.ID
	})

	// Per-category quota: group by source category, keep at most N per
	// group, then merge. This caps pathological single-category
	// explosions (e.g. arxiv dumping 20+ papers into top 30 and starving
	// news / opensource / social sections).
	if len(r.cfg.PerCategoryQuota) > 0 {
		ranked = applyPerCategoryQuota(ranked, sourceCategories, r.cfg.PerCategoryQuota)
	}

	if len(ranked) > r.cfg.TopN {
		ranked = ranked[:r.cfg.TopN]
	}
	return ranked, nil
}

// priorityWeight maps (sourceID → priority 0-10) to a multiplier on the
// raw LLM score. v1.0.1 Phase 4.1.
//
// Formula: weight = 0.5 + priority/10.0
//
//	priority = 10 → weight 1.5 (权威源, 如 DeepMind / Google AI)
//	priority =  5 → weight 1.0 (中性基线)
//	priority =  0 → weight 0.5 (未设, 降权)
//
// Nil map or missing sourceID: treated as priority=5 (neutral) — backward
// compat when sourcePriorities was not yet plumbed through. An explicitly
// populated map whose sourceID entry is 0 / missing drops to 0.5 (signals
// config drift: source row without priority field).
func priorityWeight(sourcePriorities map[int64]int, sourceID int64) float64 {
	if sourcePriorities == nil {
		return 1.0 // backward compat: no priority info, neutral weight
	}
	p, ok := sourcePriorities[sourceID]
	if !ok {
		// Map populated but source absent: treat as unknown → neutral.
		return 1.0
	}
	if p < 0 {
		p = 0
	}
	if p > 10 {
		p = 10
	}
	return 0.5 + float64(p)/10.0
}

// signalBoost maps signal_strength (distinct source hosts on same story) to
// a multiplier. v1.0.1 Phase 4.2.
//
// Formula: boost = min(1 + 0.2*(ss-1), 2.0)
//
//	ss = 0 or 1 → 1.0 (no boost, 单源)
//	ss = 2      → 1.2 (两个源共振)
//	ss = 3      → 1.4
//	ss = 5      → 1.8
//	ss ≥ 6      → 2.0 (cap, 防止被少数热点吞掉长尾位置)
//
// cap=2.0 的原因: 单纯多源报道不足以让一条 7 分的内容压过另一条 10 分权威
// 内容 (7 × 1.5 × 2.0 = 21.0 vs 10 × 1.5 × 1.0 = 15.0 — 即使这样热点还是
// 赢, 但不是无限赢). 如果发现共振信号还不够强, 再上调系数.
func signalBoost(signalStrength int) float64 {
	if signalStrength <= 1 {
		return 1.0
	}
	b := 1.0 + 0.2*float64(signalStrength-1)
	if b > 2.0 {
		b = 2.0
	}
	return b
}

// applyPerCategoryQuota walks a score-sorted ranked slice, groups items
// by their source category, truncates each group to the configured
// quota and returns the merged slice in score-desc order.
//
// Unknown categories (source not in the map, or category not in the
// quota map) are collected into a pseudo "_unknown" bucket that is NOT
// capped — this lets edge-case sources (e.g. a new source added to the
// DB but missing from config) still contribute items instead of being
// silently dropped.
func applyPerCategoryQuota(
	ranked []*RankedItem,
	sourceCategories map[int64]string,
	quota map[string]int,
) []*RankedItem {
	if len(ranked) == 0 {
		return ranked
	}

	// Input is already sorted by score desc, tie-break by ID asc. Walk
	// it once, track per-category running count, drop items that exceed
	// their category's quota.
	counts := make(map[string]int, len(quota)+1)
	out := make([]*RankedItem, 0, len(ranked))
	for _, ri := range ranked {
		if ri == nil || ri.Item == nil {
			continue
		}
		cat := ""
		if sourceCategories != nil {
			cat = strings.ToLower(strings.TrimSpace(sourceCategories[ri.Item.SourceID]))
		}
		if cat == "" {
			// Unknown category: let it through untouched.
			out = append(out, ri)
			continue
		}
		cap, known := quota[cat]
		if !known || cap <= 0 {
			// Category not in quota map: also let it through.
			out = append(out, ri)
			continue
		}
		if counts[cat] >= cap {
			continue
		}
		counts[cat]++
		out = append(out, ri)
	}
	// Output order is already score-desc because we iterated the sorted
	// input in order.
	return out
}

// rankBatchWithRetry attempts LLM calls for a single batch with
// exponential backoff driven by Config.RetryBackoffSeconds.
//
// Error handling:
//   - Permanent HTTP errors (401/403) -> return immediately, no retry.
//   - Transient errors (429/502/503/timeout/parse) -> backoff and retry.
//   - Context cancellation -> return ctx.Err() immediately.
func (r *llmRanker) rankBatchWithRetry(ctx context.Context, batch []*store.RawItem) ([]rankVerdict, error) {
	userPrompt := fmt.Sprintf(rankUserPromptTemplate, formatItemsForRank(batch))

	backoffs := r.cfg.RetryBackoffSeconds
	maxAttempts := len(backoffs) + 1 // first attempt + len(backoffs) retries

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		raw, err := r.chatComplete(ctx, rankSystemPrompt, userPrompt)
		if err != nil {
			lastErr = err
			// Permanent errors: don't retry.
			if isPermanentHTTPError(err) {
				return nil, fmt.Errorf("rank: permanent error (not retrying): %w", err)
			}
			// Context cancelled: bail out.
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// Transient error: backoff and retry if attempts remain.
			if err := sleepBackoff(ctx, attempt, maxAttempts, backoffs, "failed", err); err != nil {
				return nil, err
			}
			continue
		}
		verdicts, perr := parseRankJSON(raw)
		if perr != nil {
			lastErr = perr
			if err := sleepBackoff(ctx, attempt, maxAttempts, backoffs, "parse failed", perr); err != nil {
				return nil, err
			}
			continue
		}
		return verdicts, nil
	}
	if lastErr == nil {
		lastErr = errors.New("rank: batch failed with no specific error")
	}
	return nil, lastErr
}

// sleepBackoff handles the common retry-wait pattern: if attempts remain,
// log a warning and sleep for the configured backoff duration (respecting
// context cancellation). Returns nil to continue the retry loop, or a
// non-nil error to abort (context cancelled).
func sleepBackoff(ctx context.Context, attempt, maxAttempts int, backoffs []int, label string, cause error) error {
	if attempt >= maxAttempts {
		return nil // no more retries, let caller handle lastErr
	}
	backoff := time.Duration(backoffs[attempt-1]) * time.Second
	log.Printf("[rank] attempt %d/%d %s: %v; retrying in %v", attempt, maxAttempts, label, cause, backoff)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(backoff):
		return nil
	}
}

// formatItemsForRank renders a batch into the bullet-list that the
// rankUserPromptTemplate expects. Each line is
// "[id=N] title | source_id | URL | first-80-chars-of-content".
//
// v1.0.1 Phase 4.6: opensource (GitHub trending) items 额外附带热度信号:
//   - stars 增长数 (来自 metadata_json.stars)
//   - trending 排名 (来自 metadata_json.rank)
//   - 跨源讨论次数 (CrossMentionCount, 圈内讨论热度)
// rank LLM 看到这些硬数字后能更准地判断"该入选哪些 repo"
// (Hermes 类 trending + 广受讨论的项目不会被描述普通的 repo 挤掉).
func formatItemsForRank(batch []*store.RawItem) string {
	var b strings.Builder
	for _, it := range batch {
		if it == nil {
			continue
		}
		desc := firstRunes(strings.TrimSpace(it.Content), 80)
		if desc == "" {
			desc = "(no description)"
		}
		source := fmt.Sprintf("source#%d", it.SourceID)
		extra := formatRepoHotnessExtra(it) // 仅对 opensource 有内容, 其他返回空
		fmt.Fprintf(&b, "[id=%d] %s | %s | %s | %s%s\n",
			it.ID,
			truncateOneLine(it.Title, 140),
			source,
			it.URL,
			truncateOneLine(desc, 160),
			extra,
		)
	}
	return b.String()
}

// formatRepoHotnessExtra 给 opensource repo item 附加热度信号字符串.
// 非 opensource item 返回空字符串, 不影响原格式.
// 格式: " | stars+=10051 trending#1 mentions=8" (每项缺失则省略)
func formatRepoHotnessExtra(it *store.RawItem) string {
	if it == nil || it.MetadataJSON == "" {
		// 即使无 metadata, 只要 CrossMentionCount>0 也附上
		if it != nil && it.CrossMentionCount > 0 {
			return fmt.Sprintf(" | mentions=%d", it.CrossMentionCount)
		}
		return ""
	}
	// 只对 ossinsight metadata 提取 stars/stars_total/rank (其他 adapter 的
	// metadata 格式不同, 静默跳过).
	var meta struct {
		Stars      int `json:"stars"`       // period 内涨幅
		StarsTotal int `json:"stars_total"` // 总 star 数 (v1.0.1 Phase 4.6)
		Rank       int `json:"rank"`
	}
	_ = json.Unmarshal([]byte(it.MetadataJSON), &meta)
	parts := []string{}
	if meta.StarsTotal > 0 {
		parts = append(parts, fmt.Sprintf("stars_total=%d", meta.StarsTotal))
	}
	if meta.Stars > 0 {
		parts = append(parts, fmt.Sprintf("stars+=%d", meta.Stars))
	}
	if meta.Rank > 0 {
		parts = append(parts, fmt.Sprintf("trending#%d", meta.Rank))
	}
	if it.CrossMentionCount > 0 {
		parts = append(parts, fmt.Sprintf("mentions=%d", it.CrossMentionCount))
	}
	if len(parts) == 0 {
		return ""
	}
	return " | " + strings.Join(parts, " ")
}

// firstRunes returns the first n runes of s (not bytes) to avoid slicing
// UTF-8 mid-codepoint.
func firstRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}

// truncateOneLine collapses newlines in s and truncates to n runes. Used
// to keep per-item lines in the prompt readable by the LLM.
func truncateOneLine(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.TrimSpace(s)
	return firstRunes(s, n)
}

// parseRankJSON extracts the first JSON array from the LLM response and
// unmarshals it into []rankVerdict. It tolerates leading/trailing prose
// because some models wrap output in fenced code blocks despite our
// "only output JSON" instruction.
func parseRankJSON(raw string) ([]rankVerdict, error) {
	s := extractJSONArray(raw)
	if s == "" {
		return nil, fmt.Errorf("rank: no JSON array found in LLM output: %q", truncateOneLine(raw, 200))
	}
	var verdicts []rankVerdict
	if err := json.Unmarshal([]byte(s), &verdicts); err != nil {
		return nil, fmt.Errorf("rank: parse JSON: %w (raw: %q)", err, truncateOneLine(s, 200))
	}
	return verdicts, nil
}

// extractJSONArray locates the first '[' ... matching ']' substring in s.
// Tracks quoting and escapes so brackets inside strings don't confuse us.
func extractJSONArray(s string) string {
	start := strings.Index(s, "[")
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if esc {
			esc = false
			continue
		}
		if c == '\\' && inStr {
			esc = true
			continue
		}
		if c == '"' {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		switch c {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// chatComplete is a trimmed clone of generate.openaiGenerator.chatComplete.
// It POSTs a single chat/completions request with a temperature of 0 (we
// want repeatable scores) and returns the assistant text.
func (r *llmRanker) chatComplete(parent context.Context, system, user string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, r.cfg.Timeout)
	defer cancel()

	reqBody := chatRequest{
		Model: r.cfg.Model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Temperature: 0,
		MaxTokens:   2000,
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("rank marshal: %w", err)
	}

	url := strings.TrimRight(r.cfg.BaseURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return "", fmt.Errorf("rank new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.cfg.APIKey)

	resp, err := r.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("rank http do: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("rank read body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(body)
		if len(snippet) > 500 {
			snippet = snippet[:500] + "..."
		}
		return "", &httpStatusError{
			Code: resp.StatusCode,
			Err:  fmt.Errorf("rank openai http %d: %s", resp.StatusCode, snippet),
		}
	}

	var cr chatResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return "", fmt.Errorf("rank unmarshal response: %w", err)
	}
	if cr.Error != nil && cr.Error.Message != "" {
		return "", fmt.Errorf("rank openai error: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return "", errors.New("rank openai: empty choices")
	}
	return cr.Choices[0].Message.Content, nil
}
