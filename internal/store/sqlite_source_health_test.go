// sqlite_source_health_test.go — unit tests for v1.0.1 Phase 1.3
// source_health monitoring (UpsertSourceHealth + ListSourceHealth).
//
// Scenarios covered:
//   - First upsert on a brand-new source creates row (isNew path)
//   - Success after fail resets consecutive_failures
//   - Repeated failures accumulate consecutive_failures counter
//   - last_success_at is preserved when a failure follows a success
//   - last_error_text captured on failure, cleared on nothing on success
//   - ListSourceHealth orders by consecutive_failures DESC
//
// Run with: go test ./internal/store/ -run SourceHealth -v

package store

import (
	"context"
	"testing"
	"time"
)

// seedSource inserts a minimal source row for FK constraint on
// source_health and returns its id.
func seedSource(t *testing.T, ctx context.Context, s Store, name string) int64 {
	t.Helper()
	id, err := s.UpsertSource(ctx, &Source{
		DomainID:   "ai",
		Type:       "rss",
		Name:       name,
		ConfigJSON: `{"url":"https://example.com/feed"}`,
		Enabled:    true,
	})
	if err != nil {
		t.Fatalf("UpsertSource(%s): %v", name, err)
	}
	return id
}

func TestSourceHealth_FirstUpsertCreatesRow(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, ctx)
	sid := seedSource(t, ctx, s, "test-source-1")

	if err := s.UpsertSourceHealth(ctx, sid, true, "", 10); err != nil {
		t.Fatalf("UpsertSourceHealth: %v", err)
	}

	got, err := s.ListSourceHealth(ctx)
	if err != nil {
		t.Fatalf("ListSourceHealth: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 row, got %d", len(got))
	}
	h := got[0]
	if h.SourceID != sid {
		t.Errorf("SourceID: got %d, want %d", h.SourceID, sid)
	}
	if h.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures: got %d, want 0", h.ConsecutiveFailures)
	}
	if h.LastItemCount != 10 {
		t.Errorf("LastItemCount: got %d, want 10", h.LastItemCount)
	}
	if h.LastSuccessAt == nil {
		t.Errorf("LastSuccessAt: expected non-nil, got nil")
	}
	if h.LastErrorAt != nil {
		t.Errorf("LastErrorAt: expected nil on first success, got %v", h.LastErrorAt)
	}
}

func TestSourceHealth_FailAccumulates(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, ctx)
	sid := seedSource(t, ctx, s, "flaky-source")

	// Three consecutive failures.
	for i := 1; i <= 3; i++ {
		if err := s.UpsertSourceHealth(ctx, sid, false, "boom boom", 0); err != nil {
			t.Fatalf("UpsertSourceHealth fail #%d: %v", i, err)
		}
	}
	got, err := s.ListSourceHealth(ctx)
	if err != nil || len(got) != 1 {
		t.Fatalf("ListSourceHealth: rows=%d err=%v", len(got), err)
	}
	h := got[0]
	if h.ConsecutiveFailures != 3 {
		t.Errorf("ConsecutiveFailures: got %d, want 3", h.ConsecutiveFailures)
	}
	if h.LastErrorText != "boom boom" {
		t.Errorf("LastErrorText: got %q, want %q", h.LastErrorText, "boom boom")
	}
	if h.LastErrorAt == nil {
		t.Errorf("LastErrorAt: expected non-nil after failures")
	}
	if h.LastSuccessAt != nil {
		t.Errorf("LastSuccessAt: expected nil (no prior success), got %v", h.LastSuccessAt)
	}
}

func TestSourceHealth_SuccessAfterFailResetsCounter(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, ctx)
	sid := seedSource(t, ctx, s, "recovering-source")

	// First: 2 failures
	_ = s.UpsertSourceHealth(ctx, sid, false, "e1", 0)
	_ = s.UpsertSourceHealth(ctx, sid, false, "e2", 0)

	// Then: success
	if err := s.UpsertSourceHealth(ctx, sid, true, "", 5); err != nil {
		t.Fatalf("UpsertSourceHealth success: %v", err)
	}

	got, _ := s.ListSourceHealth(ctx)
	if len(got) != 1 {
		t.Fatalf("rows = %d", len(got))
	}
	h := got[0]
	if h.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures: got %d, want 0 (reset on success)", h.ConsecutiveFailures)
	}
	if h.LastSuccessAt == nil {
		t.Errorf("LastSuccessAt: expected non-nil after success")
	}
	// last_error_at / last_error_text 保留 (便于调试历史)
	if h.LastErrorAt == nil {
		t.Errorf("LastErrorAt: should be preserved after recovery")
	}
	if h.LastErrorText != "e2" {
		t.Errorf("LastErrorText: got %q, want %q (preserved)", h.LastErrorText, "e2")
	}
	if h.LastItemCount != 5 {
		t.Errorf("LastItemCount: got %d, want 5", h.LastItemCount)
	}
}

func TestSourceHealth_FailAfterSuccessPreservesSuccessTime(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, ctx)
	sid := seedSource(t, ctx, s, "was-ok-now-broken")

	// Success first
	_ = s.UpsertSourceHealth(ctx, sid, true, "", 20)
	before, _ := s.ListSourceHealth(ctx)
	successTimeBefore := before[0].LastSuccessAt

	time.Sleep(10 * time.Millisecond) // ensure next timestamp differs

	// Then fail
	_ = s.UpsertSourceHealth(ctx, sid, false, "fail!", 0)

	after, _ := s.ListSourceHealth(ctx)
	h := after[0]
	if h.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures: got %d, want 1", h.ConsecutiveFailures)
	}
	if h.LastSuccessAt == nil || !h.LastSuccessAt.Equal(*successTimeBefore) {
		t.Errorf("LastSuccessAt should be preserved: before=%v after=%v", successTimeBefore, h.LastSuccessAt)
	}
	if h.LastErrorText != "fail!" {
		t.Errorf("LastErrorText: got %q, want %q", h.LastErrorText, "fail!")
	}
}

func TestSourceHealth_ListOrdersBySickestFirst(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, ctx)
	a := seedSource(t, ctx, s, "source-a")
	b := seedSource(t, ctx, s, "source-b")
	c := seedSource(t, ctx, s, "source-c")

	// a: 0 fails (success), b: 3 fails, c: 1 fail
	_ = s.UpsertSourceHealth(ctx, a, true, "", 10)
	for i := 0; i < 3; i++ {
		_ = s.UpsertSourceHealth(ctx, b, false, "bad", 0)
	}
	_ = s.UpsertSourceHealth(ctx, c, false, "bad", 0)

	got, _ := s.ListSourceHealth(ctx)
	if len(got) != 3 {
		t.Fatalf("rows = %d", len(got))
	}
	// 期望顺序 by consecutive DESC: b (3) > c (1) > a (0)
	if got[0].SourceID != b {
		t.Errorf("[0] SourceID: got %d, want %d (b with 3 fails)", got[0].SourceID, b)
	}
	if got[1].SourceID != c {
		t.Errorf("[1] SourceID: got %d, want %d (c with 1 fail)", got[1].SourceID, c)
	}
	if got[2].SourceID != a {
		t.Errorf("[2] SourceID: got %d, want %d (a with 0 fails)", got[2].SourceID, a)
	}
}
