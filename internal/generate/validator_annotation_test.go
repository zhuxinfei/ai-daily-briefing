package generate

import (
	"strings"
	"testing"
)

// TestValidateInsight_AnnotationAndMermaid exercises the Batch 3 additions
// to ValidateInsight: jargon annotation coverage (soft) and mermaid fence
// detection (soft). Each case targets exactly one of the new contracts so
// regressions show up pinpoint-precisely.
//
// All cases build a minimally-valid insight skeleton (both sections +
// 3 + 2 numbered bullets) so the pre-existing hard contract (Reasons) is
// silent — we then assert on Warnings only for the new soft contracts.
func TestValidateInsight_AnnotationAndMermaid(t *testing.T) {
	// skeleton mirrors a real LLM payload: 3 industry bullets + 2 "our"
	// bullets + a valid mermaid block. Section headers include the
	// "（今日N条）" suffix the pre-existing splitInsightRegex expects —
	// without it the split swallows the body because `[^）]*` is greedy.
	// Skeleton uses ONLY well-known names so it carries zero Warnings by
	// default; tests layer extra content on top by string substitution.
	skeleton := `📊 行业洞察（今日3条）
1. OpenAI 发布 ChatGPT 新版本。【洞察】重要。
2. Google Gemini 升级。【洞察】关键。
3. Anthropic 放出 Claude 更新。【洞察】值得关注。

` + "```mermaid\ngraph LR\nA-->B\n```" + `

💭 对我们的启发（今日2条）
1. OpenAI 动作说明方向。
2. Google Gemini 的策略给了参考。
`

	cases := []struct {
		name          string
		raw           string
		wantOK        bool
		wantReasonSub string // substring expected in Reasons (when !wantOK)
		wantWarnSub   string // substring expected in Warnings (empty = no warn required)
		noWarnSub     string // substring that MUST NOT appear in Warnings
	}{
		{
			name: "case1_jargon_RAG_without_annotation",
			// Replace a "Gemini" mention with unannotated RAG.
			raw: strings.Replace(skeleton,
				"Google Gemini 升级",
				"RAG 技术兴起",
				1,
			),
			wantOK:      true, // soft contract → Reasons empty, OK=true
			wantWarnSub: "RAG",
		},
		{
			name: "case2_jargon_RAG_with_annotation",
			raw: strings.Replace(skeleton,
				"Google Gemini 升级",
				"RAG（检索增强生成，让 AI 能查资料后回答）兴起",
				1,
			),
			wantOK:    true,
			noWarnSub: "RAG",
		},
		{
			name: "case3_wellknown_OpenAI_without_annotation",
			// skeleton already has un-annotated OpenAI → no warning expected
			raw:       skeleton,
			wantOK:    true,
			noWarnSub: "OpenAI",
		},
		{
			name: "case4_missing_mermaid_block",
			raw: strings.Replace(skeleton,
				"```mermaid\ngraph LR\nA-->B\n```",
				"", // strip the fenced block entirely
				1,
			),
			wantOK:      true, // mermaid is soft
			wantWarnSub: "missing mermaid",
		},
		{
			name:      "case5_with_mermaid_block",
			raw:       skeleton,
			wantOK:    true,
			noWarnSub: "missing mermaid",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidateInsight(tc.raw)

			if got.OK != tc.wantOK {
				t.Fatalf("OK mismatch: got %v (reasons=%v) want %v",
					got.OK, got.Reasons, tc.wantOK)
			}

			if tc.wantReasonSub != "" {
				found := false
				for _, r := range got.Reasons {
					if strings.Contains(r, tc.wantReasonSub) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected Reasons to mention %q, got %v",
						tc.wantReasonSub, got.Reasons)
				}
			}

			if tc.wantWarnSub != "" {
				found := false
				for _, w := range got.Warnings {
					if strings.Contains(w, tc.wantWarnSub) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected Warnings to mention %q, got %v",
						tc.wantWarnSub, got.Warnings)
				}
			}

			if tc.noWarnSub != "" {
				for _, w := range got.Warnings {
					if strings.Contains(w, tc.noWarnSub) {
						t.Errorf("did not expect warning for %q, got %v",
							tc.noWarnSub, got.Warnings)
					}
				}
			}
		})
	}
}

// TestValidateCompose_AnnotationCoverage confirms that the new
// ValidateCompose helper — which will be wired into summarize.go's retry
// loop by Batch 2 (D5 decision) — catches un-annotated jargon at the
// section-body level. Empty input must hard-fail so the retry loop can
// tell "LLM returned empty" from "LLM returned something".
func TestValidateCompose_AnnotationCoverage(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantOK      bool
		wantReason  string
		wantWarnSub string
		noWarnSub   string
	}{
		{
			name:       "empty_body_is_hard_fail",
			body:       "",
			wantOK:     false,
			wantReason: "compose 输出为空",
		},
		{
			name: "jargon_embedding_without_annotation_warns",
			body: `1. **OpenAI 开源新模型。**
该模型支持 embedding 向量搜索 🚀 并提升了推理速度。`,
			wantOK:      true,
			wantWarnSub: "embedding",
		},
		{
			name: "jargon_embedding_with_annotation_no_warn",
			body: `1. **OpenAI 开源新模型。**
该模型支持 embedding（把文字变成向量便于 AI 比较相似度）搜索 🚀 并提升了推理速度。`,
			wantOK:    true,
			noWarnSub: "embedding",
		},
		{
			name: "wellknown_OpenAI_without_annotation_ok",
			body: `1. **OpenAI 发布。**
OpenAI 和 Anthropic 同日更新 🚀。`,
			wantOK:    true,
			noWarnSub: "OpenAI",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidateCompose(tc.body)

			if got.OK != tc.wantOK {
				t.Fatalf("OK mismatch: got %v (reasons=%v) want %v",
					got.OK, got.Reasons, tc.wantOK)
			}
			if tc.wantReason != "" {
				found := false
				for _, r := range got.Reasons {
					if strings.Contains(r, tc.wantReason) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected Reasons to mention %q, got %v",
						tc.wantReason, got.Reasons)
				}
			}
			if tc.wantWarnSub != "" {
				found := false
				for _, w := range got.Warnings {
					if strings.Contains(w, tc.wantWarnSub) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected Warnings to mention %q, got %v",
						tc.wantWarnSub, got.Warnings)
				}
			}
			if tc.noWarnSub != "" {
				for _, w := range got.Warnings {
					if strings.Contains(w, tc.noWarnSub) {
						t.Errorf("did not expect warning for %q, got %v",
							tc.noWarnSub, got.Warnings)
					}
				}
			}
		})
	}
}

// TestGlossaryLoads guards against a malformed glossary.yaml (e.g. an
// accidental tab or an unquoted YAML-reserved word breaking parse). It's
// the smallest possible loader test — if this passes, jargon detection
// is at least wired up.
func TestGlossaryLoads(t *testing.T) {
	g, err := loadGlossary()
	if err != nil {
		t.Fatalf("loadGlossary() error: %v", err)
	}
	if g == nil {
		t.Fatal("loadGlossary() returned nil glossary")
	}
	if len(g.WellKnown) < 30 {
		t.Errorf("well_known has %d entries, want >=30", len(g.WellKnown))
	}
	if len(g.Jargon) < 80 {
		t.Errorf("jargon has %d entries, want >=80", len(g.Jargon))
	}
	if len(jargonPatterns) != len(jargonTerms) {
		t.Errorf("jargonPatterns/jargonTerms length mismatch: %d vs %d",
			len(jargonPatterns), len(jargonTerms))
	}
}
