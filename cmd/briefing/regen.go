// cmd/briefing/regen.go — `briefing regen` subcommand.
//
// Rebuilds the info-card PNGs, HTML page, and Slack push for an EXISTING
// issue already persisted in SQLite. It skips ingest → rank → classify →
// compose → insight → summary (the expensive steps) and only re-runs:
//
//	load existing data → infocard (LLM JSON + PIL) → render → publish
//
// Use when you want to iterate on the visual pipeline (template, layout,
// LLM card prompt) without paying for another 7-minute rank pass or
// re-fetching 2000+ raw items.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"briefing-v3/internal/config"
	"briefing-v3/internal/gate"
	"briefing-v3/internal/infocard"
	"briefing-v3/internal/publish"
	"briefing-v3/internal/render"
	"briefing-v3/internal/store"
)

// cardImgMarker matches the ![alt](../data/images/cards/.../item-N.png) lines
// our previous run injected into BodyMD. We strip them before re-injecting
// fresh ones so the body does not accumulate stale image references across
// multiple regen passes.
var cardImgMarker = regexp.MustCompile(`!\[[^\]]*\]\(\.\./data/images/cards/[^)]+\)\s*\n?\n?`)

// anyImgMarker matches ANY ![alt](url) and [[VIDEO:url]] fragment so
// --media-only regen can start from a clean slate, whether the previous
// run was an infocard-style card PNG or an earlier mediaextract pass.
var anyImgMarker = regexp.MustCompile(`(?m)!\[[^\]]*\]\([^)]*\)[ \t]*\n?`)
var anyVideoMarker = regexp.MustCompile(`(?m)\[\[VIDEO:[^\]]*\]\][ \t]*\n?`)

func regenCommand(ctx context.Context, cfg *config.Config, date time.Time, gf *globalFlags) error {
	stage := func(name string) { fmt.Printf("[%s] %s\n", time.Now().Format("15:04:05"), name) }

	stage(fmt.Sprintf("regen start: date=%s domain=%s target=%s",
		date.Format("2006-01-02"), gf.domain, gf.target))

	// --- 0. Open store -----------------------------------------------
	s, err := store.New("data/briefing.db")
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer s.Close()

	// --- 1. Load existing issue --------------------------------------
	issue, err := s.GetIssueByDate(ctx, gf.domain, date)
	if err != nil {
		return fmt.Errorf("get issue: %w", err)
	}
	if issue == nil {
		return fmt.Errorf("no issue row for date=%s domain=%s — run `briefing run` first",
			date.Format("2006-01-02"), gf.domain)
	}
	stage(fmt.Sprintf("loaded issue id=%d status=%s items=%d", issue.ID, issue.Status, issue.ItemCount))

	// v1.0.1 Batch 2.12: regen 只取 validated 的 items, 避免把未验证
	// (pending/failed) 的半成品 section 重新渲染出来 (应该交给 briefing
	// repair 单独补救那些缺的 section).
	issueItems, err := s.ListIssueItemsByStatus(ctx, issue.ID, "validated")
	if err != nil {
		return fmt.Errorf("list issue items: %w", err)
	}
	if len(issueItems) == 0 {
		return errors.New("existing issue has zero validated items — run `briefing repair` or `briefing run` first")
	}
	stage(fmt.Sprintf("loaded %d issue items", len(issueItems)))

	// Strip any leftover image markdown from previous passes so
	// we can re-inject fresh paths without stacking. In --media-only
	// mode we clear ALL images (not just card PNGs) so the pass
	// starts from a clean BodyMD.
	for _, it := range issueItems {
		if it == nil {
			continue
		}
		it.BodyMD = cardImgMarker.ReplaceAllString(it.BodyMD, "")
		if gf.mediaOnly {
			it.BodyMD = anyImgMarker.ReplaceAllString(it.BodyMD, "")
			it.BodyMD = anyVideoMarker.ReplaceAllString(it.BodyMD, "")
			it.BodyMD = strings.TrimSpace(it.BodyMD)
		}
	}

	insight, err := s.GetIssueInsight(ctx, issue.ID)
	if err != nil {
		return fmt.Errorf("get insight: %w", err)
	}
	if insight == nil {
		return errors.New("existing issue has no insight row — run `briefing run` first")
	}
	stage("loaded insight")

	summary := strings.TrimSpace(issue.Summary)
	if summary == "" {
		// Summary row in DB is empty (the run pipeline updates a local
		// Issue struct but the stored column stayed at its zero value
		// when the initial UpsertIssue raced the Summary assignment).
		// Re-ask the LLM for a fresh 3-line summary — it is a single
		// cheap call so regen can still be 10-20x faster than `run`.
		stage("summary: DB summary empty, regenerating")
		fresh, err := generateDailySummary(ctx, cfg.LLM, issueItems)
		if err != nil {
			return fmt.Errorf("regen summary: %w", err)
		}
		summary = strings.TrimSpace(fresh)
		issue.Summary = summary
		if _, err := s.UpsertIssue(ctx, issue); err != nil {
			fmt.Printf("[WARN] persist summary: %v\n", err)
		}
	}

	// --- 2. Visual pass -----------------------------------------------
	// Two modes:
	//   (a) default: full infocard — local PIL L1 header PNG + L2 item
	//       card PNGs, all file-served under data/images/cards/
	//   (b) --media-only: NO local PIL rendering at all. Every image
	//       comes from an on-line source (og:image hotlinks via
	//       mediaextract). Items without a meaningful extractable
	//       image simply stay image-free. This matches the user
	//       preference of "all images online, no local hosting".
	var headerCardPNGRel string

	if gf.mediaOnly {
		stage("media-only: no local PIL render, pulling og:image for each item")
		// Wipe any stale card PNGs from a previous infocard pass so the
		// HTML does not leak orphaned file references.
		cardDir := filepath.Join("data", "images", "cards", date.Format("2006-01-02"))
		if entries, _ := os.ReadDir(cardDir); entries != nil {
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".png") {
					_ = os.Remove(filepath.Join(cardDir, e.Name()))
				}
			}
		}

		found := enrichItemsWithMedia(ctx, issueItems)
		stage(fmt.Sprintf("media-only: %d items got a hero image/video from og:image", found))

		// Persist mutated BodyMD back to SQLite. v1.0.1: use
		// per-section upsert so a re-run touching only a subset of
		// sections does not wipe items from sections it skipped.
		if err := s.ReplaceIssueItemsBySections(ctx, issue.ID, issueItems); err != nil {
			fmt.Printf("[WARN] replace issue items after media-only: %v\n", err)
		}
	} else {
	stage("infocard: calling LLM to re-distill cards")
	icGen, icErr := infocard.New(infocard.Config{
		BaseURL:    cfg.LLM.BaseURL,
		APIKey:     cfg.LLM.APIKey,
		Model:      cfg.LLM.Model,
		MaxRetries: 3,
		Timeout:    cfg.LLM.LLMTimeout(),
	})
	if icErr != nil {
		return fmt.Errorf("infocard new: %w", icErr)
	}

	// Shadow-remap seqs to globally-unique UIDs to avoid per-section
	// collisions clobbering each other's PNG files.
	shadowItems := make([]*store.IssueItem, 0, len(issueItems))
	uidToItem := make(map[int]*store.IssueItem, len(issueItems))
	for i, it := range issueItems {
		if it == nil {
			continue
		}
		shadow := *it
		shadow.Seq = i + 1
		shadowItems = append(shadowItems, &shadow)
		uidToItem[shadow.Seq] = it
	}

	header, cards, err := icGen.Generate(ctx, shadowItems, summary)
	if err != nil {
		return fmt.Errorf("infocard generate: %w", err)
	}
	stage(fmt.Sprintf("infocard: got header + %d cards", len(cards)))
	header.IssueDate = date.Format("2006-01-02")

	cardDir := filepath.Join("data", "images", "cards", date.Format("2006-01-02"))
	if err := os.MkdirAll(cardDir, 0o755); err != nil {
		return fmt.Errorf("mkdir cards: %w", err)
	}
	// Clear any stale PNGs so broken/renamed files cannot leak through.
	if entries, _ := os.ReadDir(cardDir); entries != nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".png") {
				_ = os.Remove(filepath.Join(cardDir, e.Name()))
			}
		}
	}

	// Header card PNG (非阻断).
	headerPath := filepath.Join(cardDir, "header.png")
	if err := renderInfoCardPNG(ctx, "header", header, headerPath); err != nil {
		fmt.Printf("[WARN] infocard header render: %v\n", err)
	} else {
		headerCardPNGRel = fmt.Sprintf("../data/images/cards/%s/header.png", date.Format("2006-01-02"))
		stage(fmt.Sprintf("infocard: header PNG → %s", headerPath))
	}

	// Item cards, each inside recover() so a single failure cannot
	// take down the regen.
	renderedCount := 0
	for _, c := range cards {
		if c == nil {
			continue
		}
		it := uidToItem[c.ItemSeq]
		if it == nil {
			fmt.Printf("[WARN] infocard: card uid=%d has no matching item, skip\n", c.ItemSeq)
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Printf("[WARN] infocard uid=%d panic: %v\n", c.ItemSeq, r)
				}
			}()
			outPath := filepath.Join(cardDir, fmt.Sprintf("item-%d.png", c.ItemSeq))
			if err := renderInfoCardPNG(ctx, "item", c, outPath); err != nil {
				fmt.Printf("[WARN] infocard item uid=%d render: %v\n", c.ItemSeq, err)
				return
			}
			renderedCount++
			relPath := fmt.Sprintf("../data/images/cards/%s/item-%d.png", date.Format("2006-01-02"), c.ItemSeq)
			alt := strings.TrimSpace(c.MainTitle)
			if alt == "" {
				alt = strings.TrimSpace(it.Title)
			}
			for _, ch := range []string{"[", "]", "(", ")"} {
				alt = strings.ReplaceAll(alt, ch, " ")
			}
			alt = strings.TrimSpace(alt)
			imgLine := fmt.Sprintf("![%s](%s)\n\n", alt, relPath)
			it.BodyMD = imgLine + strings.TrimLeft(it.BodyMD, "\n")
		}()
	}
	stage(fmt.Sprintf("infocard: rendered %d/%d item PNGs", renderedCount, len(cards)))

	// Persist mutated BodyMD back to SQLite. v1.0.1: per-section upsert.
	if err := s.ReplaceIssueItemsBySections(ctx, issue.ID, issueItems); err != nil {
		fmt.Printf("[WARN] replace issue items: %v\n", err)
	}
	} // end of: if gf.mediaOnly { ... } else { ... }

	// --- 3. Render markdown + HTML -----------------------------------
	renderSecs := make([]render.SectionMeta, 0, len(cfg.Sections))
	for _, sec := range cfg.Sections {
		renderSecs = append(renderSecs, render.SectionMeta{
			ID:    sec.ID,
			Title: sec.Title,
		})
	}
	fullMarkdown := render.RenderMarkdown(issue, issueItems, insight, renderSecs)
	sectionsMD := render.RenderSectionsMap(issueItems, renderSecs)
	stage(fmt.Sprintf("render: markdown built (%d bytes)", len(fullMarkdown)))
	_ = writeDailyMarkdown(date, fullMarkdown)

	// Only use a local hero PNG when we actually produced one (infocard
	// legacy mode). In --media-only mode headerCardPNGRel is empty and
	// we leave HeadlineImg blank — the HTML template just skips the
	// hero image block, matching the "all images online" user pref.
	headlineRelForHTML := headerCardPNGRel
	if !gf.mediaOnly && headlineRelForHTML == "" && cfg.Image.Enabled {
		headlineRelForHTML = fmt.Sprintf("../data/images/%s.png", date.Format("2006-01-02"))
	}
	// BRIEFING_HERO_URL env var allows the operator to inject a
	// pre-generated hero image URL (e.g. a hubtoday-style info poster
	// that they rendered out of band). When set, it overrides both the
	// HTML hero slot AND the Slack header image block so test+prod see
	// the same poster. The URL is NOT downloaded — it is hot-linked
	// verbatim, keeping the "no local hosting" guarantee.
	heroOverrideURL := strings.TrimSpace(os.Getenv("BRIEFING_HERO_URL"))
	if heroOverrideURL != "" {
		headlineRelForHTML = heroOverrideURL
		stage(fmt.Sprintf("hero: using BRIEFING_HERO_URL override = %s", heroOverrideURL))
	}
	htmlRes, htmlErr := render.WriteIssueHTML("docs", &render.IssueHTMLInput{
		Issue:       issue,
		Items:       issueItems,
		Insight:     insight,
		Sections:    renderSecs,
		HeadlineImg: headlineRelForHTML,
	})
	if htmlErr != nil {
		fmt.Printf("[WARN] html: %v\n", htmlErr)
	} else {
		stage(fmt.Sprintf("html: %s (%d bytes)", htmlRes.Path, htmlRes.Size))
	}
	if indexEntries, err := render.CollectIndexEntries("docs"); err == nil {
		_, _ = render.WriteIndexHTML("docs", indexEntries, "briefing-v3 · 每日早读 · 全网深度聚合")
	}

	// --- 3b. Hextra hugo path (v1.0.0 G 阶段补 regen 对接) ---------------
	// regen 在 T3 D 阶段没有同步对接 hugo path. 缺这一段就会让 hugo.go 的
	// scrub / hero prepend / 三级路径 / drop item-*.png 改动通过 regen
	// 完全失效. 这里复制 run.go 11b/11c 段的逻辑追加进来. 旧的
	// docs/*.html 路径仍然保留 (上面 line ~286), 作为 v1.0.1 之前的
	// rollback safety net.
	if hextraDir := os.Getenv("HEXTRA_SITE_DIR"); hextraDir != "" {
		hugoPath, hugoErr := render.WriteHugoPost(hextraDir, issue, issueItems, insight, renderSecs)
		if hugoErr != nil {
			fmt.Printf("[WARN] hugo write post failed: %v (continuing)\n", hugoErr)
		} else {
			stage(fmt.Sprintf("hugo: wrote %s", hugoPath))
		}
		if hugoBin := os.Getenv("HUGO_BIN"); hugoBin != "" {
			buildCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			cmd := exec.CommandContext(buildCtx, hugoBin, "--source", hextraDir, "--minify")
			// Hextra needs Go on PATH for hugo modules (T1 discovery).
			cmd.Env = append(os.Environ(), "PATH=/usr/local/go/bin:"+os.Getenv("PATH"))
			if out, err := cmd.CombinedOutput(); err != nil {
				fmt.Printf("[WARN] hugo build failed: %v\n%s (continuing)\n", err, string(out))
			} else {
				stage("hugo: build complete")
			}
			cancel()
		}
	}

	// --- 4. Build & publish ------------------------------------------
	reportURL := buildReportURL(date)
	// Slack image_url only accepts JPG/PNG/GIF. If the hero override is
	// AVIF/WEBP (modern formats many sites use now) Slack returns
	// invalid_blocks. Detect the extension and, for Slack, fall back
	// to empty so the Slack block is skipped — the HTML page still
	// shows the image natively via the <img> tag.
	slackHero := ""
	if heroOverrideURL != "" {
		low := strings.ToLower(heroOverrideURL)
		if strings.Contains(low, ".jpg") || strings.Contains(low, ".jpeg") ||
			strings.Contains(low, ".png") || strings.Contains(low, ".gif") {
			slackHero = heroOverrideURL
		}
	}
	rendered := &publish.RenderedIssue{
		Issue:            issue,
		Items:            issueItems,
		Insight:          insight,
		HeadlineImageURL: slackHero,
		SectionsMarkdown: sectionsMD,
		DateZH:           render.FormatDateZH(issue),
		ReportURL:        reportURL,
	}

	stage("gate: checking hard quality rules")
	g := gate.New(gate.Config{
		MinItems:               cfg.Gate.MinItems,
		MinSectionsWithContent: cfg.Gate.MinSectionsWithContent,
		MinInsightChars:        cfg.Gate.MinInsightChars,
		MinIndustryBullets:     cfg.Gate.MinIndustryBullets,
		MaxIndustryBullets:     cfg.Gate.MaxIndustryBullets,
		MinTakeawayBullets:     cfg.Gate.MinTakeawayBullets,
		MaxTakeawayBullets:     cfg.Gate.MaxTakeawayBullets,
		MinSourceDomains:       cfg.Gate.MinSourceDomains,
	})
	// INTERFACE CHANGE (T2/C4): gate.Check() now takes failedSections and
	// totalSections for tri-state (pass/warn/fail) outcome. regen replays an
	// already-published issue so there are no fresh compose failures to
	// report — pass nil and the configured total.
	report := g.Check(issue, issueItems, insight, nil, len(cfg.Sections))
	stage(fmt.Sprintf("gate: pass=%v items=%d sections=%d insightChars=%d domains=%d",
		report.Pass, report.ItemCount, report.SectionCount, report.InsightChars, report.SourceDomainCount))

	slackPayload, err := render.BuildSlackPayload(rendered)
	if err != nil {
		return fmt.Errorf("build slack payload: %w", err)
	}

	// Persist the exact payload bytes we are about to POST. `briefing
	// promote` reads this file to re-post the SAME bytes to the prod
	// webhook later, so that a manual "test OK, promote now" flow does
	// not risk any drift (LLM non-determinism, new og:image etc.).
	if err := savePayloadSnapshot(date, slackPayload); err != nil {
		fmt.Printf("[WARN] save payload snapshot: %v\n", err)
	}

	if gf.dryRun {
		stage("dry-run: skipping publish")
		fmt.Println(string(bytes.TrimSpace(slackPayload)))
		return nil
	}

	stage("publish: posting to Slack test channel")
	testDelivery := postSlackPayload(ctx, store.ChannelSlackTest, cfg.Slack.TestWebhook, slackPayload, issue.ID)
	if err := s.InsertDelivery(ctx, testDelivery); err != nil {
		fmt.Printf("[WARN] insert delivery: %v\n", err)
	}
	if testDelivery.Status != store.DeliveryStatusSent {
		return fmt.Errorf("slack test publish failed: %s", testDelivery.ResponseJSON)
	}
	stage("publish: slack test OK")

	// v1.0.1 Batch 2.6 parity: regen 也要遵守 BRIEFING_MODE=debug 不推 prod 规则
	// (2026-04-14 修复: 之前 regen 漏了这个保护, 有误推风险)
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("BRIEFING_MODE")))
	wantsProdTarget := gf.target == "auto" || gf.target == "prod"
	targetWantsProd := wantsProdTarget && mode == "prod" && report.Pass
	if wantsProdTarget && mode != "prod" {
		stage("publish: BRIEFING_MODE=" + mode + ", skipping prod channel (仅 prod 模式推正式频道)")
	}
	if targetWantsProd {
		stage("publish: posting to Slack prod channel")
		prodDelivery := postSlackPayload(ctx, store.ChannelSlackProd, cfg.Slack.ProdWebhook, slackPayload, issue.ID)
		if err := s.InsertDelivery(ctx, prodDelivery); err != nil {
			fmt.Printf("[WARN] insert prod delivery: %v\n", err)
		}
		if prodDelivery.Status != store.DeliveryStatusSent {
			return fmt.Errorf("slack prod publish failed: %s", prodDelivery.ResponseJSON)
		}
		stage("publish: slack prod OK")

		// #5: 飞书推送 (跟 Slack prod 同条件, fail-soft, 对齐 run.go).
		publishDailyToFeishu(ctx, insight, summary, render.FormatDateZH(issue), reportURL)

		// #8: 标记 issue status=published, 使 post-run.sh 触发 git push.
		if err := s.MarkIssuePublished(ctx, issue.ID); err != nil {
			return fmt.Errorf("mark published: %w", err)
		}
		stage("publish: marked issue published")
	} else if wantsProdTarget && !report.Pass {
		stage("publish: gate failed, skipping prod")
	} else {
		stage("publish: target=test, skipping prod")
	}

	stage("regen complete")
	return nil
}
