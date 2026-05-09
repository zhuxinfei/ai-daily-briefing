// sqlite_batch1_test.go — unit tests for the v1.0.1 critical fix
// (Batch 1 data-layer changes). These tests validate:
//
//   Bug K: UpsertIssue must NOT regress status from 'published' back
//          to 'generated' when a later pipeline rerun upserts the
//          same (domain, date) row.
//
//   Bug K'' (corollary): UpsertIssue must NOT regress published_at
//          from a non-NULL timestamp back to NULL on such a rerun.
//
//   SelfHealPublishStatus: orchestrator pre-flight can repair a
//          corrupted row iff there is delivery evidence on slack_prod.
//
//   UpsertIssueItemBySection: per-section DELETE+INSERT leaves other
//          sections intact.
//
//   RecoverStaleRunningStages: stale 'running' rows get flipped to
//          'failed' so a crashed run does not block the next attempt.
//
// Run with: go test ./internal/store/ -run NewLogic -v

package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestStore spins up a fresh SQLite DB in a temp dir and runs
// migrations, returning a migrated Store. The DB is auto-cleaned via
// t.TempDir. Ctx is the caller's context.
func newTestStore(t *testing.T, ctx context.Context) Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("New(%s): %v", dbPath, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// Seed a domain so FK constraints do not blow up.
	if err := s.UpsertDomain(ctx, &Domain{ID: "ai", Name: "AI"}); err != nil {
		t.Fatalf("UpsertDomain: %v", err)
	}
	return s
}

// seedIssueDay returns today's date at midnight UTC for deterministic
// re-use across assertions.
func seedIssueDay() time.Time {
	return time.Date(2026, 4, 14, 0, 0, 0, 0, time.UTC)
}

// TestNewLogic_UpsertIssue_PreservesPublishedStatus is the headline
// regression test for Bug K (2026-04-14). Before the fix, a rerun of
// the pipeline after a successful publish would reset status from
// 'published' to 'generated', corrupting the state machine. This test
// walks that exact scenario and asserts status survives.
func TestNewLogic_UpsertIssue_PreservesPublishedStatus(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, ctx)

	day := seedIssueDay()
	publishedAt := time.Date(2026, 4, 14, 0, 10, 14, 0, time.UTC)

	// Step 1: an 08:10 run publishes the issue (simulating the real
	// production code path).
	id1, err := s.UpsertIssue(ctx, &Issue{
		DomainID:    "ai",
		IssueDate:   day,
		Title:       "AI Daily — 2026-04-14",
		Status:      IssueStatusPublished,
		PublishedAt: &publishedAt,
		GeneratedAt: ptrTime(time.Date(2026, 4, 14, 0, 8, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatalf("first UpsertIssue: %v", err)
	}
	if id1 == 0 {
		t.Fatalf("expected non-zero id")
	}

	// Step 2: a later rerun (e.g. accidental retry, regen, manual
	// re-ingest) attempts to upsert the same (domain, date) with
	// Status='generated'. Before the Bug K fix this overwrote the
	// published row.
	id2, err := s.UpsertIssue(ctx, &Issue{
		DomainID:  "ai",
		IssueDate: day,
		Title:     "AI Daily — 2026-04-14 (rerun)",
		Status:    IssueStatusGenerated,
	})
	if err != nil {
		t.Fatalf("second UpsertIssue: %v", err)
	}
	if id2 != id1 {
		t.Errorf("upsert must preserve id: first=%d second=%d", id1, id2)
	}

	// Step 3: read back and assert status is still 'published' and
	// published_at did not regress to NULL.
	got, err := s.GetIssueByDate(ctx, "ai", day)
	if err != nil {
		t.Fatalf("GetIssueByDate: %v", err)
	}
	if got == nil {
		t.Fatalf("issue not found after upsert")
	}
	if got.Status != IssueStatusPublished {
		t.Errorf("status regressed: want=%q got=%q", IssueStatusPublished, got.Status)
	}
	if got.PublishedAt == nil {
		t.Errorf("published_at regressed to NULL after rerun")
	}
	// Title is allowed to update (it is not state-machine data).
	if !strings.Contains(got.Title, "rerun") {
		t.Errorf("expected title to reflect latest upsert, got %q", got.Title)
	}
}

// TestNewLogic_UpsertIssue_DraftCanBePromotedToGenerated verifies the
// clamp only triggers on 'published'. Draft/generated can still flow
// forward normally.
func TestNewLogic_UpsertIssue_DraftCanBePromotedToGenerated(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, ctx)
	day := seedIssueDay()

	if _, err := s.UpsertIssue(ctx, &Issue{
		DomainID:  "ai",
		IssueDate: day,
		Status:    IssueStatusDraft,
	}); err != nil {
		t.Fatalf("draft upsert: %v", err)
	}
	if _, err := s.UpsertIssue(ctx, &Issue{
		DomainID:  "ai",
		IssueDate: day,
		Status:    IssueStatusGenerated,
	}); err != nil {
		t.Fatalf("generated upsert: %v", err)
	}
	got, err := s.GetIssueByDate(ctx, "ai", day)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Status != IssueStatusGenerated {
		t.Errorf("forward promotion blocked: got=%q", got.Status)
	}
}

// TestNewLogic_SelfHealPublishStatus verifies the orchestrator
// pre-flight repair path: when a row has been corrupted (status set
// to 'generated' even though slack_prod delivery succeeded), calling
// SelfHealPublishStatus flips it back to 'published'.
func TestNewLogic_SelfHealPublishStatus(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, ctx)
	day := seedIssueDay()

	// 1. issue reaches 'generated' after a rerun (what Bug K leaves
	//    behind).
	id, err := s.UpsertIssue(ctx, &Issue{
		DomainID:  "ai",
		IssueDate: day,
		Status:    IssueStatusGenerated,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// 2. but deliveries table shows slack_prod succeeded earlier.
	if err := s.InsertDelivery(ctx, &Delivery{
		IssueID: id,
		Channel: ChannelSlackProd,
		Status:  DeliveryStatusSent,
	}); err != nil {
		t.Fatalf("insert delivery: %v", err)
	}
	n, err := s.SelfHealPublishStatus(ctx, "ai", day)
	if err != nil {
		t.Fatalf("self-heal: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 row healed, got %d", n)
	}
	got, err := s.GetIssueByDate(ctx, "ai", day)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Status != IssueStatusPublished {
		t.Errorf("self-heal failed: status=%q", got.Status)
	}
	if got.PublishedAt == nil {
		t.Errorf("self-heal did not backfill published_at")
	}

	// 3. a second call is a no-op (idempotency).
	n2, err := s.SelfHealPublishStatus(ctx, "ai", day)
	if err != nil {
		t.Fatalf("self-heal 2: %v", err)
	}
	if n2 != 0 {
		t.Errorf("expected idempotent no-op, got %d rows", n2)
	}
}

// TestNewLogic_UpsertIssueItemBySection verifies per-section upsert
// isolates touched sections.
func TestNewLogic_UpsertIssueItemBySection(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, ctx)
	day := seedIssueDay()

	id, err := s.UpsertIssue(ctx, &Issue{
		DomainID:  "ai",
		IssueDate: day,
		Status:    IssueStatusGenerated,
	})
	if err != nil {
		t.Fatalf("upsert issue: %v", err)
	}

	// seed product_update + social
	items := []*IssueItem{
		{IssueID: id, Section: SectionProductUpdate, Seq: 1, Title: "P1", BodyMD: "p1"},
		{IssueID: id, Section: SectionProductUpdate, Seq: 2, Title: "P2", BodyMD: "p2"},
		{IssueID: id, Section: SectionSocial, Seq: 1, Title: "S1", BodyMD: "s1"},
	}
	if err := s.ReplaceIssueItemsBySections(ctx, id, items); err != nil {
		t.Fatalf("replace by sections: %v", err)
	}

	// now rerun compose only for product_update
	newP := []*IssueItem{
		{IssueID: id, Section: SectionProductUpdate, Seq: 1, Title: "P1-v2", BodyMD: "p1v2"},
	}
	if err := s.UpsertIssueItemBySection(ctx, id, SectionProductUpdate, newP); err != nil {
		t.Fatalf("upsert product_update: %v", err)
	}

	all, err := s.ListIssueItems(ctx, id)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	countProduct, countSocial := 0, 0
	for _, it := range all {
		if it.Section == SectionProductUpdate {
			countProduct++
			if !strings.Contains(it.Title, "-v2") {
				t.Errorf("expected refreshed product_update title, got %q", it.Title)
			}
		}
		if it.Section == SectionSocial {
			countSocial++
		}
	}
	if countProduct != 1 {
		t.Errorf("product_update: expected 1 after rerun (was 2), got %d", countProduct)
	}
	if countSocial != 1 {
		t.Errorf("social: expected 1 (unchanged), got %d", countSocial)
	}
}

// TestNewLogic_RecoverStaleRunningStages verifies stale 'running'
// rows get recovered to 'failed' while recent rows are left alone.
func TestNewLogic_RecoverStaleRunningStages(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, ctx)
	day := seedIssueDay()

	id, err := s.UpsertIssue(ctx, &Issue{
		DomainID:  "ai",
		IssueDate: day,
		Status:    IssueStatusGenerated,
	})
	if err != nil {
		t.Fatalf("upsert issue: %v", err)
	}

	// Insert a stale row with started_at 30 minutes ago.
	stale := time.Now().UTC().Add(-30 * time.Minute)
	if err := s.UpdateStageStatus(ctx, &IssueStage{
		IssueID:   id,
		Stage:     StageCompose,
		Status:    StageStatusRunning,
		StartedAt: stale,
		Version:   1,
	}); err != nil {
		t.Fatalf("insert stale stage: %v", err)
	}
	// Insert a fresh running row (1 minute ago).
	fresh := time.Now().UTC().Add(-1 * time.Minute)
	if err := s.UpdateStageStatus(ctx, &IssueStage{
		IssueID:   id,
		Stage:     StageInsight,
		Status:    StageStatusRunning,
		StartedAt: fresh,
		Version:   1,
	}); err != nil {
		t.Fatalf("insert fresh stage: %v", err)
	}

	n, err := s.RecoverStaleRunningStages(ctx, 10*time.Minute)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 stale row, recovered %d", n)
	}
}

// TestNewLogic_InsertStubIssueItems_Idempotent verifies stubs are
// only inserted when no row exists for the (issue, section) tuple.
func TestNewLogic_InsertStubIssueItems_Idempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, ctx)
	day := seedIssueDay()

	id, err := s.UpsertIssue(ctx, &Issue{
		DomainID:  "ai",
		IssueDate: day,
		Status:    IssueStatusDraft,
	})
	if err != nil {
		t.Fatalf("upsert issue: %v", err)
	}

	sections := []string{
		SectionProductUpdate, SectionResearch, SectionIndustry,
		SectionOpenSource, SectionSocial,
	}
	if err := s.InsertStubIssueItems(ctx, id, sections); err != nil {
		t.Fatalf("insert stubs: %v", err)
	}

	// Insert a real row for product_update so the stub is "replaced".
	if err := s.UpsertIssueItemBySection(ctx, id, SectionProductUpdate, []*IssueItem{
		{IssueID: id, Section: SectionProductUpdate, Seq: 1, Title: "real", BodyMD: "real"},
	}); err != nil {
		t.Fatalf("upsert product: %v", err)
	}

	// Second stub call must NOT re-stub product_update (real row would
	// otherwise be nuked).
	if err := s.InsertStubIssueItems(ctx, id, sections); err != nil {
		t.Fatalf("second insert stubs: %v", err)
	}

	all, err := s.ListIssueItems(ctx, id)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	gotSections := map[string]int{}
	for _, it := range all {
		gotSections[it.Section]++
	}
	// product_update has exactly 1 real row.
	if n := gotSections[SectionProductUpdate]; n != 1 {
		t.Errorf("product_update: expected 1 real row, got %d", n)
	}
	// Every other section has exactly 1 stub.
	for _, sec := range sections {
		if sec == SectionProductUpdate {
			continue
		}
		if n := gotSections[sec]; n != 1 {
			t.Errorf("%s: expected 1 stub row, got %d", sec, n)
		}
	}
}

// TestNewLogic_MarkInsightValidated verifies the per-stage state
// update on issue_insights.
func TestNewLogic_MarkInsightValidated(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, ctx)
	day := seedIssueDay()
	id, _ := s.UpsertIssue(ctx, &Issue{
		DomainID: "ai", IssueDate: day, Status: IssueStatusDraft,
	})
	if err := s.UpsertIssueInsight(ctx, &IssueInsight{
		IssueID:    id,
		IndustryMD: "...",
		OurMD:      "...",
		Status:     SectionStatusRunning,
	}); err != nil {
		t.Fatalf("upsert insight: %v", err)
	}
	if err := s.MarkInsightValidated(ctx, id, SectionStatusValidated); err != nil {
		t.Fatalf("mark validated: %v", err)
	}
	got, err := s.GetIssueInsight(ctx, id)
	if err != nil {
		t.Fatalf("get insight: %v", err)
	}
	if got.Status != SectionStatusValidated {
		t.Errorf("status: want=validated got=%q", got.Status)
	}
	if got.InfocardStatus != SectionStatusValidated {
		t.Errorf("infocard_status: want=validated got=%q", got.InfocardStatus)
	}
	if got.ValidatedAt == nil || got.ValidatedAt.IsZero() {
		t.Errorf("validated_at was not stamped")
	}
}

// TestNewLogic_ClassifiedItemsRoundTrip verifies insert + list of
// the new persisted classify output table.
func TestNewLogic_ClassifiedItemsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, ctx)
	day := seedIssueDay()
	id, _ := s.UpsertIssue(ctx, &Issue{
		DomainID: "ai", IssueDate: day, Status: IssueStatusDraft,
	})
	// seed a raw_item so FK is satisfied
	if _, err := s.UpsertSource(ctx, &Source{
		DomainID: "ai", Type: "rss", Name: "test-src",
		ConfigJSON: "{}", Enabled: true,
	}); err != nil {
		t.Fatalf("upsert source: %v", err)
	}
	// Get a source id
	srcs, _ := s.ListEnabledSources(ctx, "ai")
	if len(srcs) == 0 {
		t.Fatalf("no source after upsert")
	}
	if err := s.InsertRawItems(ctx, []*RawItem{
		{DomainID: "ai", SourceID: srcs[0].ID, URL: "https://x/1", Title: "raw1"},
	}); err != nil {
		t.Fatalf("insert raw items: %v", err)
	}
	// Look up the raw_item id
	raws, _ := s.ListRecentRawItems(ctx, "ai", day.Add(-24*time.Hour))
	if len(raws) == 0 {
		t.Fatalf("no raw item after insert")
	}
	rawID := raws[0].ID

	if err := s.InsertClassifiedItems(ctx, []*ClassifiedItem{
		{IssueID: id, Section: SectionProductUpdate, RawItemID: rawID, RankScore: 0.8, Seq: 1},
	}); err != nil {
		t.Fatalf("insert classified: %v", err)
	}
	got, err := s.ListClassifiedItems(ctx, id)
	if err != nil {
		t.Fatalf("list classified: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 classified item, got %d", len(got))
	}
	if got[0].RankScore != 0.8 {
		t.Errorf("score: want 0.8 got %v", got[0].RankScore)
	}
	// idempotent upsert: same key, different seq
	if err := s.InsertClassifiedItems(ctx, []*ClassifiedItem{
		{IssueID: id, Section: SectionProductUpdate, RawItemID: rawID, RankScore: 0.9, Seq: 2},
	}); err != nil {
		t.Fatalf("second insert classified: %v", err)
	}
	got, _ = s.ListClassifiedItems(ctx, id)
	if len(got) != 1 {
		t.Errorf("upsert produced duplicates: %d rows", len(got))
	}
	if got[0].Seq != 2 || got[0].RankScore != 0.9 {
		t.Errorf("upsert did not update: seq=%d score=%v", got[0].Seq, got[0].RankScore)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
