package gate

import (
	"regexp"
	"strings"

	"briefing-v3/internal/store"
)

// WeeklyReport 是周报 gate 的三态结果, 对齐日报 Report 的语义:
//
//   - Pass == true                    → green, 正常推 test + prod (如果 mode=prod).
//   - Pass == false && Warn == true   → yellow, 推 test 加 "质量待审" 告警.
//   - Pass == false && Warn == false  → red, hard fail, 只发 alert 到 test, 不推 prod.
//
// v1.0.1 Phase 4.5 (W6) 新增. 补齐周报缺失的质量门槛, 让周报推送机制和日报
// run.go:866-892 完全对齐.
type WeeklyReport struct {
	Pass            bool
	Warn            bool
	Reasons         []string // hard-fail 原因
	Warnings        []string // soft-warn 原因
	DailyIssueCount int      // 本周纳入分析的日报数
	FocusChars      int      // FocusMD 字符数 (strip 空白后)
	TrendsChars     int      // TrendsMD 字符数
	TakeawayChars   int      // TakeawaysMD 字符数
	BannedHits      []string // 命中 banned patterns 的词 (如 webhook/cron 之类内部实现细节)
}

// CheckWeekly 判断 WeeklyIssue 是否可推送. 规则:
//
// Hard fail (Pass=false, Warn=false):
//   - weekly == nil
//   - title 空
//   - FocusMD / TrendsMD / TakeawaysMD 三者任一纯空白 (LLM 残缺)
//   - dailyIssuesCount == 0 (没数据源)
//   - 命中 banned pattern (运维/调度/缓存等词, 说明 LLM 泄露实现细节)
//
// Soft warn (Pass=false, Warn=true):
//   - Focus 内容 < 200 字 (至少 2-3 个话题, 每条 60+)
//   - Takeaway 内容 < 100 字 (至少 2 条启发)
//   - dailyIssuesCount < 5 (一周不足 5 天数据)
//
// Pass (全部通过) → 内容扎实, 可以 prod.
func CheckWeekly(weekly *store.WeeklyIssue, dailyIssueCount int, bannedPatterns []*regexp.Regexp) *WeeklyReport {
	r := &WeeklyReport{DailyIssueCount: dailyIssueCount}

	if weekly == nil {
		r.Reasons = append(r.Reasons, "weekly is nil")
		return r
	}

	focus := strings.TrimSpace(weekly.FocusMD)
	trends := strings.TrimSpace(weekly.TrendsMD)
	takeaway := strings.TrimSpace(weekly.TakeawaysMD)
	r.FocusChars = len([]rune(focus))
	r.TrendsChars = len([]rune(trends))
	r.TakeawayChars = len([]rune(takeaway))

	// --- Hard fail 条件 ---
	if strings.TrimSpace(weekly.Title) == "" {
		r.Reasons = append(r.Reasons, "title 空")
	}
	if focus == "" {
		r.Reasons = append(r.Reasons, "FocusMD 空 (本周聚焦缺失)")
	}
	if trends == "" {
		r.Reasons = append(r.Reasons, "TrendsMD 空 (趋势分析缺失)")
	}
	if takeaway == "" {
		r.Reasons = append(r.Reasons, "TakeawaysMD 空 (启发缺失)")
	}
	if dailyIssueCount == 0 {
		r.Reasons = append(r.Reasons, "本周 0 份日报, 无数据源")
	}

	// banned patterns 检查 (运维/调度等实现细节词汇不能泄露到推送内容)
	if len(bannedPatterns) == 0 {
		bannedPatterns = DefaultBannedPatterns()
	}
	combinedContent := focus + "\n" + trends + "\n" + takeaway + "\n" + weekly.SignalsMD + "\n" + weekly.PonderMD
	for _, re := range bannedPatterns {
		if re.MatchString(combinedContent) {
			hit := re.String()
			r.BannedHits = append(r.BannedHits, hit)
			r.Reasons = append(r.Reasons, "内容命中 banned pattern: "+hit)
		}
	}

	if len(r.Reasons) > 0 {
		// hard fail, 不看 warn
		return r
	}

	// --- Soft warn 条件 (所有 hard 都过才判) ---
	if r.FocusChars < 200 {
		r.Warnings = append(r.Warnings, "本周聚焦内容偏少 ("+intToStr(r.FocusChars)+" 字)")
	}
	if r.TakeawayChars < 100 {
		r.Warnings = append(r.Warnings, "启发内容偏少 ("+intToStr(r.TakeawayChars)+" 字)")
	}
	if dailyIssueCount < 5 {
		r.Warnings = append(r.Warnings, "本周日报仅 "+intToStr(dailyIssueCount)+" 份 (不足 5 份)")
	}

	if len(r.Warnings) > 0 {
		r.Warn = true
		return r
	}

	r.Pass = true
	return r
}

// WeeklyFailureDetail 返回一个简短字符串描述为什么 weekly gate hard fail,
// 给 Slack alert 和 error log 用.
func WeeklyFailureDetail(r *WeeklyReport) string {
	if r == nil {
		return "report nil"
	}
	if len(r.Reasons) == 0 {
		return "no hard-fail reason"
	}
	return strings.Join(r.Reasons, "; ")
}

func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
