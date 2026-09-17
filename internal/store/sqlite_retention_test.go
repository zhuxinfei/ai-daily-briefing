// sqlite_retention_test.go — tests for the state-DB retention (see
// sqliteStore.PruneRawItems for the incident it answers).
//
// The two properties worth pinning: PruneRawItems deletes strictly by
// fetched_at, and Compact actually returns the freed pages to the filesystem —
// a DELETE on its own leaves the file exactly as large as it was.

package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// insertRawItemsAt inserts n items whose fetched_at is the given instant.
// InsertRawItems falls back to "now" for a zero FetchedAt, so it must be set
// explicitly to age a row.
func insertRawItemsAt(t *testing.T, ctx context.Context, s Store, sourceID int64, n int, fetchedAt time.Time, content string) {
	t.Helper()
	items := make([]*RawItem, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%s-%s-%d", fetchedAt.Format("20060102"), content, i)
		items = append(items, &RawItem{
			DomainID:   "ai",
			SourceID:   sourceID,
			ExternalID: id,
			URL:        "https://example.com/" + id,
			Title:      "title",
			FetchedAt:  fetchedAt,
			Content:    content,
		})
	}
	if err := s.InsertRawItems(ctx, items); err != nil {
		t.Fatalf("InsertRawItems: %v", err)
	}
}

// TestMigrate_IndexesClassifiedItemsRawItem pins migration 007. The index is
// invisible — nothing fails without it, pruning just gets slower every day as
// classified_items grows (0.83 s at 1.4k child rows, 7.12 s at 30k). A
// migration that silently did not apply would look exactly like success.
func TestMigrate_IndexesClassifiedItemsRawItem(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, ctx) // runs Migrate

	sqlite, ok := s.(*sqliteStore)
	if !ok {
		t.Fatalf("expected *sqliteStore, got %T", s)
	}
	var name string
	err := sqlite.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='classified_items' AND name=?`,
		"idx_classified_items_raw_item",
	).Scan(&name)
	if err != nil {
		t.Fatalf("index idx_classified_items_raw_item missing after Migrate: %v", err)
	}
}

func countRawItems(t *testing.T, ctx context.Context, s Store) int {
	t.Helper()
	items, err := s.ListRecentRawItems(ctx, "ai", time.Time{})
	if err != nil {
		t.Fatalf("ListRecentRawItems: %v", err)
	}
	return len(items)
}

// TestPruneRawItems_KeepsWindowAndDropsOlder pins the boundary: rows fetched
// before the cutoff go, rows on or after it stay.
func TestPruneRawItems_KeepsWindowAndDropsOlder(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, ctx)
	sid := seedSource(t, ctx, s, "retention-src")

	now := time.Now().UTC()
	old := now.AddDate(0, 0, -40)  // outside a 30-day window
	edge := now.AddDate(0, 0, -29) // inside it

	insertRawItemsAt(t, ctx, s, sid, 5, old, "old")
	insertRawItemsAt(t, ctx, s, sid, 3, edge, "edge")
	if got := countRawItems(t, ctx, s); got != 8 {
		t.Fatalf("precondition: %d raw_items, want 8", got)
	}

	cutoff := now.AddDate(0, 0, -30)
	n, err := s.PruneRawItems(ctx, cutoff)
	if err != nil {
		t.Fatalf("PruneRawItems: %v", err)
	}
	if n != 5 {
		t.Errorf("pruned %d rows, want 5", n)
	}
	if got := countRawItems(t, ctx, s); got != 3 {
		t.Errorf("after prune: %d raw_items, want 3", got)
	}
}

// TestPruneRawItems_NoopWhenNothingIsOld guards the common case: a window that
// covers everything must delete nothing, not everything.
func TestPruneRawItems_NoopWhenNothingIsOld(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, ctx)
	sid := seedSource(t, ctx, s, "retention-src-fresh")

	insertRawItemsAt(t, ctx, s, sid, 4, time.Now().UTC().AddDate(0, 0, -2), "fresh")

	n, err := s.PruneRawItems(ctx, time.Now().UTC().AddDate(0, 0, -30))
	if err != nil {
		t.Fatalf("PruneRawItems: %v", err)
	}
	if n != 0 {
		t.Errorf("pruned %d rows, want 0", n)
	}
	if got := countRawItems(t, ctx, s); got != 4 {
		t.Errorf("%d raw_items left, want 4", got)
	}
}

// TestPruneRawItems_SparesRowsClassifiedItemsReferences pins the constraint
// that the first cut of this prune violated: classified_items.raw_item_id is a
// NOT NULL foreign key onto raw_items, so deleting a referenced row does not
// skip it — it aborts the entire DELETE with "FOREIGN KEY constraint failed"
// and nothing gets pruned at all.
func TestPruneRawItems_SparesRowsClassifiedItemsReferences(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, ctx)
	sid := seedSource(t, ctx, s, "retention-src-fk")

	old := time.Now().UTC().AddDate(0, 0, -60)
	insertRawItemsAt(t, ctx, s, sid, 4, old, "linked")

	// Reference exactly one of them from a classified item.
	rows, err := s.ListRecentRawItems(ctx, "ai", time.Time{})
	if err != nil {
		t.Fatalf("ListRecentRawItems: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("precondition: %d raw_items, want 4", len(rows))
	}
	issueID, err := s.UpsertIssue(ctx, &Issue{DomainID: "ai", IssueDate: time.Now().UTC(), Title: "t"})
	if err != nil {
		t.Fatalf("UpsertIssue: %v", err)
	}
	referenced := rows[0].ID
	if err := s.InsertClassifiedItems(ctx, []*ClassifiedItem{{
		IssueID: issueID, Section: SectionOpenSource, RawItemID: referenced, RankScore: 0.9, Seq: 1,
	}}); err != nil {
		t.Fatalf("InsertClassifiedItems: %v", err)
	}

	n, err := s.PruneRawItems(ctx, time.Now().UTC().AddDate(0, 0, -30))
	if err != nil {
		t.Fatalf("PruneRawItems returned an error instead of sparing the referenced row: %v", err)
	}
	if n != 3 {
		t.Errorf("pruned %d rows, want 3 (the referenced one must survive)", n)
	}

	remaining, err := s.ListRecentRawItems(ctx, "ai", time.Time{})
	if err != nil {
		t.Fatalf("ListRecentRawItems: %v", err)
	}
	if len(remaining) != 1 || remaining[0].ID != referenced {
		t.Errorf("remaining raw_items = %d (want just id=%d)", len(remaining), referenced)
	}
}

// TestCompact_ShrinksFileAfterPrune is the regression that actually matters for
// the incident: the file must get smaller, since the push limit is measured in
// bytes of data/briefing.db, not in rows.
func TestCompact_ShrinksFileAfterPrune(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := s.UpsertDomain(ctx, &Domain{ID: "ai", Name: "AI"}); err != nil {
		t.Fatalf("UpsertDomain: %v", err)
	}
	sid := seedSource(t, ctx, s, "retention-src-size")

	// Enough bulky content that the file grows by megabytes, so the assertion
	// is about real page reclamation rather than noise.
	bulk := make([]byte, 200<<10) // 200 KiB per item
	for i := range bulk {
		bulk[i] = 'x'
	}
	insertRawItemsAt(t, ctx, s, sid, 40, time.Now().UTC().AddDate(0, 0, -60), string(bulk))
	if err := s.Compact(ctx); err != nil {
		t.Fatalf("Compact (grow): %v", err)
	}
	grown, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat grown db: %v", err)
	}

	// Keep a single recent row so the DB is not simply emptied.
	insertRawItemsAt(t, ctx, s, sid, 1, time.Now().UTC(), "keep-me")
	if _, err := s.PruneRawItems(ctx, time.Now().UTC().AddDate(0, 0, -30)); err != nil {
		t.Fatalf("PruneRawItems: %v", err)
	}
	if err := s.Compact(ctx); err != nil {
		t.Fatalf("Compact (shrink): %v", err)
	}

	shrunk, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat shrunk db: %v", err)
	}
	if shrunk.Size() >= grown.Size() {
		t.Errorf("db did not shrink after prune+compact: %d → %d bytes", grown.Size(), shrunk.Size())
	}
	if got := countRawItems(t, ctx, s); got != 1 {
		t.Errorf("after prune+compact: %d raw_items, want the 1 retained row", got)
	}
}
