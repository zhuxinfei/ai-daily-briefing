package generate

import (
	"strings"
	"testing"

	"briefing-v3/internal/store"
)

// merge 20260420: codex 重写 mermaid 为 mermaidLabelFrom + mermaidThemeLabel,
// 旧的 extractMermaidLabels 及其三级兜底测试已失效 (函数不存在), 删除不补齐.
// TestRuleBasedMermaidDiagram 覆盖了集成层, 单元层 TestMermaidLabelFrom 够用.

// TestMermaidLabelFrom 验证 N1 mermaid 节点 label 提取.
func TestMermaidLabelFrom(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Anthropic 连发 Claude Code routines 和电脑控制", "Anthropi"},   // 前 8 rune (Anthropic 9 字, 截 8)
		{"1. **Claude 会点屏**", "Claude 会"},                          // "Claude 会点屏" = 10 rune, 截前 8
		{"教育部把 AI 列为必修课，力推", "教育部把 AI "},                           // "，" 切到 "教育部把 AI 列为必修课", 前 8 rune
		{"  ", ""},
		{"", ""},
		{"OpenAI 扩展 Trusted", "OpenAI 扩"},                         // 前 8 rune
	}
	for _, tc := range cases {
		got := mermaidLabelFrom(tc.in)
		if got != tc.want {
			t.Errorf("in=%q got=%q want=%q", tc.in, got, tc.want)
		}
	}
}

// TestRuleBasedMermaidDiagram 验证 mermaid 块格式合法 + 命中 regex.
func TestRuleBasedMermaidDiagram(t *testing.T) {
	in := &Input{Issue: &store.Issue{Summary: "Anthropic 大升级\n教育部 AI 必修\n斯坦福 Index"}}
	md := ruleBasedMermaidDiagram(in)
	if !strings.HasPrefix(md, "```mermaid\n") {
		t.Errorf("expect ```mermaid prefix, got %q", md[:20])
	}
	if !strings.HasSuffix(md, "```") {
		t.Errorf("expect ``` suffix")
	}
	if !strings.Contains(md, "graph LR") {
		t.Errorf("expect graph LR")
	}
	if !strings.Contains(md, "classDef blue") {
		t.Errorf("expect classDef for styling")
	}
	// 能被 validator 的 mermaidBlockRegex 识别.
	if !mermaidBlockRegex.MatchString(md) {
		t.Errorf("mermaidBlockRegex should match fallback output, got: %s", md)
	}
}
