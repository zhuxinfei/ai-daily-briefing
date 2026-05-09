package store

import (
	"context"
	"time"
)

// Store is the data access interface. Concrete implementation must be
// injection-friendly and context-aware. Errors should be wrapped with
// meaningful context so callers can distinguish connection errors from
// not-found vs constraint violations.
//
// Day 1 has a single SQLite implementation. Schema is initialized via Migrate().
type Store interface {
	// Lifecycle
	Migrate(ctx context.Context) error
	Close() error

	// Domain
	UpsertDomain(ctx context.Context, d *Domain) error
	GetDomain(ctx context.Context, id string) (*Domain, error)

	// Source
	UpsertSource(ctx context.Context, s *Source) (int64, error)
	ListEnabledSources(ctx context.Context, domainID string) ([]*Source, error)

	// RawItem
	// InsertRawItems inserts items in bulk; duplicates (source_id, external_id)
	// must be silently skipped (ON CONFLICT DO NOTHING).
	InsertRawItems(ctx context.Context, items []*RawItem) error
	ListRecentRawItems(ctx context.Context, domainID string, since time.Time) ([]*RawItem, error)
	UpdateRawItemContent(ctx context.Context, id int64, content string) error

	// Issue
	// UpsertIssue inserts or updates an issue for (domain_id, issue_date),
	// returning the resulting id.
	UpsertIssue(ctx context.Context, issue *Issue) (int64, error)
	GetIssueByDate(ctx context.Context, domainID string, date time.Time) (*Issue, error)
	MarkIssuePublished(ctx context.Context, issueID int64) error
	NextIssueNumber(ctx context.Context, domainID string) (int, error)

	// IssueItem
	ReplaceIssueItems(ctx context.Context, issueID int64, items []*IssueItem) error
	ListIssueItems(ctx context.Context, issueID int64) ([]*IssueItem, error)
	ListIssueItemsByIssueIDs(ctx context.Context, ids []int64) (map[int64][]*IssueItem, error)

	// IssueItem — per-section state tracking (v1.0.1 critical fix).
	//
	// UpsertIssueItemBySection deletes every existing item for the given
	// (issue, section) tuple, then inserts the provided items. Unlike
	// ReplaceIssueItems, other sections' items are untouched — this is
	// the building block for `briefing repair --section X`.
	UpsertIssueItemBySection(ctx context.Context, issueID int64, section string, items []*IssueItem) error
	// ReplaceIssueItemsBySections is the multi-section wrapper: it
	// groups items by Section and invokes UpsertIssueItemBySection for
	// each distinct section. Sections NOT present in `items` are left
	// alone. Replaces the old ReplaceIssueItems at call sites that need
	// "write back the result of compose/infocard per-section" semantics.
	ReplaceIssueItemsBySections(ctx context.Context, issueID int64, items []*IssueItem) error
	// InsertStubIssueItems inserts placeholder rows (status='pending')
	// for every section we plan to fill in this run, so gate/repair can
	// tell the difference between "not attempted" and "attempted & failed".
	InsertStubIssueItems(ctx context.Context, issueID int64, sections []string) error
	// ListIssueItemsByStatus returns items filtered by status; callers
	// that render to Slack/markdown should always pass "validated" so
	// half-written content never escapes.
	ListIssueItemsByStatus(ctx context.Context, issueID int64, status string) ([]*IssueItem, error)
	// UpdateIssueItemStatus patches one item's status (and bumps
	// retry_count when a prior attempt failed). validatedAt is set to
	// CURRENT_TIMESTAMP iff status == "validated".
	UpdateIssueItemStatus(ctx context.Context, itemID int64, status string) error

	// IssueInsight
	UpsertIssueInsight(ctx context.Context, insight *IssueInsight) error
	GetIssueInsight(ctx context.Context, issueID int64) (*IssueInsight, error)
	ListIssueInsightsByIssueIDs(ctx context.Context, ids []int64) (map[int64]*IssueInsight, error)

	// IssueInsight — per-stage state tracking.
	//
	// MarkInsightValidated flips status to 'validated' and sets
	// validated_at = CURRENT_TIMESTAMP. infocardStatus is an independent
	// dimension (rendered image set) and is updated in the same call.
	MarkInsightValidated(ctx context.Context, issueID int64, infocardStatus string) error

	// ClassifiedItem — persisted classify output for compose-only reruns.
	InsertClassifiedItems(ctx context.Context, items []*ClassifiedItem) error
	ListClassifiedItems(ctx context.Context, issueID int64) ([]*ClassifiedItem, error)

	// Stage ledger — every pipeline stage writes a row here so
	// `briefing status` can surface progress and `briefing repair` can
	// determine what to re-run.
	UpdateStageStatus(ctx context.Context, stage *IssueStage) error
	// RecoverStaleRunningStages marks every stage that has been in
	// 'running' state for longer than `threshold` as 'failed'. Called on
	// orchestrator start-up so a crashed pipeline doesn't block re-runs.
	RecoverStaleRunningStages(ctx context.Context, threshold time.Duration) (int64, error)

	// LLM audit log.
	InsertLLMCall(ctx context.Context, call *LLMCall) error

	// SelfHealPublishStatus repairs an issue row whose `status` was
	// reset from 'published' to 'generated' by a careless rerun, iff we
	// have deliveries evidence that slack_prod already accepted the push.
	// Returns the number of rows updated (0 or 1).
	SelfHealPublishStatus(ctx context.Context, domainID string, date time.Time) (int64, error)

	// WeeklyIssue
	UpsertWeeklyIssue(ctx context.Context, w *WeeklyIssue) (int64, error)
	GetWeeklyIssue(ctx context.Context, domainID string, year, week int) (*WeeklyIssue, error)
	// MarkWeeklyPublished sets weekly_issues.status='published' and
	// published_at=now (idempotent COALESCE). 与 MarkIssuePublished 对称.
	MarkWeeklyPublished(ctx context.Context, weeklyID int64) error
	ListDailyIssuesByDateRange(ctx context.Context, domainID string, start, end time.Time) ([]*Issue, error)

	// Delivery
	InsertDelivery(ctx context.Context, delivery *Delivery) error
	ListDeliveries(ctx context.Context, issueID int64) ([]*Delivery, error)

	// SourceHealth — per-source ingest outcome tracking (v1.0.1 Phase 1.3).
	//
	// UpsertSourceHealth records one ingest result. When success=true,
	// last_success_at is set to NOW, consecutive_failures reset to 0.
	// When success=false, last_error_at set, consecutive_failures += 1
	// (the helper reads the old row and increments in-Go to avoid SQL
	// counter acrobatics).
	UpsertSourceHealth(ctx context.Context, sourceID int64, success bool, errorText string, itemCount int) error
	// ListSourceHealth returns all health rows joined to sources table.
	// Intended for `briefing status --sources` operator view.
	ListSourceHealth(ctx context.Context) ([]*SourceHealth, error)
}
