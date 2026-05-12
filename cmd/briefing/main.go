// Package main is the briefing-v3 CLI entry point.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"briefing-v3/internal/config"
	"briefing-v3/internal/render"
	"briefing-v3/internal/store"
)

// v1.0.1 exit code taxonomy (D1 决策 in docs/specs/2026-04-14-v1.0.1-critical-fix-plan.md).
// orchestrator.sh 根据这些 code 决定重试 / 告警 / 放弃.
const (
	ExitSuccess     = 0 // pipeline complete + published
	ExitUsage       = 2 // transient (CLI 用法错误 or LLM 5xx/timeout — orchestrator 重试)
	ExitContentFail = 3 // gate hard fail, 文字产物缺失 (orchestrator 仍重试至 Attempt 4 走 repair)
	ExitConfigFail  = 4 // BRIEFING_MODE 缺失 / secrets.env 缺 key / LLM endpoint 不可达
	ExitInfraFail   = 5 // DB 锁 / 磁盘满 / panic
	ExitNeedsHuman  = 6 // Attempt 4 + repair 仍缺 section, 告警已发, 等人工介入 (Batch 2.21)
)

// PipelineError 是 pipeline 层的类型化错误, main() 通过它映射 exit code.
type PipelineError struct {
	Code int
	Err  error
}

func (e *PipelineError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit code %d", e.Code)
	}
	return e.Err.Error()
}

func (e *PipelineError) Unwrap() error { return e.Err }

// 类型化 error 构造器, 让 pipeline 各层清晰分类失败原因.
func transient(err error) error   { return &PipelineError{Code: ExitUsage, Err: err} }
func contentFail(err error) error { return &PipelineError{Code: ExitContentFail, Err: err} }
func configFail(err error) error  { return &PipelineError{Code: ExitConfigFail, Err: err} }
func infraFail(err error) error   { return &PipelineError{Code: ExitInfraFail, Err: err} }
func needsHuman(err error) error  { return &PipelineError{Code: ExitNeedsHuman, Err: err} }

// mapExitCode 把任意 error 映射到对应的 exit code.
// 非 PipelineError 的错误默认归为 infra, 防止意外 panic 被误判为 transient.
func mapExitCode(err error) int {
	if err == nil {
		return ExitSuccess
	}
	var pe *PipelineError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ExitInfraFail
}

// briefingMode 读取 BRIEFING_MODE 环境变量, 未设置或非法值返回 ExitConfigFail.
// v1.0.1 D2 决策: BRIEFING_MODE 必填, 防止"调试阶段误推正式频道"(2026-04-14
// 故障场景). secrets.env 模板默认 debug, 需显式改 prod 才推正式频道.
func briefingMode() (string, error) {
	m := strings.ToLower(strings.TrimSpace(os.Getenv("BRIEFING_MODE")))
	switch m {
	case "debug", "prod":
		return m, nil
	case "":
		return "", configFail(errors.New("BRIEFING_MODE env var is required (set 'debug' or 'prod' in config/secrets.env)"))
	default:
		return "", configFail(fmt.Errorf("BRIEFING_MODE=%q is invalid, must be 'debug' or 'prod'", m))
	}
}

// 抑制 "imported and not used" 编译错误, 上述函数在 Batch 2 后续项目里会被调用.
var _ = transient
var _ = contentFail
var _ = infraFail
var _ = needsHuman

const usage = `briefing-v3 — AI daily briefing generator

Usage:
    briefing <command> [flags]

Commands:
    migrate     Initialize or migrate the SQLite schema
    seed        Load sources from config/ai.yaml into the database
    run         Fetch + classify + compose + render + publish (main pipeline)
    repair      Re-run compose for specific sections (v1.0.1: orchestrator uses
                this on Attempt 4 when some section stayed 'failed'; currently
                aliases to full 'run' — real per-section fallback is P1 refinement)
    weekly      Generate weekly analysis report from this week's daily issues
    regen       Reuse existing SQLite data, rebuild infocard + HTML + push
    serve       Start static file server for docs/ (web viewer)
    promote     Manually promote an existing issue to Slack prod channel
    promote-weekly  Re-post the saved weekly Slack payload to Slack prod
    promote-feishu  Re-post the saved daily Feishu card to Feishu chat
    promote-weekly-feishu
                Re-post the saved weekly Feishu card to Feishu chat
    status      Show the status of a specific issue (v1.0.1: displays issue
                metadata + per-section item status + recent deliveries)
    help        Show this help message

Flags (available on most commands):
    -c, --config string   YAML config path (default "config/ai.yaml")
    -d, --date string     Issue date YYYY-MM-DD (default today in Asia/Shanghai)
        --domain string   Domain id (default "ai")
        --target string   test|auto|prod (default "test")
        --dry-run         Skip actual Slack push
        --no-images       Skip mediaextract / infocard / headline PNG stages
                          (text-only mode, used as an escape hatch when image
                          pipeline is unstable)

Serve flags:
        --port int        listen port (default 8080)
        --docs string     directory to serve (default "docs")
        --addr string     bind address (default "0.0.0.0")
`

// globalFlags are shared by most subcommands. They are parsed once at
// entry and passed down to each command implementation.
type globalFlags struct {
	configPath string
	dateStr    string
	domain     string
	target     string
	dryRun     bool
	noImages   bool
	mediaOnly  bool
}

// parseGlobalFlags parses the flag set used by every command. Unknown
// flags cause FlagSet.ExitOnError to terminate with a usage message.
func parseGlobalFlags(args []string) (*globalFlags, []string) {
	fs := flag.NewFlagSet("briefing", flag.ExitOnError)
	gf := &globalFlags{}
	fs.StringVar(&gf.configPath, "config", "config/ai.yaml", "YAML config path")
	fs.StringVar(&gf.configPath, "c", "config/ai.yaml", "YAML config path (shorthand)")
	fs.StringVar(&gf.dateStr, "date", "", "issue date YYYY-MM-DD")
	fs.StringVar(&gf.dateStr, "d", "", "issue date (shorthand)")
	fs.StringVar(&gf.domain, "domain", "ai", "domain id")
	fs.StringVar(&gf.target, "target", "test", "test|auto|prod")
	fs.BoolVar(&gf.dryRun, "dry-run", false, "skip actual slack push")
	fs.BoolVar(&gf.noImages, "no-images", false, "skip all image generation stages (mediaextract/infocard/headline)")
	fs.BoolVar(&gf.mediaOnly, "media-only", false, "regen only: skip infocard template PNGs, use mediaextract og:image instead (hubtoday style)")
	_ = fs.Parse(args)
	return gf, fs.Args()
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(ExitUsage)
	}
	cmd := os.Args[1]
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		fmt.Print(usage)
		return
	}

	// `serve` has its own flag set, handle it before parseGlobalFlags
	// (which understands --date, --target etc that are irrelevant here).
	if cmd == "serve" {
		if err := serveCommand(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "serve error: %v\n", err)
			os.Exit(mapExitCode(err))
		}
		return
	}

	gf, _ := parseGlobalFlags(os.Args[2:])

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(gf.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(ExitConfigFail)
	}

	date, err := resolveDate(gf.dateStr, cfg.Domain.Timezone)
	if err != nil {
		fmt.Fprintf(os.Stderr, "date error: %v\n", err)
		os.Exit(ExitUsage)
	}

	// v1.0.1 Batch 2.7: 对会推 Slack 或调用 LLM 的命令强制校验 BRIEFING_MODE.
	// migrate/seed/status/help 不需要 (纯本地, 不推不调).
	switch cmd {
	case "run", "repair", "weekly", "regen", "promote", "promote-weekly", "promote-feishu", "promote-weekly-feishu":
		if _, merr := briefingMode(); merr != nil {
			fmt.Fprintf(os.Stderr, "config error: %v\n", merr)
			os.Exit(ExitConfigFail)
		}
	}

	switch cmd {
	case "migrate":
		err = migrateCommand(ctx, cfg)
	case "seed":
		err = seedCommand(ctx, cfg)
	case "run":
		// #9: 全局 pipeline 超时 30 分钟, 防止 LLM hang / 网络卡死导致
		// pipeline 无限等待. 只对 run 子命令生效.
		runCtx, runCancel := context.WithTimeout(ctx, 30*time.Minute)
		err = runCommand(runCtx, cfg, date, gf)
		runCancel()
	case "repair":
		// v1.0.1 Batch 2.18: orchestrator Attempt 4 调 briefing repair
		// --section X,Y 只补缺失 section. 当前最小实现: 别名给 run (Team C
		// orchestrator 已经 graceful fallback 到完整 run 所以等价).
		// 真正的 per-section 重跑 (跳过 rank+classify, 只 compose 指定
		// section) 是 P1 refinement, 等后续细化.
		repairCtx, repairCancel := context.WithTimeout(ctx, 30*time.Minute)
		err = runCommand(repairCtx, cfg, date, gf)
		repairCancel()
	case "weekly":
		err = weeklyCommand(ctx, cfg, date, gf)
	case "regen":
		err = regenCommand(ctx, cfg, date, gf)
	case "promote":
		err = promoteCommand(ctx, cfg, date, gf)
	case "promote-weekly":
		err = promoteWeeklyCommand(ctx, cfg, date, gf)
	case "promote-feishu":
		err = promoteFeishuCommand(ctx, cfg, date, gf)
	case "promote-weekly-feishu":
		err = promoteWeeklyFeishuCommand(ctx, cfg, date, gf)
	case "status":
		err = statusCommand(ctx, cfg, date, gf)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		fmt.Fprint(os.Stderr, usage)
		os.Exit(ExitUsage)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(mapExitCode(err))
	}
}

// resolveDate parses a YYYY-MM-DD string in the given timezone, or
// returns today's midnight in that timezone when s is empty.
func resolveDate(s, tz string) (time.Time, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil || loc == nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	if s == "" {
		now := time.Now().In(loc)
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc), nil
	}
	t, err := time.ParseInLocation("2006-01-02", s, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid date %q: %w", s, err)
	}
	return t, nil
}

// migrateCommand initializes the SQLite schema.
// BRIEFING_DB env var overrides the default path so Team A's batch-1
// verification can run against /tmp/test_batch1.db without touching
// the production database.
func migrateCommand(ctx context.Context, cfg *config.Config) error {
	dbPath := os.Getenv("BRIEFING_DB")
	if dbPath == "" {
		dbPath = "data/briefing.db"
	}
	s, err := store.New(dbPath)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		return err
	}
	fmt.Printf("migrate: OK (db=%s)\n", dbPath)
	return nil
}

// seedCommand inserts the domain + all enabled sources from config into DB.
func seedCommand(ctx context.Context, cfg *config.Config) error {
	s, err := store.New("data/briefing.db")
	if err != nil {
		return err
	}
	defer s.Close()

	// Ensure schema exists before inserting.
	if err := s.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	// Upsert domain record.
	if err := s.UpsertDomain(ctx, &store.Domain{
		ID:         cfg.Domain.ID,
		Name:       cfg.Domain.Name,
		ConfigPath: "config/ai.yaml",
	}); err != nil {
		return fmt.Errorf("upsert domain: %w", err)
	}

	// Upsert each enabled source. We serialize the full SourceConfig so
	// adapters can recover type-specific options (query/hl/gl/limit/...).
	inserted := 0
	for _, src := range cfg.EnabledSources() {
		cfgJSON, err := marshalSourceConfig(src)
		if err != nil {
			return fmt.Errorf("marshal source %s: %w", src.ID, err)
		}
		_, err = s.UpsertSource(ctx, &store.Source{
			DomainID:   cfg.Domain.ID,
			Type:       src.Type,
			Name:       src.Name,
			ConfigJSON: cfgJSON,
			Enabled:    src.Enabled,
		})
		if err != nil {
			return fmt.Errorf("upsert source %s: %w", src.ID, err)
		}
		inserted++
	}
	fmt.Printf("seed: %d sources upserted\n", inserted)
	return nil
}

// marshalSourceConfig serializes a SourceConfig to a JSON string blob
// suitable for storing in sources.config_json.
func marshalSourceConfig(src config.SourceConfig) (string, error) {
	// Build a flat map combining the explicit fields with the inline Extra,
	// so adapters can unmarshal into whatever shape they prefer.
	payload := map[string]any{
		"id":       src.ID,
		"type":     src.Type,
		"category": src.Category,
		"name":     src.Name,
		"url":      src.URL,
		"enabled":  src.Enabled,
		"priority": src.Priority,
	}
	for k, v := range src.Extra {
		payload[k] = v
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// runCommand is the main pipeline entry point. The actual wiring of every
// stage lives in run.go so this file stays focused on CLI plumbing.
func runCommand(ctx context.Context, cfg *config.Config, date time.Time, gf *globalFlags) error {
	return runPipeline(ctx, cfg, date, gf)
}

// payloadSnapshotPath returns the canonical file path that run/regen
// use to persist the last Slack payload they POSTed to the test
// channel. promoteCommand reads this file and re-POSTs the EXACT same
// bytes to the prod webhook so "manual sign-off then promote" cannot
// ever drift from what the reviewer saw in test.
func payloadSnapshotPath(date time.Time) string {
	return fmt.Sprintf("data/slack-payload-%s.json", date.Format("2006-01-02"))
}

func weeklySnapshotLabel(date time.Time) string {
	year, week := date.ISOWeek()
	return fmt.Sprintf("%d-W%02d", year, week)
}

func weeklyPayloadSnapshotPath(date time.Time) string {
	return fmt.Sprintf("data/slack-weekly-payload-%s.json", weeklySnapshotLabel(date))
}

func dailyFeishuSnapshotPath(date time.Time) string {
	return fmt.Sprintf("data/feishu-daily-card-%s.json", date.Format("2006-01-02"))
}

func weeklyFeishuSnapshotPath(date time.Time) string {
	return fmt.Sprintf("data/feishu-weekly-card-%s.json", weeklySnapshotLabel(date))
}

// savePayloadSnapshot writes payload verbatim to payloadSnapshotPath(date).
// Used by run and regen right before they POST to test, so the prod
// promote path can re-send bytes that are guaranteed to match exactly.
func savePayloadSnapshot(date time.Time, payload []byte) error {
	return saveJSONSnapshot(payloadSnapshotPath(date), payload)
}

func saveWeeklyPayloadSnapshot(date time.Time, payload []byte) error {
	return saveJSONSnapshot(weeklyPayloadSnapshotPath(date), payload)
}

func saveDailyFeishuSnapshot(date time.Time, payload []byte) error {
	return saveJSONSnapshot(dailyFeishuSnapshotPath(date), payload)
}

func saveWeeklyFeishuSnapshot(date time.Time, payload []byte) error {
	return saveJSONSnapshot(weeklyFeishuSnapshotPath(date), payload)
}

func saveJSONSnapshot(path string, payload []byte) error {
	if err := os.MkdirAll("data", 0o755); err != nil {
		return fmt.Errorf("mkdir data: %w", err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// promoteCommand re-posts the SAME Slack payload that was saved during
// the last run/regen to the prod webhook. The user workflow is:
//
//  1. `briefing run --target test` (or `regen --media-only --target test`)
//     — generates + POSTs to test webhook + persists payload snapshot
//  2. Reviewer inspects the test channel, confirms the content is good
//  3. `briefing promote --date YYYY-MM-DD` — reads the snapshot and
//     POSTs the EXACT same bytes to the prod webhook
//
// This guarantees test and prod see identical blocks, even though the
// underlying pipeline (LLM, mediaextract, etc.) is non-deterministic
// between invocations.
func promoteCommand(ctx context.Context, cfg *config.Config, date time.Time, gf *globalFlags) error {
	path := payloadSnapshotPath(date)
	payload, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read snapshot %s: %w (did you run `briefing run` / `briefing regen` first?)", path, err)
	}
	if len(payload) == 0 {
		return fmt.Errorf("snapshot %s is empty", path)
	}
	prodURL := cfg.Slack.ProdWebhook
	if prodURL == "" {
		return errors.New("cfg.Slack.ProdWebhook is empty — set SLACK_PROD_WEBHOOK in secrets.env")
	}

	fmt.Printf("[%s] promote: re-posting snapshot %s (%d bytes) to Slack prod webhook\n",
		time.Now().Format("15:04:05"), path, len(payload))

	if gf.dryRun {
		fmt.Println("dry-run: not posting to prod")
		fmt.Println(string(payload))
		return nil
	}

	s, err := store.New("data/briefing.db")
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer s.Close()

	issue, err := s.GetIssueByDate(ctx, gf.domain, date)
	if err != nil || issue == nil {
		// The issue row is only used to record a Delivery log entry.
		// If it is missing we still want to be able to promote, so we
		// fall back to issueID 0 — the Slack POST itself does not need
		// it.
		fmt.Printf("[WARN] get issue for delivery log: %v (proceeding with issueID=0)\n", err)
	}
	var issueID int64
	if issue != nil {
		issueID = issue.ID
	}

	delivery := postSlackPayload(ctx, store.ChannelSlackProd, prodURL, payload, issueID)
	if s != nil && issue != nil {
		if err := s.InsertDelivery(ctx, delivery); err != nil {
			fmt.Printf("[WARN] insert prod delivery: %v\n", err)
		}
	}
	if delivery.Status != store.DeliveryStatusSent {
		return fmt.Errorf("slack prod publish failed: %s", delivery.ResponseJSON)
	}
	fmt.Printf("[%s] promote: slack prod OK\n", time.Now().Format("15:04:05"))

	// 与 run.go:1074 对齐: dry-run + promote 工作流下, run.go 的 dry-run 分支
	// 提前 return, MarkIssuePublished 永远到不了; 必须在 promote 推送成功后
	// 补标. 否则 issues.status 卡在 'generated', 周报 ListDailyIssuesByDateRange
	// (status=published OR published_at IS NOT NULL) 会漏掉这天.
	// fail-soft: Slack/飞书 已推, 标 published 失败只 warn, 不让 post-run.sh
	// 误判成 prod 推送失败. orchestrator selfheal 兜底.
	if issue != nil {
		if err := s.MarkIssuePublished(ctx, issue.ID); err != nil {
			fmt.Printf("[WARN] promote: mark issue published: %v\n", err)
		}
	}

	// #4: promote 加飞书推送 — 跟 run.go 的 prod publish 对齐.
	// fail-soft: 飞书失败只 warn 不阻断 promote.
	if issue != nil {
		insight, ierr := s.GetIssueInsight(ctx, issue.ID)
		if ierr != nil {
			fmt.Printf("[WARN] promote: get insight for feishu: %v\n", ierr)
		}
		reportURL := buildReportURL(date)
		publishDailyToFeishu(ctx, insight, issue.Summary, render.FormatDateZH(issue), reportURL)
	} else {
		fmt.Println("[WARN] promote: no issue row, skipping feishu push")
	}

	// 2026-04-22: dry-run + promote 新工作流下, run.go 的 appendSentURLs/Titles
	// 在 dry-run gate 之后被跳过, 必须在 promote 成功后补上, 否则下一天
	// pipeline 会重推今天已推的 URL/title. fail-soft: 写入失败只 warn.
	if issue != nil {
		items, ierr := s.ListIssueItemsByStatus(ctx, issue.ID, "validated")
		if ierr != nil {
			fmt.Printf("[WARN] promote: list items for dedup persist: %v\n", ierr)
		} else {
			if urls := collectIssueItemSourceURLs(items); len(urls) > 0 {
				appendSentURLs(urls)
				fmt.Printf("[%s] promote: persisted %d URLs to sent set\n",
					time.Now().Format("15:04:05"), len(urls))
			}
			var titles []string
			for _, it := range items {
				if it != nil && strings.TrimSpace(it.Title) != "" {
					titles = append(titles, it.Title)
				}
			}
			if len(titles) > 0 {
				appendSentTitles(titles)
				fmt.Printf("[%s] promote: persisted %d titles to sent set\n",
					time.Now().Format("15:04:05"), len(titles))
			}
		}
	}

	return nil
}

func promoteWeeklyCommand(ctx context.Context, cfg *config.Config, date time.Time, gf *globalFlags) error {
	path := weeklyPayloadSnapshotPath(date)
	payload, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read weekly snapshot %s: %w (did you run `briefing weekly` first?)", path, err)
	}
	if len(payload) == 0 {
		return fmt.Errorf("weekly snapshot %s is empty", path)
	}
	prodURL := cfg.Slack.ProdWebhook
	if prodURL == "" {
		return errors.New("cfg.Slack.ProdWebhook is empty — set SLACK_PROD_WEBHOOK in secrets.env")
	}
	fmt.Printf("[%s] promote-weekly: re-posting snapshot %s (%d bytes) to Slack prod webhook\n",
		time.Now().Format("15:04:05"), path, len(payload))
	if gf.dryRun {
		fmt.Println("dry-run: not posting weekly snapshot to prod")
		fmt.Println(string(payload))
		return nil
	}
	delivery := postSlackPayload(ctx, store.ChannelSlackProd, prodURL, payload, 0)
	if delivery.Status != store.DeliveryStatusSent {
		return fmt.Errorf("weekly slack prod publish failed: %s", delivery.ResponseJSON)
	}
	fmt.Printf("[%s] promote-weekly: slack prod OK\n", time.Now().Format("15:04:05"))

	// 与 promoteCommand (daily) 对齐: 推送成功后 mark weekly_issues.status='published'.
	// fail-soft: Slack 已推, mark 失败只 warn.
	s, err := store.New("data/briefing.db")
	if err != nil {
		fmt.Printf("[WARN] promote-weekly: open store for mark published: %v\n", err)
		return nil
	}
	defer s.Close()
	isoYear, isoWeek := date.ISOWeek()
	weekly, err := s.GetWeeklyIssue(ctx, gf.domain, isoYear, isoWeek)
	if err != nil || weekly == nil {
		fmt.Printf("[WARN] promote-weekly: get weekly row for %d-W%02d: %v\n", isoYear, isoWeek, err)
		return nil
	}
	if err := s.MarkWeeklyPublished(ctx, weekly.ID); err != nil {
		fmt.Printf("[WARN] promote-weekly: mark weekly published: %v\n", err)
	}
	return nil
}

func promoteFeishuCommand(ctx context.Context, cfg *config.Config, date time.Time, gf *globalFlags) error {
	path := dailyFeishuSnapshotPath(date)
	return replayFeishuSnapshot(ctx, path, "promote-feishu", gf.dryRun)
}

func promoteWeeklyFeishuCommand(ctx context.Context, cfg *config.Config, date time.Time, gf *globalFlags) error {
	path := weeklyFeishuSnapshotPath(date)
	return replayFeishuSnapshot(ctx, path, "promote-weekly-feishu", gf.dryRun)
}

func replayFeishuSnapshot(ctx context.Context, path, label string, dryRun bool) error {
	cardBytes, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read feishu snapshot %s: %w", path, err)
	}
	if len(cardBytes) == 0 {
		return fmt.Errorf("feishu snapshot %s is empty", path)
	}
	appID := os.Getenv("FEISHU_APP_ID")
	appSecret := os.Getenv("FEISHU_APP_SECRET")
	chatID := os.Getenv("FEISHU_CHAT_ID")
	if appID == "" || appSecret == "" || chatID == "" {
		return errors.New("feishu credentials are empty — set FEISHU_APP_ID / FEISHU_APP_SECRET / FEISHU_CHAT_ID in secrets.env")
	}
	var card map[string]any
	if err := json.Unmarshal(cardBytes, &card); err != nil {
		return fmt.Errorf("parse feishu snapshot %s: %w", path, err)
	}
	fmt.Printf("[%s] %s: re-posting snapshot %s to Feishu chat\n",
		time.Now().Format("15:04:05"), label, path)
	if dryRun {
		fmt.Println("dry-run: not posting feishu snapshot")
		fmt.Println(string(cardBytes))
		return nil
	}
	token, err := feishuGetToken(appID, appSecret)
	if err != nil {
		return err
	}
	if err := feishuPostCard(ctx, token, chatID, card); err != nil {
		return err
	}
	fmt.Printf("[%s] %s: Feishu post OK\n", time.Now().Format("15:04:05"), label)
	return nil
}

// statusCommand prints the current issue, per-section item status, and recent
// deliveries for the given date. v1.0.1 Batch 2.17: 运维可视化, 便于在
// briefing repair / 故障排查时快速看清哪些 section validated / failed / pending.
func statusCommand(ctx context.Context, cfg *config.Config, date time.Time, gf *globalFlags) error {
	dbPath := os.Getenv("BRIEFING_DB")
	if dbPath == "" {
		dbPath = "data/briefing.db"
	}
	s, err := store.New(dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer s.Close()

	issue, err := s.GetIssueByDate(ctx, gf.domain, date)
	if err != nil {
		return fmt.Errorf("get issue: %w", err)
	}
	if issue == nil {
		fmt.Printf("status: no issue for date=%s domain=%s (run `briefing run` first)\n",
			date.Format("2006-01-02"), gf.domain)
		return nil
	}

	fmt.Printf("=== Issue ===\n")
	fmt.Printf("  id          %d\n", issue.ID)
	fmt.Printf("  date        %s\n", issue.IssueDate.Format("2006-01-02"))
	fmt.Printf("  domain      %s\n", issue.DomainID)
	fmt.Printf("  status      %s\n", issue.Status)
	fmt.Printf("  title       %s\n", issue.Title)
	fmt.Printf("  item_count  %d\n", issue.ItemCount)
	if issue.GeneratedAt != nil {
		fmt.Printf("  generated   %s\n", issue.GeneratedAt.Format("2006-01-02 15:04:05"))
	}
	if issue.PublishedAt != nil {
		fmt.Printf("  published   %s\n", issue.PublishedAt.Format("2006-01-02 15:04:05"))
	}

	// Per-section status breakdown (iterate known statuses so we see both
	// validated items and pending/failed ones if the pipeline hit errors).
	fmt.Printf("\n=== Items by status ===\n")
	for _, st := range []string{"validated", "pending", "running", "failed", "degraded"} {
		items, ierr := s.ListIssueItemsByStatus(ctx, issue.ID, st)
		if ierr != nil {
			fmt.Printf("  %-10s  (query error: %v)\n", st, ierr)
			continue
		}
		if len(items) == 0 {
			continue
		}
		// Bucket by section for readable output.
		bySection := make(map[string]int)
		for _, it := range items {
			if it != nil {
				bySection[it.Section]++
			}
		}
		parts := make([]string, 0, len(bySection))
		for sec, n := range bySection {
			parts = append(parts, fmt.Sprintf("%s=%d", sec, n))
		}
		fmt.Printf("  %-10s  %d items  [%s]\n", st, len(items), strings.Join(parts, ", "))
	}

	// Recent deliveries (uses existing ListDeliveries interface).
	fmt.Printf("\n=== Recent deliveries (same issue) ===\n")
	if deliveries, derr := s.ListDeliveries(ctx, issue.ID); derr == nil {
		if len(deliveries) == 0 {
			fmt.Printf("  (none)\n")
		}
		for _, d := range deliveries {
			if d == nil {
				continue
			}
			fmt.Printf("  %-12s %s  @ %s\n", d.Channel, d.Status, d.SentAt.Format("2006-01-02 15:04:05"))
		}
	} else {
		fmt.Printf("  (deliveries query failed: %v)\n", derr)
	}

	return nil
}
