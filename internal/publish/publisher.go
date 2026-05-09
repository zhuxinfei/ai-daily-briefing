// Package publish defines distribution channel abstractions and implementations.
//
// Each channel has its own file in this package:
//   - slack.go        — Slack webhook (Block Kit payload)
//   - feishu_doc.go   — Feishu wiki doc (year/month/issue hierarchy)
//   - feishu_bot.go   — Feishu group chat message with @all
//
// Publishers are side-effectful but MUST NOT persist Delivery records themselves;
// the main orchestrator records them via Store after observing the returned result.
package publish

import (
	"context"

	"briefing-v3/internal/store"
)

// RenderedIssue bundles an Issue together with its items and insight, ready
// to be formatted by a Publisher for its specific channel.
type RenderedIssue struct {
	Issue   *store.Issue
	Items   []*store.IssueItem // sorted by section, then seq
	Insight *store.IssueInsight

	// HeadlineImageURL is the publicly reachable URL of the generated
	// newspaper-style cover image for today's issue. Empty string means
	// the renderer failed or image generation was disabled; publishers
	// should gracefully omit the image block in that case.
	HeadlineImageURL string

	// SectionsMarkdown maps section ID (e.g. "product_update", "research")
	// to the pre-rendered markdown for that section's body. Populated by
	// internal/render.RenderMarkdown during the render stage so that Slack
	// and other channel publishers don't need to re-parse the full report.
	SectionsMarkdown map[string]string

	// DateZH is today's date in Chinese form, e.g. "2026年4月11日".
	// Published header blocks prefer this over raw YYYY-MM-DD for readability.
	DateZH string

	// ReportURL is a public link to the full daily briefing. Used by the
	// Slack "view full report" button. May be empty when there is no
	// published web view yet; publishers should fall back to a placeholder.
	ReportURL string

	// QualityWarn is true when the upstream gate produced a soft-warn
	// verdict (Pass=false, Warn=true). Slack publishers should surface a
	// "质量待审" marker in the message header so users can tell the
	// briefing was shipped despite some missing signals. Default false.
	//
	// v1.0.0 D7b: introduced to ship tri-state gate results through to
	// the Slack renderer without adding a new cross-package dependency
	// on internal/gate from internal/render.
	QualityWarn bool

	// QualityWarnings is the free-form list of soft-warn reasons the
	// gate emitted, in render order (already deduplicated). When
	// non-empty the Slack footer surfaces them in a context block so
	// operators can glance at why the warn state triggered.
	QualityWarnings []string

	// FailedSections lists the section IDs whose compose stage
	// degraded (LLM call failed and was skipped with continue). Slack
	// renderers display this in the footer context block when
	// non-empty so readers know which sections are missing content.
	FailedSections []string
}

// Publisher sends a RenderedIssue to one distribution channel.
type Publisher interface {
	// Name returns the channel tag used in the deliveries table,
	// e.g. "slack_test", "feishu_doc", "feishu_bot".
	Name() string

	// Publish formats and delivers the issue. It MUST return a *store.Delivery
	// reflecting the attempt (including failures), with SentAt populated.
	// A non-nil error indicates an unrecoverable dispatch failure; the
	// returned Delivery.Status should be "failed" in that case.
	Publish(ctx context.Context, rendered *RenderedIssue) (*store.Delivery, error)
}
