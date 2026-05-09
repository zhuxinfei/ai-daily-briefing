-- v1.0.1 critical fix (2026-04-14): per-section / per-stage state tracking
-- ---------------------------------------------------------------------------
-- This migration adds the bookkeeping tables and columns we need to
--   (a) know which migrations have run (schema_versioning)
--   (b) track per-section status on issue_items (validated / failed / pending)
--   (c) track per-stage status on issue_insights (insight_md / infocard_status)
--   (d) record every pipeline stage run (issue_stages) with hash + version + parent
--   (e) persist classify output (classified_items) so we don't re-run classify
--       to re-compose a single failed section
--   (f) audit every LLM call (llm_calls)
--   (g) prevent double publish per-subtype (deliveries.sub_status + UNIQUE)
--
-- Idempotency: every CREATE uses IF NOT EXISTS; ALTERs are guarded at the
-- Go layer (we swallow "duplicate column" errors the same way the 003/004
-- migrations already do). All DEFAULTs make the migration non-destructive
-- for existing rows.
-- ---------------------------------------------------------------------------

-- 1) Migration bookkeeping.
CREATE TABLE IF NOT EXISTS schema_migrations (
    version     INTEGER PRIMARY KEY,
    name        TEXT NOT NULL,
    applied_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Backfill v1-v4 iff we're running on a database that has the matching
-- artifacts but no history row yet. We use INSERT OR IGNORE so re-runs
-- are safe, and the INSERT ... WHERE EXISTS guards prevent back-fills
-- on a brand-new database (those rows get inserted by the Go migrate
-- driver after each pending migration runs successfully).
INSERT OR IGNORE INTO schema_migrations (version, name)
    SELECT 1, '001_initial'
    WHERE EXISTS (SELECT 1 FROM sqlite_master WHERE type='table' AND name='issues');

INSERT OR IGNORE INTO schema_migrations (version, name)
    SELECT 2, '002_weekly'
    WHERE EXISTS (SELECT 1 FROM sqlite_master WHERE type='table' AND name='weekly_issues');

INSERT OR IGNORE INTO schema_migrations (version, name)
    SELECT 3, '003_weekly_diagram'
    WHERE EXISTS (
        SELECT 1 FROM pragma_table_info('weekly_issues') WHERE name='trends_diagram'
    );

INSERT OR IGNORE INTO schema_migrations (version, name)
    SELECT 4, '004_weekly_diagram_detail'
    WHERE EXISTS (
        SELECT 1 FROM pragma_table_info('weekly_issues') WHERE name='trends_diagram_detail'
    );

-- 2) issue_items per-section status.
--    status: 'pending' | 'running' | 'validated' | 'degraded' | 'failed'
ALTER TABLE issue_items ADD COLUMN status        TEXT    NOT NULL DEFAULT 'validated';
ALTER TABLE issue_items ADD COLUMN validated_at  TIMESTAMP;
ALTER TABLE issue_items ADD COLUMN retry_count   INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_issue_items_issue_status ON issue_items(issue_id, status);

-- 3) issue_insights per-stage status (insight + infocard are 2 distinct stages).
--    status:           'pending' | 'running' | 'validated' | 'degraded' | 'failed'
--    infocard_status:  'pending' | 'running' | 'validated' | 'degraded' | 'failed'
ALTER TABLE issue_insights ADD COLUMN status           TEXT    NOT NULL DEFAULT 'validated';
ALTER TABLE issue_insights ADD COLUMN validated_at     TIMESTAMP;
ALTER TABLE issue_insights ADD COLUMN infocard_status  TEXT    NOT NULL DEFAULT 'pending';

-- 4) Pipeline stage ledger.
--    Every stage of every pipeline run writes a row here (ingest, filter,
--    rank, classify, compose, media, insight, summary, infocard, render,
--    gate, publish). If a stage re-runs for the same (issue, stage) we
--    bump `version` so history is preserved.
--
--    status: 'running' | 'succeeded' | 'failed' | 'skipped'
--    input_hash: sha256 of normalized input JSON — lets a later run
--                 short-circuit ("we already computed this input").
--    parent_versions: JSON array of version numbers of the stages this
--                      stage consumed (for traceability; e.g. compose
--                      depends on classify v=3 and rank v=2).
CREATE TABLE IF NOT EXISTS issue_stages (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id         INTEGER NOT NULL REFERENCES issues(id),
    stage            TEXT    NOT NULL,
    status           TEXT    NOT NULL DEFAULT 'running',
    input_hash       TEXT,
    version          INTEGER NOT NULL DEFAULT 1,
    parent_versions  TEXT,
    started_at       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at     TIMESTAMP,
    error_text       TEXT,
    UNIQUE(issue_id, stage, version)
);

CREATE INDEX IF NOT EXISTS idx_issue_stages_issue_stage
    ON issue_stages(issue_id, stage);
CREATE INDEX IF NOT EXISTS idx_issue_stages_running
    ON issue_stages(status, started_at)
    WHERE status = 'running';

-- 5) Persisted classify output — lets `briefing repair --section X`
--    re-run only compose for a single failed section without re-running
--    the expensive classify LLM round.
CREATE TABLE IF NOT EXISTS classified_items (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id     INTEGER NOT NULL REFERENCES issues(id),
    section      TEXT    NOT NULL,
    raw_item_id  INTEGER NOT NULL REFERENCES raw_items(id),
    rank_score   REAL,
    seq          INTEGER NOT NULL,
    created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(issue_id, section, raw_item_id)
);

CREATE INDEX IF NOT EXISTS idx_classified_items_issue_section
    ON classified_items(issue_id, section, seq);

-- 6) LLM call audit log. One row per request (includes failures). Used by
--    `briefing status --verbose` and postmortem analysis of 502/timeout
--    bursts.
CREATE TABLE IF NOT EXISTS llm_calls (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id         INTEGER,
    stage            TEXT NOT NULL,
    model            TEXT NOT NULL,
    prompt_hash      TEXT,
    request_tokens   INTEGER,
    response_tokens  INTEGER,
    latency_ms       INTEGER,
    http_status      INTEGER,
    error_text       TEXT,
    called_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_llm_calls_issue_stage
    ON llm_calls(issue_id, stage, called_at);

-- 7) deliveries.sub_status lets us separate "test publish" vs "prod
--    publish" vs "alert publish" on the same channel. UNIQUE guarantees
--    a rerun cannot accidentally double-publish the same combination.
ALTER TABLE deliveries ADD COLUMN sub_status TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_deliveries_unique_pub
    ON deliveries(issue_id, channel, sub_status)
    WHERE sub_status IS NOT NULL AND status = 'sent';
