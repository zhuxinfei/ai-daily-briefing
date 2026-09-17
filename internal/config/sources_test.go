package config

import (
	"strings"
	"testing"
)

const aiConfigPath = "../../config/ai.yaml"

// TestAIDomainHasEnabledProjectSource 固化导致 2026-09 开源TOP项目整版空白的
// 结构前提: opensource 板块只由 category=project 的源喂养 —
//   - classify 的确定性规则把 project 直接映射到 opensource;
//   - run.go 的 opensource 兜底 (backfillOpenSourceCoverageFromPool) 只从
//     category=project 的候选里捞人.
//
// 也就是说, 只要没有"启用的 project 源", 这个板块就必然空, 而且 pipeline
// 本身不会失败 (它只是安静地跳过). 这条断言让那种情况在 CI 里直接炸掉,
// 而不是等读者发现板块空了几天.
func TestAIDomainHasEnabledProjectSource(t *testing.T) {
	cfg, err := Load(aiConfigPath)
	if err != nil {
		t.Fatalf("Load(%s): %v", aiConfigPath, err)
	}

	var projectSources []SourceConfig
	for _, s := range cfg.EnabledSources() {
		if strings.EqualFold(strings.TrimSpace(s.Category), "project") {
			projectSources = append(projectSources, s)
		}
	}
	if len(projectSources) == 0 {
		t.Fatalf("category=project 的启用源为 0: 开源TOP项目板块将必然为空 " +
			"(classify 与 opensource 兜底都只认 category=project)")
	}

	for _, s := range projectSources {
		if strings.TrimSpace(s.URL) == "" {
			t.Errorf("project 源 %q 没有 url", s.ID)
		}
	}
}

// TestGitHubTrendingSinceIsValid 防止 since 拼错: 这个值会被直接拼进
// github.com/trending 的查询串, 拼错不会报错, 只会静默地把窗口变成
// GitHub 的默认值 (daily), 让"看一周热度"的配置意图悄悄失效.
func TestGitHubTrendingSinceIsValid(t *testing.T) {
	cfg, err := Load(aiConfigPath)
	if err != nil {
		t.Fatalf("Load(%s): %v", aiConfigPath, err)
	}

	valid := map[string]bool{"daily": true, "weekly": true, "monthly": true}
	for _, s := range cfg.EnabledSources() {
		if s.Type != "github_trending" {
			continue
		}
		raw, ok := s.Extra["since"]
		if !ok {
			continue // 省略即用 GitHub 默认窗口, 合法
		}
		since, _ := raw.(string)
		if !valid[strings.ToLower(strings.TrimSpace(since))] {
			t.Errorf("源 %q 的 since=%q 非法, 只接受 daily/weekly/monthly", s.ID, since)
		}
	}
}
