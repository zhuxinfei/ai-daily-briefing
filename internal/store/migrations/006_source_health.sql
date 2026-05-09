-- v1.0.1 Phase 1.3: per-source health monitoring (2026-04-14)
-- ---------------------------------------------------------------------------
-- Track each source's ingest outcome so we can:
--   (a) surface per-source status via `briefing status --sources`
--   (b) alert on consecutive failures (e.g. Anthropic HTML scrape blocked)
--   (c) auto-disable stale/broken sources (via `auto_disabled` flag)
--
-- Single-row-per-source (PK = source_id). Each ingest run upserts one row.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS source_health (
    source_id            INTEGER PRIMARY KEY REFERENCES sources(id),
    last_success_at      TIMESTAMP,
    last_error_at        TIMESTAMP,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    last_error_text      TEXT,
    last_item_count      INTEGER NOT NULL DEFAULT 0,
    auto_disabled        INTEGER NOT NULL DEFAULT 0, -- 0/1 boolean
    updated_at           TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Index for "find sources with N+ consecutive failures" queries.
CREATE INDEX IF NOT EXISTS idx_source_health_failures
    ON source_health(consecutive_failures)
    WHERE consecutive_failures > 0;
