-- briefing-v3 initial schema
-- See docs/specs/2026-04-10-briefing-v3-design.md §7

-- Domains: first-class entity for multi-domain support ('ai', 'web3', ...)
CREATE TABLE IF NOT EXISTS domains (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    config_path TEXT,
    created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Sources: data source configurations per domain
CREATE TABLE IF NOT EXISTS sources (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    domain_id   TEXT NOT NULL REFERENCES domains(id),
    type        TEXT NOT NULL,
    name        TEXT NOT NULL,
    config_json TEXT NOT NULL,
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(domain_id, type, name)
);

CREATE INDEX IF NOT EXISTS idx_sources_domain_enabled ON sources(domain_id, enabled);

-- RawItems: items ingested from sources, pre-LLM
CREATE TABLE IF NOT EXISTS raw_items (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    domain_id     TEXT NOT NULL REFERENCES domains(id),
    source_id     INTEGER NOT NULL REFERENCES sources(id),
    external_id   TEXT,
    url           TEXT NOT NULL,
    title         TEXT,
    author        TEXT,
    published_at  TIMESTAMP,
    fetched_at    TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    content       TEXT,
    metadata_json TEXT,
    UNIQUE(source_id, external_id)
);

CREATE INDEX IF NOT EXISTS idx_raw_items_domain_fetched ON raw_items(domain_id, fetched_at);
CREATE INDEX IF NOT EXISTS idx_raw_items_url ON raw_items(url);

-- Issues: one daily briefing per (domain, date)
CREATE TABLE IF NOT EXISTS issues (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    domain_id     TEXT NOT NULL REFERENCES domains(id),
    issue_date    DATE NOT NULL,
    issue_number  INTEGER,
    title         TEXT,
    summary       TEXT,
    status        TEXT NOT NULL DEFAULT 'draft',
    source_count  INTEGER DEFAULT 0,
    item_count    INTEGER DEFAULT 0,
    generated_at  TIMESTAMP,
    published_at  TIMESTAMP,
    UNIQUE(domain_id, issue_date)
);

CREATE INDEX IF NOT EXISTS idx_issues_domain_date ON issues(domain_id, issue_date);

-- IssueItems: entries grouped into named sections per issue
CREATE TABLE IF NOT EXISTS issue_items (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id          INTEGER NOT NULL REFERENCES issues(id),
    section           TEXT NOT NULL,
    seq               INTEGER NOT NULL,
    title             TEXT NOT NULL,
    body_md           TEXT NOT NULL,
    source_urls_json  TEXT,
    raw_item_ids_json TEXT,
    created_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_issue_items_issue ON issue_items(issue_id, section, seq);

-- IssueInsights: LLM-generated industry insights + our takeaways
CREATE TABLE IF NOT EXISTS issue_insights (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id     INTEGER NOT NULL REFERENCES issues(id),
    industry_md  TEXT,
    our_md       TEXT,
    model        TEXT,
    temperature  REAL,
    retry_count  INTEGER NOT NULL DEFAULT 0,
    generated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(issue_id)
);

-- Deliveries: publish attempts per channel
CREATE TABLE IF NOT EXISTS deliveries (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id      INTEGER NOT NULL REFERENCES issues(id),
    channel       TEXT NOT NULL,
    target        TEXT,
    status        TEXT NOT NULL,
    response_json TEXT,
    sent_at       TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_deliveries_issue ON deliveries(issue_id, channel);
