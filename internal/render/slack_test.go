package render

import (
	"strings"
	"testing"
)

// TestStripMermaidBlocks_RemovesHeading 验证 T2 修复:
// "🗺️ 今日关系图" heading 应该跟 mermaid 块一起被 strip, 否则会留下空标题孤儿.
func TestStripMermaidBlocks_RemovesHeading(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		notContains []string
	}{
		{
			name: "heading_with_mermaid",
			input: "💭 启发\n\n1. xxx\n\n🗺️ 今日关系图\n\n```mermaid\ngraph LR\nA-->B\n```\n",
			notContains: []string{
				"🗺️", "今日关系图", "mermaid", "graph LR",
			},
		},
		{
			name:  "heading_only_no_mermaid",
			input: "正文\n\n🗺️ 今日关系图\n\n后面段落",
			notContains: []string{
				"🗺️", "今日关系图",
			},
		},
		{
			name:  "heading_with_hash_prefix",
			input: "## 🗺️ 今日关系图\n\n```mermaid\ngraph LR\nA-->B\n```",
			notContains: []string{
				"今日关系图", "mermaid",
			},
		},
		{
			name:  "no_heading_no_mermaid",
			input: "纯文本无任何关系图标记",
			notContains: []string{
				"今日关系图", "mermaid",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := StripMermaidBlocks(tc.input)
			for _, s := range tc.notContains {
				if strings.Contains(out, s) {
					t.Errorf("output unexpectedly contains %q\nfull output:\n%s", s, out)
				}
			}
		})
	}
}
