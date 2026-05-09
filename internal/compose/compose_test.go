// compose_test.go — v1.0.1 Phase 4.2.1 compose section retry.
//
// Focus: verify that when the Summarizer transiently fails for a section,
// compose retries it after all other sections have been processed.
//
// Run with: go test ./internal/compose/ -v

package compose

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"briefing-v3/internal/generate"
	"briefing-v3/internal/store"
)

// flakySummarizer succeeds or fails based on per-section call counters.
// Typical use: set failureByCallCount[section] = N to make the Nth call fail.
type flakySummarizer struct {
	mu       sync.Mutex
	calls    map[string]int
	failUpTo map[string]int // section -> number of initial calls to fail
	output   map[string]string
}

func newFlakySummarizer() *flakySummarizer {
	return &flakySummarizer{
		calls:    map[string]int{},
		failUpTo: map[string]int{},
		output:   map[string]string{},
	}
}

func (f *flakySummarizer) Summarize(_ context.Context, sectionTitle string, items []*store.RawItem) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[sectionTitle]++
	if f.calls[sectionTitle] <= f.failUpTo[sectionTitle] {
		return "", errors.New("openai http 502: upstream request failed")
	}
	if body, ok := f.output[sectionTitle]; ok {
		return body, nil
	}
	// Default non-empty markdown reply.
	return "1. **Test section body.**\n本 section 的默认回复 [详情(briefing)](https://example.com)。", nil
}

var _ generate.Summarizer = (*flakySummarizer)(nil)

// override sectionRetryWait for tests so we don't actually sleep 30s.
func withShortRetryWait(t *testing.T, d time.Duration) func() {
	t.Helper()
	orig := sectionRetryWait
	sectionRetryWait = d
	return func() { sectionRetryWait = orig }
}

func TestCompose_AllSectionsOK(t *testing.T) {
	c := New()
	sections := []SectionConfig{
		{ID: "a", Title: "A", MinItems: 1, MaxItems: 5},
		{ID: "b", Title: "B", MinItems: 1, MaxItems: 5},
	}
	sectioned := map[string][]*store.RawItem{
		"a": {{ID: 1, Title: "item a1", URL: "https://x.com/1"}},
		"b": {{ID: 2, Title: "item b1", URL: "https://x.com/2"}},
	}
	out, failed, err := c.Compose(context.Background(), 1, sectioned, sections, newFlakySummarizer())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(failed) != 0 {
		t.Errorf("expected no failed sections, got %v", failed)
	}
	if len(out) == 0 {
		t.Errorf("expected some issue items, got 0")
	}
}

func TestCompose_RetrySucceedsSecondPass(t *testing.T) {
	// Section "b" fails first call, succeeds second call → retry should recover it.
	defer withShortRetryWait(t, 10*time.Millisecond)()

	c := New()
	sections := []SectionConfig{
		{ID: "a", Title: "SectionA", MinItems: 1, MaxItems: 5},
		{ID: "b", Title: "SectionB", MinItems: 1, MaxItems: 5},
	}
	sectioned := map[string][]*store.RawItem{
		"a": {{ID: 1, Title: "item a1", URL: "https://x.com/1"}},
		"b": {{ID: 2, Title: "item b1", URL: "https://x.com/2"}},
	}
	fs := newFlakySummarizer()
	fs.failUpTo["SectionB"] = 1 // first call fails, second succeeds

	out, failed, err := c.Compose(context.Background(), 1, sectioned, sections, fs)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(failed) != 0 {
		t.Errorf("expected retry to recover SectionB, still failed: %v", failed)
	}
	// Summarizer should have been called 2 times for SectionA (1) + SectionB (2) = 3 total.
	if fs.calls["SectionA"] != 1 {
		t.Errorf("expected SectionA 1 call, got %d", fs.calls["SectionA"])
	}
	if fs.calls["SectionB"] != 2 {
		t.Errorf("expected SectionB 2 calls (1 fail + 1 retry), got %d", fs.calls["SectionB"])
	}
	if len(out) < 2 {
		t.Errorf("expected at least 2 issue items, got %d", len(out))
	}
}

func TestCompose_RetryFailsSecondPass_FallbackUsesRawItems(t *testing.T) {
	// Section "b" fails first AND second call → T14 fallback 用 raw items 拼简版.
	// failedSections 应为空 (fallback 成功了), 但 section 内容应包含 ⚠️ 标记.
	defer withShortRetryWait(t, 10*time.Millisecond)()

	c := New()
	sections := []SectionConfig{
		{ID: "a", Title: "SectionA", MinItems: 1, MaxItems: 5},
		{ID: "b", Title: "SectionB", MinItems: 1, MaxItems: 5},
	}
	sectioned := map[string][]*store.RawItem{
		"a": {{ID: 1, Title: "item a1", URL: "https://x.com/1"}},
		"b": {{ID: 2, Title: "item b1 fallback", URL: "https://x.com/2", Content: "fallback body content"}},
	}
	fs := newFlakySummarizer()
	fs.failUpTo["SectionB"] = 999 // always fail LLM

	out, failed, err := c.Compose(context.Background(), 1, sectioned, sections, fs)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// T14 fallback 兜底 → failedSections 应为空.
	if len(failed) != 0 {
		t.Errorf("T14 fallback expected, got failedSections=%v", failed)
	}
	// 1 original compose + 1 retry + 1 simplified-LLM fallback = 3
	if fs.calls["SectionB"] != 3 {
		t.Errorf("expected SectionB 3 calls (1 original + 1 retry + 1 simplified-LLM fallback), got %d", fs.calls["SectionB"])
	}
	// 验证 fallback 产物在 out 里, 标题里能看到 raw item 的 fallback 标记.
	foundFallback := false
	for _, item := range out {
		if item.Section == "b" && containsAny(item.BodyMD, "⚠️", "原始候选", "fallback") {
			foundFallback = true
			break
		}
	}
	if !foundFallback {
		t.Errorf("expected fallback marker (⚠️ / 原始候选 / fallback) in section b body, got items=%v", out)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestCompose_RetryCancelledByContext(t *testing.T) {
	// Retry should abort early if ctx is cancelled during the wait.
	defer withShortRetryWait(t, 5*time.Second)() // long enough to cancel before firing

	c := New()
	sections := []SectionConfig{
		{ID: "a", Title: "SectionA", MinItems: 1, MaxItems: 5},
	}
	sectioned := map[string][]*store.RawItem{
		"a": {{ID: 1, Title: "item a1", URL: "https://x.com/1"}},
	}
	fs := newFlakySummarizer()
	fs.failUpTo["SectionA"] = 999

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, failed, err := c.Compose(ctx, 1, sectioned, sections, fs)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(failed) != 1 {
		t.Errorf("expected 1 failed section, got %v", failed)
	}
	if elapsed > 1*time.Second {
		t.Errorf("ctx cancel should have short-circuited retry wait, took %v", elapsed)
	}
	// Should only have 1 call (original pass), retry was cancelled.
	if fs.calls["SectionA"] != 1 {
		t.Errorf("expected only 1 call (retry cancelled), got %d", fs.calls["SectionA"])
	}
}

// emptyThenOKSummarizer: first call returns "" (not error), second returns real body.
// Tests that empty markdown is also retried (compose.go treats empty as soft-failure).
type emptyThenOKSummarizer struct {
	calls int
}

func (e *emptyThenOKSummarizer) Summarize(_ context.Context, _ string, _ []*store.RawItem) (string, error) {
	e.calls++
	if e.calls == 1 {
		return "", nil // empty but no error → failedSections
	}
	return "1. **Recovered.**\n重试后生成了实际内容 [链接(briefing)](https://example.com)。", nil
}

func TestCompose_EmptyMarkdownRetried(t *testing.T) {
	defer withShortRetryWait(t, 10*time.Millisecond)()

	c := New()
	sections := []SectionConfig{
		{ID: "a", Title: "SectionA", MinItems: 1, MaxItems: 5},
	}
	sectioned := map[string][]*store.RawItem{
		"a": {{ID: 1, Title: "item a1", URL: "https://x.com/1"}},
	}
	e := &emptyThenOKSummarizer{}

	out, failed, err := c.Compose(context.Background(), 1, sectioned, sections, e)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(failed) != 0 {
		t.Errorf("expected retry to recover, still failed: %v", failed)
	}
	if e.calls != 2 {
		t.Errorf("expected 2 Summarize calls (empty + recovered), got %d", e.calls)
	}
	if len(out) == 0 {
		t.Errorf("expected issue items from recovered retry, got 0")
	}
}
