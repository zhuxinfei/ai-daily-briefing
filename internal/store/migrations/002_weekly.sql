-- v1.0.1: weekly report storage
-- One row per (domain, year, ISO-week).

CREATE TABLE IF NOT EXISTS weekly_issues (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    domain_id       TEXT NOT NULL REFERENCES domains(id),
    year            INTEGER NOT NULL,
    week            INTEGER NOT NULL,
    start_date      DATE NOT NULL,
    end_date        DATE NOT NULL,
    title           TEXT,
    focus_md        TEXT,
    signals_md      TEXT,
    trends_md       TEXT,
    takeaways_md    TEXT,
    ponder_md       TEXT,
    full_md         TEXT,
    daily_issue_ids TEXT,
    status          TEXT NOT NULL DEFAULT 'draft',
    generated_at    TIMESTAMP,
    published_at    TIMESTAMP,
    created_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(domain_id, year, week)
);

CREATE INDEX IF NOT EXISTS idx_weekly_domain_year_week
    ON weekly_issues(domain_id, year, week);
