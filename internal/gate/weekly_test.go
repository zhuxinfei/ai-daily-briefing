package gate

import (
	"strings"
	"testing"

	"briefing-v3/internal/store"
)

// TestCheckWeekly 覆盖 W6 gate 三态 + hard-fail / soft-warn 边界.
func TestCheckWeekly(t *testing.T) {
	good := &store.WeeklyIssue{
		Year: 2026, Week: 16,
		Title:       "第16周 AI周报：算力军备",
		FocusMD:     strings.Repeat("本周聚焦内容丰富. ", 30),    // > 200 字
		SignalsMD:   "信号 1. 信号 2.",
		TrendsMD:    "趋势分析正常.",
		TakeawaysMD: strings.Repeat("启发深度要充足. ", 15), // > 100 字
		PonderMD:    "",
	}

	t.Run("pass_all_good", func(t *testing.T) {
		r := CheckWeekly(good, 7, nil)
		if !r.Pass {
			t.Errorf("expect Pass, got %+v", r)
		}
		if r.Warn {
			t.Errorf("expect Warn=false")
		}
	})

	t.Run("hard_fail_empty_focus", func(t *testing.T) {
		bad := *good
		bad.FocusMD = ""
		r := CheckWeekly(&bad, 7, nil)
		if r.Pass || r.Warn {
			t.Errorf("empty focus should hard-fail, got %+v", r)
		}
		if len(r.Reasons) == 0 {
			t.Error("expect reasons for hard fail")
		}
	})

	t.Run("hard_fail_empty_title", func(t *testing.T) {
		bad := *good
		bad.Title = ""
		r := CheckWeekly(&bad, 7, nil)
		if r.Pass || r.Warn {
			t.Errorf("empty title should hard-fail")
		}
	})

	t.Run("hard_fail_zero_dailies", func(t *testing.T) {
		r := CheckWeekly(good, 0, nil)
		if r.Pass || r.Warn {
			t.Errorf("0 daily issues should hard-fail")
		}
	})

	t.Run("hard_fail_banned_pattern_webhook", func(t *testing.T) {
		bad := *good
		bad.TrendsMD = "趋势: 下周 webhook 要升级"
		r := CheckWeekly(&bad, 7, nil)
		if r.Pass {
			t.Errorf("banned pattern 'webhook' should trigger hard-fail")
		}
		if len(r.BannedHits) == 0 {
			t.Errorf("expect BannedHits entry")
		}
	})

	t.Run("soft_warn_focus_too_short", func(t *testing.T) {
		bad := *good
		bad.FocusMD = "短" // < 200 字
		r := CheckWeekly(&bad, 7, nil)
		if r.Pass || !r.Warn {
			t.Errorf("focus < 200 chars should warn, got pass=%v warn=%v", r.Pass, r.Warn)
		}
	})

	t.Run("soft_warn_takeaway_too_short", func(t *testing.T) {
		bad := *good
		bad.TakeawaysMD = "少"
		r := CheckWeekly(&bad, 7, nil)
		if r.Pass || !r.Warn {
			t.Errorf("takeaway < 100 chars should warn")
		}
	})

	t.Run("soft_warn_too_few_dailies", func(t *testing.T) {
		r := CheckWeekly(good, 3, nil)
		if r.Pass || !r.Warn {
			t.Errorf("< 5 dailies should warn")
		}
	})

	t.Run("nil_weekly_hard_fail", func(t *testing.T) {
		r := CheckWeekly(nil, 7, nil)
		if r.Pass || r.Warn {
			t.Errorf("nil weekly should hard-fail")
		}
	})
}
