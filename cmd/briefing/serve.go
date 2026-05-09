// cmd/briefing/serve.go — lightweight static file server for docs/.
//
// This is briefing-v3's own HTTP frontend. It is started once (via
// systemd) and then simply serves the contents of docs/ over HTTP. No
// external web server (nginx/apache/caddy) is required.
//
// Design:
//
//   - one port, one directory, no query routing
//   - root "/" redirects to "/index.html"
//   - any request ending with "/" is served from index.html inside that dir
//   - graceful shutdown on SIGINT/SIGTERM
//   - access log to stdout (captured by systemd journal)
//
// The server intentionally does not add any auth or TLS: those are the
// responsibility of a reverse proxy / CDN if the operator wants them.
// For v1.0.0 this runs plain HTTP on 0.0.0.0:<port>.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"briefing-v3/internal/config"
	"briefing-v3/internal/store"
)

// serveCommand handles `briefing serve` subcommand flags and launches the HTTP server.
// Usage: briefing serve [--port N] [--docs PATH]
func serveCommand(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var (
		port    int
		docsDir string
		addr    string
	)
	fs.IntVar(&port, "port", 8080, "TCP port to listen on")
	fs.StringVar(&docsDir, "docs", "docs", "directory to serve")
	fs.StringVar(&addr, "addr", "0.0.0.0", "interface address (0.0.0.0, ::, 127.0.0.1...)")
	_ = fs.Parse(args)

	absDocs, err := filepath.Abs(docsDir)
	if err != nil {
		return fmt.Errorf("resolve docs dir: %w", err)
	}
	if fi, err := os.Stat(absDocs); err != nil {
		return fmt.Errorf("docs dir %q unreachable: %w", absDocs, err)
	} else if !fi.IsDir() {
		return fmt.Errorf("docs path %q is not a directory", absDocs)
	}

	listenAddr := fmt.Sprintf("%s:%d", addr, port)
	log.Printf("briefing serve: docs=%s listen=%s", absDocs, listenAddr)

	mux := http.NewServeMux()

	// Health check endpoint for systemd / external monitors.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})

	// /api/chat — live AI chat endpoint.
	//
	// The server-side load is best-effort: if config or the database
	// cannot be opened (e.g. missing OPENAI_API_KEY env var) we still
	// start and /api/chat returns 503 instead of hard-failing the whole
	// serve process. This keeps the static file server always up even
	// if chat breaks.
	if cfg, loadErr := config.Load("config/ai.yaml"); loadErr == nil {
		mux.Handle("/api/chat", corsMiddleware(logMiddleware(newChatHandler(cfg, "data/briefing.db"))))
		log.Printf("briefing serve: /api/chat endpoint enabled (model=%s)", cfg.LLM.Model)
	} else {
		log.Printf("briefing serve: /api/chat DISABLED: %v", loadErr)
		mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "chat endpoint unavailable: "+loadErr.Error(), http.StatusServiceUnavailable)
		})
	}

	// File server rooted at docsDir. We wrap it to:
	//   1. redirect "/" to "/index.html" explicitly (some browsers cache
	//      aggressively and "/" is otherwise ambiguous),
	//   2. set sensible Cache-Control so a refreshed page picks up new runs,
	//   3. log each request with method + path + status + duration.
	fileHandler := http.FileServer(http.Dir(absDocs))

	mux.Handle("/", logMiddleware(wrapFileServer(fileHandler)))

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	idleClosed := make(chan struct{})
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		log.Println("briefing serve: shutdown requested")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		close(idleClosed)
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("http server: %w", err)
	}
	<-idleClosed
	log.Println("briefing serve: stopped cleanly")
	return nil
}

// wrapFileServer adds cache headers and index.html fallback on directory paths.
func wrapFileServer(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Force "/" to show the index explicitly so it is obvious in logs.
		if r.URL.Path == "/" {
			r.URL.Path = "/index.html"
		}
		// Stop browsers caching stale pages between daily updates. Note
		// we use must-revalidate + 5-minute max-age so repeat clicks
		// still feel instant but a new day always wins.
		if strings.HasSuffix(r.URL.Path, ".html") {
			w.Header().Set("Cache-Control", "public, max-age=300, must-revalidate")
		} else if strings.Contains(r.URL.Path, "/cards/") && (strings.HasSuffix(r.URL.Path, ".png") || strings.HasSuffix(r.URL.Path, ".jpg")) {
			// v1.0.0: hero 大字报和 item card PNG 路径固定但内容每天变,
			// 不能 cache. 之前用 immutable + 24h 导致浏览器永远显示旧图
			// (用户原话"大字报和之前一模一样"). 改成 no-store 强制每次
			// 重新拉, 让大字报跟早报内容同步.
			w.Header().Set("Cache-Control", "no-store, max-age=0, must-revalidate")
		} else if strings.HasSuffix(r.URL.Path, ".png") || strings.HasSuffix(r.URL.Path, ".jpg") || strings.HasSuffix(r.URL.Path, ".avif") {
			// 其他静态图 (hugo theme assets/logo/favicon) 路径包含 hash
			// 或永不变, 24h 缓存依然安全.
			w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		}
		h.ServeHTTP(w, r)
	})
}

// statusRecorder records the HTTP status for the access log.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

// logMiddleware prints one line per request in a stable format.
func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		elapsed := time.Since(start)
		log.Printf("%s %s %d %s %s", r.Method, r.URL.Path, rec.code, elapsed.Truncate(time.Millisecond), r.RemoteAddr)
	})
}

// corsMiddleware adds CORS headers so the GitHub Pages chat widget can
// call /api/chat cross-origin.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "https://ylzsdafei.github.io")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------- /api/chat endpoint ----------------

// chatRequestBody is the JSON shape the front-end chat widget posts to
// /api/chat. It carries the full conversation history (so the server
// can stay stateless) plus the issue date that the user currently has
// open, which is used to inject the relevant per-day context into the
// LLM system prompt.
type chatRequestBody struct {
	IssueDate string                   `json:"issue_date"`
	Messages  []chatRequestBodyMessage `json:"messages"`
}

type chatRequestBodyMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatResponseBody is what we return to the browser. The front-end
// feeds reply through marked.js for markdown rendering.
type chatResponseBody struct {
	Reply string `json:"reply"`
	Model string `json:"model,omitempty"`
	Error string `json:"error,omitempty"`
}

// newChatHandler returns an http.Handler that proxies POSTs to the
// OpenAI-compatible LLM configured in cfg.LLM. It injects the daily
// briefing content (issue title + summary + item titles + insight) as
// a system prompt so the AI can answer questions about "today's report".
//
// Safety limits:
//   - max 20 messages per turn
//   - max 2000 runes per user message
//   - max_tokens 2048 on the upstream call
//   - 60s hard timeout
//
// Failures return JSON {"reply":"", "error":"..."} with HTTP 500 so
// the front-end can render the error message in-chat instead of a
// blank panel.
func newChatHandler(cfg *config.Config, dbPath string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Content-Type") == "" {
			r.Header.Set("Content-Type", "application/json")
		}

		// Decode request body.
		var req chatRequestBody
		if err := json.NewDecoder(io.LimitReader(r.Body, 256*1024)).Decode(&req); err != nil {
			writeChatError(w, http.StatusBadRequest, "请求格式错误: "+err.Error())
			return
		}
		if len(req.Messages) == 0 {
			writeChatError(w, http.StatusBadRequest, "messages is empty")
			return
		}
		if len(req.Messages) > 20 {
			writeChatError(w, http.StatusBadRequest, "messages count exceeds 20")
			return
		}
		// Trim overly long user messages to protect the LLM budget.
		for i := range req.Messages {
			if len([]rune(req.Messages[i].Content)) > 2000 {
				rs := []rune(req.Messages[i].Content)
				req.Messages[i].Content = string(rs[:2000]) + "……(已截断)"
			}
		}

		// Build system prompt with today's briefing context.
		briefingContext := loadIssueContext(dbPath, req.IssueDate)
		systemPrompt := `你是 briefing-v3 AI 早报的助手。你的任务是帮读者理解今天的 AI 行业早报、解释其中的技术概念、对比不同事件、回答相关问题。

规则:
- 只根据下面给出的"今日早报内容"以及你已知的 AI 行业常识回答
- 如果问题超出今日早报范围, 告诉用户"这条信息今日早报里没有"然后给出你基于常识的简短补充
- 回答用简体中文 Markdown, 可以用 **粗体**、列表、小标题, 但不要用代码块除非在讲代码
- 面向非技术读者, 专业名词要加括号注释
- 保持简洁, 一次回答 100-400 字, 不要写长篇论述, 鼓励追问

---
今日早报内容 (` + req.IssueDate + `):

` + briefingContext

		// Build OpenAI messages.
		msgs := []map[string]any{{"role": "system", "content": systemPrompt}}
		for _, m := range req.Messages {
			role := m.Role
			if role != "user" && role != "assistant" {
				role = "user"
			}
			msgs = append(msgs, map[string]any{"role": role, "content": m.Content})
		}

		// Call the OpenAI-compatible endpoint.
		reply, err := callChatLLM(r.Context(), cfg, msgs)
		if err != nil {
			log.Printf("chat: LLM call failed: %v", err)
			writeChatError(w, http.StatusBadGateway, "AI 服务出错: "+err.Error())
			return
		}

		resp := chatResponseBody{
			Reply: reply,
			Model: cfg.LLM.Model,
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(resp)
	})
}

// writeChatError returns a JSON error response the front-end can show.
func writeChatError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(chatResponseBody{Error: msg, Reply: "**出错了** ❌\n\n" + msg})
}

// loadIssueContext reads the briefing.db SQLite file and returns a
// plain-text dump of the requested issue's title, summary, item
// titles per section, and insight. It is best-effort: on any error
// we return an empty string so the chat still works (just without
// today-specific context).
func loadIssueContext(dbPath, issueDate string) string {
	// Defensively validate the date format (YYYY-MM-DD) so we never
	// feed arbitrary input into a filename or SQL query.
	if len(issueDate) != 10 || issueDate[4] != '-' || issueDate[7] != '-' {
		return ""
	}
	if _, err := time.Parse("2006-01-02", issueDate); err != nil {
		return ""
	}
	s, err := store.New(dbPath)
	if err != nil {
		return ""
	}
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	date, _ := time.Parse("2006-01-02", issueDate)
	issue, err := s.GetIssueByDate(ctx, "ai", date)
	if err != nil || issue == nil {
		return "(今日早报尚未生成)"
	}
	// v1.0.1 Batch 2.13: chat 回答只基于 validated items, 防止把未验证
	// (pending/failed) 的半成品 section 内容喂给 LLM 作为 context.
	items, err := s.ListIssueItemsByStatus(ctx, issue.ID, "validated")
	if err != nil {
		items = nil
	}
	insight, _ := s.GetIssueInsight(ctx, issue.ID)

	var b strings.Builder
	fmt.Fprintf(&b, "【标题】%s\n", strings.TrimSpace(issue.Title))
	if sum := strings.TrimSpace(issue.Summary); sum != "" {
		fmt.Fprintf(&b, "【今日摘要】\n%s\n\n", sum)
	}
	// Group items by section.
	if len(items) > 0 {
		bySection := map[string][]*store.IssueItem{}
		order := []string{}
		for _, it := range items {
			if _, ok := bySection[it.Section]; !ok {
				order = append(order, it.Section)
			}
			bySection[it.Section] = append(bySection[it.Section], it)
		}
		sectionTitles := map[string]string{
			store.SectionProductUpdate: "产品与功能更新",
			store.SectionResearch:      "前沿研究",
			store.SectionIndustry:      "行业展望与社会影响",
			store.SectionOpenSource:    "开源TOP项目",
			store.SectionSocial:        "社媒分享",
		}
		for _, sec := range order {
			title := sectionTitles[sec]
			if title == "" {
				title = sec
			}
			fmt.Fprintf(&b, "【%s】\n", title)
			for i, it := range bySection[sec] {
				t := strings.TrimSpace(it.Title)
				if t == "" {
					continue
				}
				fmt.Fprintf(&b, "%d. %s\n", i+1, t)
			}
			b.WriteString("\n")
		}
	}
	if insight != nil {
		if im := strings.TrimSpace(insight.IndustryMD); im != "" {
			fmt.Fprintf(&b, "【行业洞察】\n%s\n\n", im)
		}
		if om := strings.TrimSpace(insight.OurMD); om != "" {
			fmt.Fprintf(&b, "【对我们的启发】\n%s\n\n", om)
		}
	}
	// Hard cap context so we don't blow the token budget on a huge
	// issue. ~3500 chinese chars is plenty.
	out := b.String()
	if len([]rune(out)) > 3500 {
		rs := []rune(out)
		out = string(rs[:3500]) + "……(上下文已截断)"
	}
	return out
}

// callChatLLM does a single POST to {BaseURL}/v1/chat/completions
// with the pre-built messages slice. Returns the assistant reply text.
func callChatLLM(parent context.Context, cfg *config.Config, messages []map[string]any) (string, error) {
	reqBody := map[string]any{
		"model":       cfg.LLM.Model,
		"messages":    messages,
		"temperature": 0.4,
		"max_tokens":  2048,
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}

	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()

	apiURL := strings.TrimRight(cfg.LLM.BaseURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(buf))
	if err != nil {
		return "", fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.LLM.APIKey)

	hc := &http.Client{Timeout: 60 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(body)
		if len(snippet) > 500 {
			snippet = snippet[:500]
		}
		return "", fmt.Errorf("http %d: %s", resp.StatusCode, snippet)
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("unmarshal: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", fmt.Errorf("llm: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("empty choices")
	}
	return strings.TrimSpace(parsed.Choices[0].Message.Content), nil
}
