package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"briefing-v3/internal/config"
	"briefing-v3/internal/store"
)

func newSeedTestStore(t *testing.T) store.Store {
	t.Helper()
	s, err := store.New(filepath.Join(t.TempDir(), "seed.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// seedCommand upserts the domain before its sources; sources.domain_id is a
	// foreign key onto domains, so a store without one cannot take a source row.
	if err := s.UpsertDomain(context.Background(), &store.Domain{ID: "ai", Name: "AI"}); err != nil {
		t.Fatalf("UpsertDomain: %v", err)
	}
	return s
}

func seedCfg(url string, enabled bool) *config.Config {
	return &config.Config{
		Domain: config.DomainConfig{ID: "ai", Name: "AI"},
		Sources: []config.SourceConfig{{
			ID: "gh", Type: "github_trending", Category: "project",
			Name: "GitHub Trending", URL: url, Enabled: enabled,
		}},
	}
}

func enabledURLs(t *testing.T, ctx context.Context, s store.Store) []string {
	t.Helper()
	rows, err := s.ListEnabledSources(ctx, "ai")
	if err != nil {
		t.Fatalf("ListEnabledSources: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ConfigJSON)
	}
	return out
}

// TestSeedSources_ConfigCanDisableASource is the regression for a silent
// one-way bug: seed used to walk cfg.EnabledSources(), so a source marked
// `enabled: false` was never upserted at all and the existing row kept
// enabled=1 and its old config_json. The config could switch a source on but
// never off — which is how the retired ossinsight feed kept being fetched
// after config/ai.yaml had supposedly disabled it.
func TestSeedSources_ConfigCanDisableASource(t *testing.T) {
	ctx := context.Background()
	s := newSeedTestStore(t)

	// Seeded enabled: the source is listed.
	if _, err := seedSources(ctx, s, seedCfg("https://github.com/trending", true)); err != nil {
		t.Fatalf("seedSources(enabled): %v", err)
	}
	if got := enabledURLs(t, ctx, s); len(got) != 1 {
		t.Fatalf("after enabling: %d enabled sources, want 1", len(got))
	}

	// Seeded disabled: it must actually go away. This is the case that used to
	// be skipped entirely.
	if _, err := seedSources(ctx, s, seedCfg("https://github.com/trending", false)); err != nil {
		t.Fatalf("seedSources(disabled): %v", err)
	}
	if got := enabledURLs(t, ctx, s); len(got) != 0 {
		t.Fatalf("after disabling: %d enabled sources, want 0 — the config must be able to switch a source off, "+
			"not only on", len(got))
	}

	// Re-enabled with different config: the disabled pass must not have left
	// the old config_json behind on the row.
	if _, err := seedSources(ctx, s, seedCfg("https://example.com/other", true)); err != nil {
		t.Fatalf("seedSources(re-enabled): %v", err)
	}
	got := enabledURLs(t, ctx, s)
	if len(got) != 1 {
		t.Fatalf("after re-enabling: %d enabled sources, want 1", len(got))
	}
	if !strings.Contains(got[0], "https://example.com/other") {
		t.Errorf("config_json = %s, want the re-enabled URL — the row kept stale config from before it was disabled", got[0])
	}
}
