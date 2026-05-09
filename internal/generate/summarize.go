package generate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"briefing-v3/internal/store"
)

// Summarizer is the Step 1B LLM text-generation interface used by the
// compose package to turn a batch of same-section RawItems into a single
// markdown chunk. The output format follows the upstream
// summarizationPromptStepOne style: ordered list, bold titles, adequate
// emoji, embedded hyperlinks.
//
// Concrete implementations are wired up via the existing
// openaiGenerator (which also implements Generator) so callers can share
// a single Config and HTTP client across both interfaces.
type Summarizer interface {
	// Summarize returns the markdown body for one section. sectionTitle
	// is the human-facing section name (e.g. "产品与功能更新") — NOT
	// the internal section id. Empty items returns empty string, nil.
	Summarize(ctx context.Context, sectionTitle string, items []*store.RawItem) (string, error)
}

// summarizeSystemPrompt is ported from upstream
// CloudFlare-AI-Insight-Daily/src/prompt/summarizationPromptStepOne.js
// then extended with the "好的示例" clause that the upstream evals
// rely on. Do not paraphrase: the emoji, quoting and numbered-list
// requirements are load-bearing, downstream render code trims leading
// markdown headers and expects ordered-list items.
const summarizeSystemPrompt = `你是一名专业的 AI 日报编辑。你的任务是把一批 AI 领域的候选条目整理成一个 section 的主体 markdown 内容。

输出格式要求:
1. 每条条目用有序列表 "1. **一句话概括标题。**\n紧跟一段 3-5 句话说明 🚀 带适度 emoji 💡"
2. 说明里必须引用超链接 **[简短中文锚文本(briefing)](原始URL)**
   - 锚文本由你根据内容自拟，6-14 个汉字，点出具体是什么（例："官方发布页(briefing)"、"GitHub 仓库(briefing)"、"完整报道(briefing)"、"arxiv 论文(briefing)"、"黄仁勋演讲视频(briefing)"）
   - **严禁** 输出裸露 URL（如 "详情见 https://xxx.com"），所有 URL 必须包在 [锚文本](url) 里
   - **严禁** 把 URL 本身当作锚文本（如 [https://xxx.com](https://xxx.com)）
   - 每条至少 1 个超链接引用，最多 3 个
3. 关键词用 **粗体** 强调
4. 语言风格: 通俗易懂、流畅自然、生动不失深度，有适度口语化 (๑•̀ㅂ•́)و 但不低俗
5. 非大众熟知的专业名词必须加括号注释（让非技术同事读完能获得知识）
   读者是公司里做业务 / 人事 / 行政 / 财务 / 运营 / 设计等文职岗位的同事 —— 他们会用 ChatGPT，但不懂代码、不熟 AI 技术栈、不认论文术语。
   目标不是"让他们看完名词列表"，而是"让他们读完每条能明白这件事对行业/产品/工作意味着什么"。宁可多 1 个注释也不要让文职同事读完一脸问号。
   大众熟知不用注释：OpenAI、ChatGPT、Google、Meta、Anthropic、Claude、DeepSeek、Gemini、英伟达、GitHub、arxiv 等大公司大产品；Agent、AI、大模型、开源、API、prompt 等大众化概念。
   必须注释示例：
     产品/工具类：Skyscanner（全球机票比价平台）、HuggingFace（全球最大 AI 模型共享社区）、Sentry（帮程序员自动发现 bug 的工具）、Notion（团队协作办公工具）
     框架/技术栈：PyTorch（最流行的 AI 开发框架）、Safetensors（一种更安全的 AI 模型文件打包方式）、WebGPU（让浏览器直接调用显卡加速的新标准）、MCP（Anthropic 提出的模型上下文协议）
     概念/算法：RAG（检索增强生成，让 AI 回答前先查资料）、embedding（把文字变成向量便于 AI 比较相似度）、MoE（混合专家模型，多个小模型分工协作）、fine-tuning（用特定数据微调模型）、LoRA（一种低成本微调模型的技术）、tokenizer（把文字切成小块喂给模型的工具）、inference（模型推理，让训练好的模型回答问题的过程）、checkpoint（训练中保存的模型快照）、alignment（对齐, 让模型行为符合人类意图的训练过程）
     硬件/基础设施：GaN（一种比硅更省电的新型芯片材料）、GPU cluster（显卡集群, 训练大模型的硬件设施）、GW 级算力（吉瓦级电力规模的数据中心）
   需要注释的类别：编程框架、开发者工具、学术项目、底层技术概念、模型训练术语、论文算法名等纯技术名词。
   英文论文/项目名可以保留原名（如 Seedance 2.0 / OccuBench），但必须用中文说明它是什么、解决什么问题、对谁有用。
   判断标准：如果 HR / 行政 / 财务 / 运营 同事可能不认识这个词，就必须加注释。

   **强制规则（严格执行）**：
     a. 一条正文里任何英文单词/短语/缩写（除去 OpenAI/Google/Meta/Anthropic/ChatGPT/Claude/GitHub 这 7 个大家都认识的名词），都必须加中文翻译或注释。例："AI browser companion（AI 浏览器助手, 陪你浏览网页协助完成任务）"。
     b. 一条正文至少要有 1-2 个术语注释。如果实在找不到需要注释的术语, 说明这条内容本身太简单没有技术深度, 优先选其他候选条目。
     c. 括号注释不要用"XX 技术"、"XX 方法"这种废话, 要说清楚"它是干嘛的, 解决什么问题, 普通人能理解的类比更好"。反例："CheckPoint（训练检查点）" 太空 → 正例："CheckPoint（训练过程中保存的模型快照, 像写作业时的'存档'）"。
     d. 不要把所有注释堆在一条里, 让注释在整段 item 里自然分布。
6. 严格来源于原材料，不捏造、不添加原文未提及的事实
7. 输出必须是简体中文 Markdown
8. 直接输出 markdown，不加任何前置说明或标题
9. 关于标题：如果原始条目的标题本身已经简洁有力、含有具体公司/产品名、读起来顺，允许直接引用或做轻度汉化/改写，不必强行重写；标题党风格的词（"突袭"/"炸裂"/"屠榜"/"重磅"等）每条最多用一个，不堆砌
   **标题级注释强制规则（最重要，严格执行）**：标题里只要出现英文术语 / 论文名 / 算法名 / 产品名 / 技术名词, 必须在英文名后面紧跟一个中文括号注释, 让文职同事看标题就能懂这是什么. 不要把注释只藏在正文里 —— 读者看标题一眼就懂, 否则整条价值大打折扣.
     **唯一白名单（只有这些可以不注释）**: OpenAI / Google / Microsoft / Amazon / Meta / Apple / Nvidia / 英伟达 / Anthropic / ChatGPT / Claude / Claude Code / Codex / Cursor / Gemini / Copilot / GitHub / DeepSeek / Chrome / Mac / Windows / iOS / Android — 仅限这些公司名本身和旗舰 AI 助手/编码工具本身. 超出这个清单一律注释, 没有例外.
     **核心原则（比清单更重要）**: 公司名家喻户晓 ≠ 该公司所有产品都家喻户晓. 白名单只放"公司名本身"和"旗舰 AI 助手本身", 公司名后面跟着任何具体子产品/子服务/子平台（无论是哪家公司的）都必须加注释 —— 判断标准始终是"HR/财务/行政同事会不会想 '这到底是啥'". 会, 就加注释. 下面反例是讲原则, 不是穷举清单:
     反例（纯技术术语）：**Target Policy Optimization 提出更稳的决策优化思路。** → 正例：**Target Policy Optimization（一种新的强化学习训练方法）提出更稳的决策优化思路。**
     反例（论文/开源项目名）：**Seedance 2.0 开源登场。** → 正例：**Seedance 2.0（字节跳动推出的 AI 视频生成模型）开源登场。**
     反例（新产品名）：**HoloTab 登陆 Chrome。** → 正例：**HoloTab（一款把浏览器标签页变成 AI 助手的插件）登陆 Chrome。**
     反例（白名单公司的具体子产品）：**AWS 推出 Amazon Bio Discovery。** → 正例：**AWS 推出 Amazon Bio Discovery（亚马逊面向生命科学的 AI 研究平台）。**
     反例（白名单公司的企业协作产品）：**DeepL 瞄准 Microsoft Teams 会议场景。** → 正例：**DeepL 瞄准 Microsoft Teams（微软的企业协作与视频会议平台）会议场景。**
     读懂原则: 上面的反例和正例是**原理示范**, 不是要你只注释这几个特定名字. 遇到任何结构类似的情况（公司名+具体子产品、陌生技术词、专业缩写）都照原则处理.
10. 说明部分必须基于原文重新组织语言，不能只是原文复制粘贴

重要通用原则: 所有摘要内容必须严格来源于原文。不得捏造、歪曲或添加原文未提及的信息。读者是不懂技术的同事与领导，优先把话说清楚，其次才是吸引眼球。

参考好的示例:
1. **DeepSeek 深夜暗更疑似 V4 突袭。**
DeepSeek 昨晚推送重磅更新，新增**快速模式**和**专家模式**两档 ⚡。网友实测后模型竟 😲 自称**V4版本**，视觉模型也悄悄开启灰度测试。[DeepSeek 更新详情(briefing)](原始URL)

2. **Anthropic 托管 Agent 定价 0.08 美元/小时，Agent 白菜价时代到来。**
Anthropic 放出**托管式 Agent 平台**，开发者只需定义任务和规则就能让 Claude 自动 👉 跑完整个流程。Sentry（程序员自动查 bug 的工具）已实现代码自动修复，Notion（团队协作办公工具）支持多任务并行。[Anthropic 托管 Agent 发布(briefing)](原始URL)`

// summarizeUserPromptTemplate takes:
//
//	%s: section title (e.g. "产品与功能更新")
//	%s: joined candidate item lines
const summarizeUserPromptTemplate = `Section: %s

以下是本 section 的候选条目，请按要求整理输出:

%s`

// Summarize implements the Summarizer interface on openaiGenerator.
// The existing openaiGenerator (from openai.go) already carries a Config
// and http.Client, so we reuse them here rather than standing up a new
// struct. openaiGenerator therefore satisfies both the Generator and
// Summarizer interfaces.
func (g *openaiGenerator) Summarize(ctx context.Context, sectionTitle string, items []*store.RawItem) (string, error) {
	if len(items) == 0 {
		return "", nil
	}
	if strings.TrimSpace(sectionTitle) == "" {
		return "", errors.New("generate: Summarize requires a non-empty sectionTitle")
	}

	userPrompt := fmt.Sprintf(
		summarizeUserPromptTemplate,
		sectionTitle,
		formatItemsForSummarize(items),
	)

	// v1.0.1 Bug J 修复: retry 节奏由 cfg.RetryBackoffSeconds 驱动.
	// 旧策略 1/2/4/8s 共 15s 对 LLM 上游分钟级抖动完全无效 (2026-04-14
	// 故障实证: 5 次全挂). 默认序列 [10,30,90,180,300] 总 ~10 分钟,
	// 给单 section 足够恢复窗口; 可通过 ai.yaml llm.retry_backoff_seconds
	// 运维调. Length 决定有效 MaxAttempts.
	backoffs := g.cfg.RetryBackoffSeconds
	if len(backoffs) == 0 {
		backoffs = []int{10, 30, 90, 180, 300}
	}
	maxAttempts := len(backoffs)
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		raw, err := g.chatComplete(ctx, summarizeSystemPrompt, userPrompt, g.cfg.MaxTokens*2)
		if err != nil {
			lastErr = err
			if attempt < maxAttempts {
				backoff := time.Duration(backoffs[attempt-1]) * time.Second
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(backoff):
				}
			}
			continue
		}
		cleaned := strings.TrimSpace(raw)
		if cleaned == "" {
			lastErr = errors.New("generate: Summarize produced empty output")
			continue
		}
		return cleaned, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("generate: Summarize failed after %d attempts", maxAttempts)
	}
	return "", lastErr
}

// formatItemsForSummarize renders each RawItem as one bullet in the
// user prompt. We include title, url, source id and a truncated excerpt
// of the extracted content so the LLM has enough grounding to summarize.
func formatItemsForSummarize(items []*store.RawItem) string {
	const maxItems = 8     // cap so prompt never blows token budget
	const maxExcerpt = 250 // runes per item excerpt (reduced from 400 to avoid 502)

	var b strings.Builder
	n := 0
	for _, it := range items {
		if it == nil {
			continue
		}
		if n >= maxItems {
			break
		}
		excerpt := strings.TrimSpace(it.Content)
		if excerpt == "" {
			excerpt = "(no excerpt)"
		}
		if len([]rune(excerpt)) > maxExcerpt {
			excerpt = string([]rune(excerpt)[:maxExcerpt]) + "..."
		}
		// Collapse newlines in excerpt so each item stays on a couple
		// of lines and the LLM can easily parse the list.
		excerpt = strings.ReplaceAll(excerpt, "\n", " ")
		title := strings.TrimSpace(it.Title)

		fmt.Fprintf(&b, "- 标题: %s\n  来源: source#%d\n  URL: %s\n  摘要: %s\n\n",
			title, it.SourceID, it.URL, excerpt)
		n++
	}
	return strings.TrimSpace(b.String())
}

// compile-time assertion: openaiGenerator implements Summarizer.
var _ Summarizer = (*openaiGenerator)(nil)

// (unused helper to silence the unused-import linter if time ends up
// unreferenced in a given build — kept intentionally near the var so a
// human reviewer sees the note.)
var _ = time.Second
