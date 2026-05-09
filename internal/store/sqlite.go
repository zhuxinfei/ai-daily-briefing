package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// initialSchema is embedded from internal/store/migrations/001_initial.sql.
// A mirror copy lives at migrations/001_initial.sql (project root) for
// external migration tools and DBAs to inspect; the two files must stay in sync.
//
//go:embed migrations/001_initial.sql
var initialSchema string

//go:embed migrations/002_weekly.sql
var weeklySchema string

//go:embed migrations/003_weekly_diagram.sql
var weeklyDiagramSchema string

//go:embed migrations/004_weekly_diagram_detail.sql
var weeklyDiagramDetailSchema string

//go:embed migrations/005_schema_versioning.sql
var schemaVersioningSchema string

//go:embed migrations/006_source_health.sql
var sourceHealthSchema string

// migration is one logical step in the schema evolution. `version` is
// monotonically increasing and forms the primary key of the
// schema_migrations audit table. `sql` is the SQL blob to execute.
// `tolerateDup` means "ignore a `duplicate column` error from an ADD
// COLUMN statement"; older migrations (003, 004) were authored before
// we had an explicit migration ledger and are re-applied idempotently
// for backwards compatibility.
type migration struct {
	version     int
	name        string
	sql         string
	tolerateDup bool
}

// allMigrations is the canonical ordered list. Adding a new migration
// = append one row + put the .sql file under migrations/ + //go:embed it.
// Never renumber or remove rows, only append.
func allMigrations() []migration {
	return []migration{
		{1, "001_initial", initialSchema, false},
		{2, "002_weekly", weeklySchema, false},
		{3, "003_weekly_diagram", weeklyDiagramSchema, true},
		{4, "004_weekly_diagram_detail", weeklyDiagramDetailSchema, true},
		{5, "005_schema_versioning", schemaVersioningSchema, true},
		{6, "006_source_health", sourceHealthSchema, true},
	}
}

// New opens (or creates) a SQLite database at dbPath and returns a Store.
// The caller must invoke Migrate(ctx) before using the Store for reads/writes.
func New(dbPath string) (Store, error) {
	// modernc.org/sqlite driver name is "sqlite".
	// Pragmas: busy_timeout avoids "database is locked" during concurrent writes,
	// foreign_keys enforces referential integrity, journal_mode=WAL improves concurrency.
	dsn := dbPath + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite ping: %w", err)
	}
	return &sqliteStore{db: db}, nil
}

type sqliteStore struct {
	db *sql.DB
}

// -------- Lifecycle --------

// Migrate runs every pending migration in order. The flow is:
//  1. Apply migrations 1..4 once (they are all idempotent — CREATE TABLE IF
//     NOT EXISTS or ALTER TABLE ADD COLUMN with duplicate-column tolerance).
//  2. Apply migration 5 (schema_versioning). This one creates the
//     `schema_migrations` ledger as its first statement and back-fills
//     v1..v4 rows when their corresponding schema artifacts already exist.
//  3. After every migration that has a row count of 0 in the ledger,
//     insert its row into schema_migrations so subsequent runs can skip.
//
// The extra belt-and-braces logic (guard around duplicate column /
// table already exists) exists because 003/004 were shipped before we
// had the ledger and some databases may have inconsistent state.
func (s *sqliteStore) Migrate(ctx context.Context) error {
	// Step 1: apply every migration in order. We silently swallow the
	// "duplicate column" error on ALTER TABLE ADD COLUMN for migrations
	// flagged tolerateDup so re-runs on an already-migrated DB succeed.
	for _, m := range allMigrations() {
		if _, err := s.db.ExecContext(ctx, m.sql); err != nil {
			if m.tolerateDup && strings.Contains(err.Error(), "duplicate column") {
				// Already applied; fall through to book-keeping below.
			} else {
				return fmt.Errorf("migrate %s: %w", m.name, err)
			}
		}
	}

	// Step 2: ensure the ledger is present (happens inside 005) and
	// back-fill any still-missing rows. At this point every migration
	// has run, so it is safe to record them unconditionally. The v1-v4
	// rows were already INSERT OR IGNORE'd inside 005; we repeat the
	// records here to guarantee correctness even if 005 ever changes
	// its back-fill guards.
	for _, m := range allMigrations() {
		if _, err := s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO schema_migrations (version, name) VALUES (?, ?)`,
			m.version, m.name,
		); err != nil {
			return fmt.Errorf("record migration %s: %w", m.name, err)
		}
	}

	return nil
}

func (s *sqliteStore) Close() error {
	return s.db.Close()
}

// -------- Domain --------

func (s *sqliteStore) UpsertDomain(ctx context.Context, d *Domain) error {
	const q = `
		INSERT INTO domains (id, name, config_path, created_at)
		VALUES (?, ?, ?, COALESCE(?, CURRENT_TIMESTAMP))
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			config_path = excluded.config_path
	`
	var createdAt any
	if !d.CreatedAt.IsZero() {
		createdAt = d.CreatedAt
	}
	if _, err := s.db.ExecContext(ctx, q, d.ID, d.Name, d.ConfigPath, createdAt); err != nil {
		return fmt.Errorf("upsert domain %q: %w", d.ID, err)
	}
	return nil
}

func (s *sqliteStore) GetDomain(ctx context.Context, id string) (*Domain, error) {
	const q = `SELECT id, name, COALESCE(config_path, ''), created_at FROM domains WHERE id = ?`
	var d Domain
	err := s.db.QueryRowContext(ctx, q, id).Scan(&d.ID, &d.Name, &d.ConfigPath, &d.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get domain %q: %w", id, err)
	}
	return &d, nil
}

// -------- Source --------

func (s *sqliteStore) UpsertSource(ctx context.Context, src *Source) (int64, error) {
	const q = `
		INSERT INTO sources (domain_id, type, name, config_json, enabled, created_at)
		VALUES (?, ?, ?, ?, ?, COALESCE(?, CURRENT_TIMESTAMP))
		ON CONFLICT(domain_id, type, name) DO UPDATE SET
			config_json = excluded.config_json,
			enabled = excluded.enabled
		RETURNING id
	`
	var createdAt any
	if !src.CreatedAt.IsZero() {
		createdAt = src.CreatedAt
	}
	enabled := 0
	if src.Enabled {
		enabled = 1
	}
	var id int64
	err := s.db.QueryRowContext(ctx, q,
		src.DomainID, src.Type, src.Name, src.ConfigJSON, enabled, createdAt,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert source %s/%s/%s: %w", src.DomainID, src.Type, src.Name, err)
	}
	return id, nil
}

func (s *sqliteStore) ListEnabledSources(ctx context.Context, domainID string) ([]*Source, error) {
	const q = `
		SELECT id, domain_id, type, name, config_json, enabled, created_at
		FROM sources
		WHERE domain_id = ? AND enabled = 1
		ORDER BY id
	`
	rows, err := s.db.QueryContext(ctx, q, domainID)
	if err != nil {
		return nil, fmt.Errorf("list sources %q: %w", domainID, err)
	}
	defer rows.Close()

	var out []*Source
	for rows.Next() {
		var src Source
		var enabled int
		if err := rows.Scan(
			&src.ID, &src.DomainID, &src.Type, &src.Name,
			&src.ConfigJSON, &enabled, &src.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan source: %w", err)
		}
		src.Enabled = enabled != 0
		// Cheap one-off parse of config_json to surface the Category field.
		// We intentionally ignore errors here — a missing or malformed JSON
		// leaves Category empty, which downstream rule-based classify will
		// treat as "unknown → fall through to LLM".
		src.Category = extractSourceCategory(src.ConfigJSON)
		// v1.0.1 Phase 4.1: also surface Priority for rank weighting.
		// 0 = not set (rank treats as neutral, weight 1.0).
		src.Priority = extractSourcePriority(src.ConfigJSON)
		out = append(out, &src)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sources: %w", err)
	}
	return out, nil
}

// extractSourceCategory pulls the "category" string out of a source's
// config_json blob. Returns an empty string on any parse error or if the
// field is missing. Kept here (not in types.go) because it is a SQLite
// implementation detail of how sources.config_json is serialized by
// cmd/briefing/main.go:marshalSourceConfig.
func extractSourceCategory(configJSON string) string {
	if strings.TrimSpace(configJSON) == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(configJSON), &m); err != nil {
		return ""
	}
	v, ok := m["category"]
	if !ok {
		return ""
	}
	cat, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(cat)
}

// extractSourcePriority pulls the "priority" int out of config_json.
// Returns 0 on any parse error / missing field (rank treats 0 as neutral).
// v1.0.1 Phase 4.1.
func extractSourcePriority(configJSON string) int {
	if strings.TrimSpace(configJSON) == "" {
		return 0
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(configJSON), &m); err != nil {
		return 0
	}
	v, ok := m["priority"]
	if !ok {
		return 0
	}
	// JSON numbers come back as float64 even when YAML input was int.
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

// -------- RawItem --------

func (s *sqliteStore) InsertRawItems(ctx context.Context, items []*RawItem) error {
	if len(items) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const q = `
		INSERT OR IGNORE INTO raw_items
			(domain_id, source_id, external_id, url, title, author, published_at, fetched_at, content, metadata_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	stmt, err := tx.PrepareContext(ctx, q)
	if err != nil {
		return fmt.Errorf("prepare insert raw_items: %w", err)
	}
	defer stmt.Close()
	lookupByExt, err := tx.PrepareContext(ctx,
		`SELECT id FROM raw_items WHERE source_id = ? AND external_id = ? ORDER BY id DESC LIMIT 1`)
	if err != nil {
		return fmt.Errorf("prepare lookup raw_items by external_id: %w", err)
	}
	defer lookupByExt.Close()
	lookupByURL, err := tx.PrepareContext(ctx,
		`SELECT id FROM raw_items WHERE source_id = ? AND url = ? ORDER BY id DESC LIMIT 1`)
	if err != nil {
		return fmt.Errorf("prepare lookup raw_items by url: %w", err)
	}
	defer lookupByURL.Close()

	for _, it := range items {
		var publishedAt any
		if !it.PublishedAt.IsZero() {
			publishedAt = it.PublishedAt
		}
		var fetchedAt any
		if !it.FetchedAt.IsZero() {
			fetchedAt = it.FetchedAt
		} else {
			fetchedAt = time.Now().UTC()
		}
		if _, err := stmt.ExecContext(ctx,
			it.DomainID, it.SourceID, nullString(it.ExternalID),
			it.URL, it.Title, it.Author,
			publishedAt, fetchedAt,
			it.Content, it.MetadataJSON,
		); err != nil {
			return fmt.Errorf("insert raw_item (source=%d ext=%q): %w", it.SourceID, it.ExternalID, err)
		}
		var rowID int64
		if strings.TrimSpace(it.ExternalID) != "" {
			if err := lookupByExt.QueryRowContext(ctx, it.SourceID, it.ExternalID).Scan(&rowID); err != nil {
				return fmt.Errorf("lookup raw_item id by external_id (source=%d ext=%q): %w", it.SourceID, it.ExternalID, err)
			}
		} else {
			if err := lookupByURL.QueryRowContext(ctx, it.SourceID, it.URL).Scan(&rowID); err != nil {
				return fmt.Errorf("lookup raw_item id by url (source=%d url=%q): %w", it.SourceID, it.URL, err)
			}
		}
		it.ID = rowID
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit insert raw_items: %w", err)
	}
	return nil
}

func (s *sqliteStore) ListRecentRawItems(ctx context.Context, domainID string, since time.Time) ([]*RawItem, error) {
	const q = `
		SELECT id, domain_id, source_id, COALESCE(external_id, ''), url,
		       COALESCE(title, ''), COALESCE(author, ''),
		       published_at, fetched_at,
		       COALESCE(content, ''), COALESCE(metadata_json, '')
		FROM raw_items
		WHERE domain_id = ? AND fetched_at >= ?
		ORDER BY fetched_at DESC, id DESC
	`
	rows, err := s.db.QueryContext(ctx, q, domainID, since)
	if err != nil {
		return nil, fmt.Errorf("list raw_items %q: %w", domainID, err)
	}
	defer rows.Close()

	var out []*RawItem
	for rows.Next() {
		var it RawItem
		var publishedAt sql.NullTime
		if err := rows.Scan(
			&it.ID, &it.DomainID, &it.SourceID, &it.ExternalID, &it.URL,
			&it.Title, &it.Author,
			&publishedAt, &it.FetchedAt,
			&it.Content, &it.MetadataJSON,
		); err != nil {
			return nil, fmt.Errorf("scan raw_item: %w", err)
		}
		if publishedAt.Valid {
			it.PublishedAt = publishedAt.Time
		}
		out = append(out, &it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate raw_items: %w", err)
	}
	return out, nil
}

func (s *sqliteStore) UpdateRawItemContent(ctx context.Context, id int64, content string) error {
	const q = `UPDATE raw_items SET content = ? WHERE id = ?`
	res, err := s.db.ExecContext(ctx, q, content, id)
	if err != nil {
		return fmt.Errorf("update raw_item %d content: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("update raw_item %d: not found", id)
	}
	return nil
}

// -------- Issue --------

func (s *sqliteStore) UpsertIssue(ctx context.Context, issue *Issue) (int64, error) {
	// Use ON CONFLICT to preserve id on update (INSERT OR REPLACE would reassign id
	// and cascade-break FK references from issue_items).
	//
	// Bug K fix (2026-04-14): the previous version overwrote `status` with
	// excluded.status unconditionally. That caused a rerun of the pipeline
	// (classify/compose/etc) to reset an already-published issue's status back
	// to 'generated', which then tricked the orchestrator into trying to
	// publish it again. We now clamp status with a CASE expression: once an
	// issue is 'published' we refuse to downgrade it. Similarly we never let
	// published_at/generated_at regress to NULL once they are non-NULL (so a
	// partial rerun that omits them preserves the original timestamps).
	const q = `
		INSERT INTO issues
			(domain_id, issue_date, issue_number, title, summary, status,
			 source_count, item_count, generated_at, published_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(domain_id, issue_date) DO UPDATE SET
			issue_number = excluded.issue_number,
			title = excluded.title,
			summary = excluded.summary,
			status = CASE
				WHEN issues.status = 'published' THEN issues.status
				ELSE excluded.status
			END,
			source_count = excluded.source_count,
			item_count = excluded.item_count,
			generated_at = COALESCE(excluded.generated_at, issues.generated_at),
			published_at = COALESCE(issues.published_at, excluded.published_at)
		RETURNING id
	`
	status := issue.Status
	if status == "" {
		status = IssueStatusDraft
	}
	var id int64
	err := s.db.QueryRowContext(ctx, q,
		issue.DomainID, issue.IssueDate.Format("2006-01-02"),
		nullInt(int64(issue.IssueNumber)),
		issue.Title, issue.Summary, status,
		issue.SourceCount, issue.ItemCount,
		nullTimePtr(issue.GeneratedAt), nullTimePtr(issue.PublishedAt),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert issue %s/%s: %w", issue.DomainID, issue.IssueDate.Format("2006-01-02"), err)
	}
	return id, nil
}

func (s *sqliteStore) GetIssueByDate(ctx context.Context, domainID string, date time.Time) (*Issue, error) {
	const q = `
		SELECT id, domain_id, issue_date, COALESCE(issue_number, 0),
		       COALESCE(title, ''), COALESCE(summary, ''), status,
		       COALESCE(source_count, 0), COALESCE(item_count, 0),
		       generated_at, published_at
		FROM issues
		WHERE domain_id = ? AND issue_date = ?
	`
	row := s.db.QueryRowContext(ctx, q, domainID, date.Format("2006-01-02"))

	var is Issue
	var generatedAt, publishedAt sql.NullTime
	err := row.Scan(
		&is.ID, &is.DomainID, &is.IssueDate, &is.IssueNumber,
		&is.Title, &is.Summary, &is.Status,
		&is.SourceCount, &is.ItemCount,
		&generatedAt, &publishedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get issue %s/%s: %w", domainID, date.Format("2006-01-02"), err)
	}
	if generatedAt.Valid {
		t := generatedAt.Time
		is.GeneratedAt = &t
	}
	if publishedAt.Valid {
		t := publishedAt.Time
		is.PublishedAt = &t
	}
	return &is, nil
}

func (s *sqliteStore) MarkIssuePublished(ctx context.Context, issueID int64) error {
	const q = `
		UPDATE issues
		SET status = ?, published_at = COALESCE(published_at, CURRENT_TIMESTAMP)
		WHERE id = ?
	`
	res, err := s.db.ExecContext(ctx, q, IssueStatusPublished, issueID)
	if err != nil {
		return fmt.Errorf("mark issue %d published: %w", issueID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("mark issue %d published: not found", issueID)
	}
	return nil
}

func (s *sqliteStore) MarkWeeklyPublished(ctx context.Context, weeklyID int64) error {
	const q = `
		UPDATE weekly_issues
		SET status = ?, published_at = COALESCE(published_at, CURRENT_TIMESTAMP)
		WHERE id = ?
	`
	res, err := s.db.ExecContext(ctx, q, IssueStatusPublished, weeklyID)
	if err != nil {
		return fmt.Errorf("mark weekly %d published: %w", weeklyID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("mark weekly %d published: not found", weeklyID)
	}
	return nil
}

func (s *sqliteStore) NextIssueNumber(ctx context.Context, domainID string) (int, error) {
	const q = `SELECT COALESCE(MAX(issue_number), 0) + 1 FROM issues WHERE domain_id = ?`
	var n int
	if err := s.db.QueryRowContext(ctx, q, domainID).Scan(&n); err != nil {
		return 0, fmt.Errorf("next issue number %q: %w", domainID, err)
	}
	return n, nil
}

// -------- IssueItem --------

func (s *sqliteStore) ReplaceIssueItems(ctx context.Context, issueID int64, items []*IssueItem) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM issue_items WHERE issue_id = ?`, issueID); err != nil {
		return fmt.Errorf("delete issue_items %d: %w", issueID, err)
	}

	if len(items) > 0 {
		const q = `
			INSERT INTO issue_items
				(issue_id, section, seq, title, body_md,
				 source_urls_json, raw_item_ids_json,
				 status, validated_at, retry_count, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, COALESCE(?, CURRENT_TIMESTAMP))
		`
		stmt, err := tx.PrepareContext(ctx, q)
		if err != nil {
			return fmt.Errorf("prepare insert issue_items: %w", err)
		}
		defer stmt.Close()
		for _, it := range items {
			var createdAt any
			if !it.CreatedAt.IsZero() {
				createdAt = it.CreatedAt
			}
			status := it.Status
			if status == "" {
				status = SectionStatusValidated
			}
			validatedAt := nullTimePtr(it.ValidatedAt)
			if validatedAt == nil && status == SectionStatusValidated {
				validatedAt = time.Now().UTC()
			}
			if _, err := stmt.ExecContext(ctx,
				issueID, it.Section, it.Seq, it.Title, it.BodyMD,
				it.SourceURLsJSON, it.RawItemIDsJSON,
				status, validatedAt, it.RetryCount, createdAt,
			); err != nil {
				return fmt.Errorf("insert issue_item (issue=%d section=%s seq=%d): %w",
					issueID, it.Section, it.Seq, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit replace issue_items: %w", err)
	}
	return nil
}

func (s *sqliteStore) ListIssueItems(ctx context.Context, issueID int64) ([]*IssueItem, error) {
	const q = `
		SELECT id, issue_id, section, seq, title, body_md,
		       COALESCE(source_urls_json, ''), COALESCE(raw_item_ids_json, ''),
		       created_at,
		       status, validated_at, retry_count
		FROM issue_items
		WHERE issue_id = ?
		ORDER BY section, seq, id
	`
	rows, err := s.db.QueryContext(ctx, q, issueID)
	if err != nil {
		return nil, fmt.Errorf("list issue_items %d: %w", issueID, err)
	}
	defer rows.Close()

	var out []*IssueItem
	for rows.Next() {
		var it IssueItem
		var validatedAt sql.NullTime
		if err := rows.Scan(
			&it.ID, &it.IssueID, &it.Section, &it.Seq, &it.Title, &it.BodyMD,
			&it.SourceURLsJSON, &it.RawItemIDsJSON, &it.CreatedAt,
			&it.Status, &validatedAt, &it.RetryCount,
		); err != nil {
			return nil, fmt.Errorf("scan issue_item: %w", err)
		}
		if validatedAt.Valid {
			t := validatedAt.Time
			it.ValidatedAt = &t
		}
		out = append(out, &it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate issue_items: %w", err)
	}
	return out, nil
}

func (s *sqliteStore) ListIssueItemsByIssueIDs(ctx context.Context, ids []int64) (map[int64][]*IssueItem, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	// v1.0.1 Batch 2.14: 只返回 status='validated' 的 items, 防止未验证
	// (pending/failed/running) 的半成品进入 weekly 聚合 (weekly.go L64 是
	// 本方法的唯一调用者, 用这些 items 生成周报的 DailyBundle).
	q := `SELECT id, issue_id, section, seq, title, body_md,
	       COALESCE(source_urls_json, ''), COALESCE(raw_item_ids_json, ''), created_at,
	       status, validated_at, retry_count
	      FROM issue_items
	      WHERE issue_id IN (` + strings.Join(placeholders, ",") + `)
	        AND status = 'validated'
	      ORDER BY issue_id, section, seq`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list items by ids: %w", err)
	}
	defer rows.Close()
	out := make(map[int64][]*IssueItem)
	for rows.Next() {
		var it IssueItem
		var validatedAt sql.NullTime
		if err := rows.Scan(&it.ID, &it.IssueID, &it.Section, &it.Seq, &it.Title, &it.BodyMD,
			&it.SourceURLsJSON, &it.RawItemIDsJSON, &it.CreatedAt,
			&it.Status, &validatedAt, &it.RetryCount); err != nil {
			return nil, fmt.Errorf("scan item: %w", err)
		}
		if validatedAt.Valid {
			t := validatedAt.Time
			it.ValidatedAt = &t
		}
		out[it.IssueID] = append(out[it.IssueID], &it)
	}
	return out, rows.Err()
}

// -------- IssueInsight --------

func (s *sqliteStore) UpsertIssueInsight(ctx context.Context, insight *IssueInsight) error {
	// v1.0.1: also persist status / validated_at / infocard_status.
	// Defaults: status defaults to 'validated' on new rows (the insight
	// LLM call succeeded if we got here) and infocard_status defaults to
	// 'pending' until the card render stage flips it.
	const q = `
		INSERT INTO issue_insights
			(issue_id, industry_md, our_md, model, temperature, retry_count,
			 generated_at, status, validated_at, infocard_status)
		VALUES (?, ?, ?, ?, ?, ?, COALESCE(?, CURRENT_TIMESTAMP), ?, ?, ?)
		ON CONFLICT(issue_id) DO UPDATE SET
			industry_md = excluded.industry_md,
			our_md = excluded.our_md,
			model = excluded.model,
			temperature = excluded.temperature,
			retry_count = excluded.retry_count,
			generated_at = excluded.generated_at,
			status = excluded.status,
			validated_at = excluded.validated_at,
			infocard_status = CASE
				WHEN excluded.infocard_status = 'pending' AND issue_insights.infocard_status <> 'pending'
					THEN issue_insights.infocard_status
				ELSE excluded.infocard_status
			END
	`
	var generatedAt any
	if !insight.GeneratedAt.IsZero() {
		generatedAt = insight.GeneratedAt
	}
	status := insight.Status
	if status == "" {
		status = SectionStatusValidated
	}
	infocardStatus := insight.InfocardStatus
	if infocardStatus == "" {
		infocardStatus = SectionStatusPending
	}
	validatedAt := nullTimePtr(insight.ValidatedAt)
	if validatedAt == nil && status == SectionStatusValidated {
		validatedAt = time.Now().UTC()
	}
	if _, err := s.db.ExecContext(ctx, q,
		insight.IssueID, insight.IndustryMD, insight.OurMD,
		insight.Model, insight.Temperature, insight.RetryCount, generatedAt,
		status, validatedAt, infocardStatus,
	); err != nil {
		return fmt.Errorf("upsert insight for issue %d: %w", insight.IssueID, err)
	}
	return nil
}

func (s *sqliteStore) GetIssueInsight(ctx context.Context, issueID int64) (*IssueInsight, error) {
	const q = `
		SELECT id, issue_id,
		       COALESCE(industry_md, ''), COALESCE(our_md, ''),
		       COALESCE(model, ''), COALESCE(temperature, 0),
		       retry_count, generated_at,
		       status, validated_at, infocard_status
		FROM issue_insights
		WHERE issue_id = ?
	`
	var in IssueInsight
	var validatedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, q, issueID).Scan(
		&in.ID, &in.IssueID, &in.IndustryMD, &in.OurMD,
		&in.Model, &in.Temperature, &in.RetryCount, &in.GeneratedAt,
		&in.Status, &validatedAt, &in.InfocardStatus,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get insight for issue %d: %w", issueID, err)
	}
	if validatedAt.Valid {
		t := validatedAt.Time
		in.ValidatedAt = &t
	}
	return &in, nil
}

// -------- WeeklyIssue --------

func (s *sqliteStore) UpsertWeeklyIssue(ctx context.Context, w *WeeklyIssue) (int64, error) {
	const q = `
		INSERT INTO weekly_issues
			(domain_id, year, week, start_date, end_date, title,
			 focus_md, signals_md, trends_md, trends_diagram, trends_diagram_detail,
			 takeaways_md, ponder_md,
			 full_md, daily_issue_ids, status, generated_at, published_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(domain_id, year, week) DO UPDATE SET
			start_date = excluded.start_date,
			end_date = excluded.end_date,
			title = excluded.title,
			focus_md = excluded.focus_md,
			signals_md = excluded.signals_md,
			trends_md = excluded.trends_md,
			trends_diagram = excluded.trends_diagram,
			trends_diagram_detail = excluded.trends_diagram_detail,
			takeaways_md = excluded.takeaways_md,
			ponder_md = excluded.ponder_md,
			full_md = excluded.full_md,
			daily_issue_ids = excluded.daily_issue_ids,
			status = excluded.status,
			generated_at = excluded.generated_at,
			published_at = excluded.published_at
		RETURNING id
	`
	status := w.Status
	if status == "" {
		status = IssueStatusDraft
	}
	var id int64
	err := s.db.QueryRowContext(ctx, q,
		w.DomainID, w.Year, w.Week,
		w.StartDate.Format("2006-01-02"), w.EndDate.Format("2006-01-02"),
		w.Title,
		w.FocusMD, w.SignalsMD, w.TrendsMD, w.TrendsDiagram, w.TrendsDiagramDetail,
		w.TakeawaysMD, w.PonderMD,
		w.FullMD, w.DailyIssueIDs, status,
		nullTimePtr(w.GeneratedAt), nullTimePtr(w.PublishedAt),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert weekly %s/%d-W%02d: %w", w.DomainID, w.Year, w.Week, err)
	}
	return id, nil
}

func (s *sqliteStore) GetWeeklyIssue(ctx context.Context, domainID string, year, week int) (*WeeklyIssue, error) {
	const q = `
		SELECT id, domain_id, year, week, start_date, end_date,
		       COALESCE(title, ''), COALESCE(focus_md, ''),
		       COALESCE(signals_md, ''), COALESCE(trends_md, ''),
		       COALESCE(trends_diagram, ''), COALESCE(trends_diagram_detail, ''),
		       COALESCE(takeaways_md, ''), COALESCE(ponder_md, ''),
		       COALESCE(full_md, ''), COALESCE(daily_issue_ids, ''),
		       status, generated_at, published_at
		FROM weekly_issues
		WHERE domain_id = ? AND year = ? AND week = ?
	`
	var w WeeklyIssue
	var generatedAt, publishedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, q, domainID, year, week).Scan(
		&w.ID, &w.DomainID, &w.Year, &w.Week, &w.StartDate, &w.EndDate,
		&w.Title, &w.FocusMD, &w.SignalsMD, &w.TrendsMD, &w.TrendsDiagram, &w.TrendsDiagramDetail,
		&w.TakeawaysMD, &w.PonderMD, &w.FullMD, &w.DailyIssueIDs,
		&w.Status, &generatedAt, &publishedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get weekly %s/%d-W%02d: %w", domainID, year, week, err)
	}
	if generatedAt.Valid {
		t := generatedAt.Time
		w.GeneratedAt = &t
	}
	if publishedAt.Valid {
		t := publishedAt.Time
		w.PublishedAt = &t
	}
	return &w, nil
}

func (s *sqliteStore) ListDailyIssuesByDateRange(ctx context.Context, domainID string, start, end time.Time) ([]*Issue, error) {
	const q = `
		SELECT id, domain_id, issue_date, COALESCE(issue_number, 0),
		       COALESCE(title, ''), COALESCE(summary, ''), status,
		       COALESCE(source_count, 0), COALESCE(item_count, 0),
		       generated_at, published_at
		FROM issues
		WHERE domain_id = ?
		  AND issue_date >= ?
		  AND issue_date <= ?
		  AND (status = ? OR published_at IS NOT NULL)
		ORDER BY issue_date ASC
	`
	rows, err := s.db.QueryContext(ctx, q, domainID,
		start.Format("2006-01-02"), end.Format("2006-01-02"), IssueStatusPublished)
	if err != nil {
		return nil, fmt.Errorf("list issues %s [%s..%s]: %w",
			domainID, start.Format("2006-01-02"), end.Format("2006-01-02"), err)
	}
	defer rows.Close()

	var out []*Issue
	for rows.Next() {
		var is Issue
		var generatedAt, publishedAt sql.NullTime
		if err := rows.Scan(
			&is.ID, &is.DomainID, &is.IssueDate, &is.IssueNumber,
			&is.Title, &is.Summary, &is.Status,
			&is.SourceCount, &is.ItemCount,
			&generatedAt, &publishedAt,
		); err != nil {
			return nil, fmt.Errorf("scan issue: %w", err)
		}
		if generatedAt.Valid {
			t := generatedAt.Time
			is.GeneratedAt = &t
		}
		if publishedAt.Valid {
			t := publishedAt.Time
			is.PublishedAt = &t
		}
		out = append(out, &is)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate issues: %w", err)
	}
	return out, nil
}

func (s *sqliteStore) ListIssueInsightsByIssueIDs(ctx context.Context, ids []int64) (map[int64]*IssueInsight, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	q := `SELECT id, issue_id, COALESCE(industry_md, ''), COALESCE(our_md, ''),
	       COALESCE(model, ''), COALESCE(temperature, 0), retry_count, generated_at,
	       status, validated_at, infocard_status
	      FROM issue_insights WHERE issue_id IN (` + strings.Join(placeholders, ",") + `)`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list insights by ids: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]*IssueInsight)
	for rows.Next() {
		var in IssueInsight
		var validatedAt sql.NullTime
		if err := rows.Scan(&in.ID, &in.IssueID, &in.IndustryMD, &in.OurMD,
			&in.Model, &in.Temperature, &in.RetryCount, &in.GeneratedAt,
			&in.Status, &validatedAt, &in.InfocardStatus); err != nil {
			return nil, fmt.Errorf("scan insight: %w", err)
		}
		if validatedAt.Valid {
			t := validatedAt.Time
			in.ValidatedAt = &t
		}
		out[in.IssueID] = &in
	}
	return out, rows.Err()
}

// -------- Delivery --------

func (s *sqliteStore) InsertDelivery(ctx context.Context, d *Delivery) error {
	const q = `
		INSERT INTO deliveries (issue_id, channel, target, status, response_json, sent_at)
		VALUES (?, ?, ?, ?, ?, COALESCE(?, CURRENT_TIMESTAMP))
	`
	var sentAt any
	if !d.SentAt.IsZero() {
		sentAt = d.SentAt
	}
	if _, err := s.db.ExecContext(ctx, q,
		d.IssueID, d.Channel, d.Target, d.Status, d.ResponseJSON, sentAt,
	); err != nil {
		return fmt.Errorf("insert delivery (issue=%d channel=%s): %w", d.IssueID, d.Channel, err)
	}
	return nil
}

func (s *sqliteStore) ListDeliveries(ctx context.Context, issueID int64) ([]*Delivery, error) {
	const q = `
		SELECT id, issue_id, channel, COALESCE(target, ''), status,
		       COALESCE(response_json, ''), sent_at
		FROM deliveries
		WHERE issue_id = ?
		ORDER BY sent_at, id
	`
	rows, err := s.db.QueryContext(ctx, q, issueID)
	if err != nil {
		return nil, fmt.Errorf("list deliveries %d: %w", issueID, err)
	}
	defer rows.Close()

	var out []*Delivery
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(
			&d.ID, &d.IssueID, &d.Channel, &d.Target, &d.Status, &d.ResponseJSON, &d.SentAt,
		); err != nil {
			return nil, fmt.Errorf("scan delivery: %w", err)
		}
		out = append(out, &d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate deliveries: %w", err)
	}
	return out, nil
}

// -------- helpers --------

func nullString(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func nullInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

func nullTimePtr(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return *t
}

// -------- IssueItem (v1.0.1 per-section state tracking) --------

// ReplaceIssueItemsBySections is the multi-section wrapper around
// UpsertIssueItemBySection. It groups `items` by Section and invokes
// the per-section upsert once for each. Unlike the legacy
// ReplaceIssueItems, sections absent from `items` are NOT deleted —
// they survive to be rerendered later. Callers that want "clean slate
// for every section this run touches" should pass exactly one Set of
// sections (e.g. the five canonical ones) to guarantee determinism.
func (s *sqliteStore) ReplaceIssueItemsBySections(ctx context.Context, issueID int64, items []*IssueItem) error {
	bySection := map[string][]*IssueItem{}
	order := []string{}
	for _, it := range items {
		if it == nil {
			continue
		}
		if _, ok := bySection[it.Section]; !ok {
			order = append(order, it.Section)
		}
		bySection[it.Section] = append(bySection[it.Section], it)
	}
	for _, sec := range order {
		if err := s.UpsertIssueItemBySection(ctx, issueID, sec, bySection[sec]); err != nil {
			return err
		}
	}
	return nil
}

// UpsertIssueItemBySection performs DELETE + INSERT scoped to a single
// (issue, section). Used by compose per-section reruns and
// `briefing repair --section X` so we don't stomp on sections that
// have already succeeded in a prior attempt.
func (s *sqliteStore) UpsertIssueItemBySection(ctx context.Context, issueID int64, section string, items []*IssueItem) error {
	if strings.TrimSpace(section) == "" {
		return fmt.Errorf("upsert issue items by section: empty section name")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM issue_items WHERE issue_id = ? AND section = ?`,
		issueID, section,
	); err != nil {
		return fmt.Errorf("delete issue_items (issue=%d section=%s): %w", issueID, section, err)
	}

	if len(items) > 0 {
		const q = `
			INSERT INTO issue_items
				(issue_id, section, seq, title, body_md,
				 source_urls_json, raw_item_ids_json,
				 status, validated_at, retry_count, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, COALESCE(?, CURRENT_TIMESTAMP))
		`
		stmt, err := tx.PrepareContext(ctx, q)
		if err != nil {
			return fmt.Errorf("prepare insert: %w", err)
		}
		defer stmt.Close()
		for _, it := range items {
			var createdAt any
			if !it.CreatedAt.IsZero() {
				createdAt = it.CreatedAt
			}
			status := it.Status
			if status == "" {
				status = SectionStatusValidated
			}
			validatedAt := nullTimePtr(it.ValidatedAt)
			if validatedAt == nil && status == SectionStatusValidated {
				validatedAt = time.Now().UTC()
			}
			if _, err := stmt.ExecContext(ctx,
				issueID, it.Section, it.Seq, it.Title, it.BodyMD,
				it.SourceURLsJSON, it.RawItemIDsJSON,
				status, validatedAt, it.RetryCount, createdAt,
			); err != nil {
				return fmt.Errorf("insert issue_item (issue=%d section=%s seq=%d): %w",
					issueID, it.Section, it.Seq, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit upsert issue items by section: %w", err)
	}
	return nil
}

// InsertStubIssueItems seeds one 'pending' row per section so the gate
// can distinguish "section not attempted" from "section succeeded with 0
// items". Idempotent: if a stub already exists for (issue, section) we
// skip it (INSERT OR IGNORE on a unique-ish proxy). We don't actually
// have a UNIQUE index on (issue, section, seq=0) yet — we approximate
// by skipping inserts when any row for the section already exists.
func (s *sqliteStore) InsertStubIssueItems(ctx context.Context, issueID int64, sections []string) error {
	if len(sections) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, sec := range sections {
		if strings.TrimSpace(sec) == "" {
			continue
		}
		// Skip if any row already exists for this (issue, section).
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM issue_items WHERE issue_id = ? AND section = ?`,
			issueID, sec,
		).Scan(&n); err != nil {
			return fmt.Errorf("count existing stub: %w", err)
		}
		if n > 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO issue_items
				(issue_id, section, seq, title, body_md,
				 source_urls_json, raw_item_ids_json,
				 status, validated_at, retry_count)
			VALUES (?, ?, 0, '', '', '[]', '[]', ?, NULL, 0)
		`, issueID, sec, SectionStatusPending); err != nil {
			return fmt.Errorf("insert stub issue_item (issue=%d section=%s): %w", issueID, sec, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit insert stubs: %w", err)
	}
	return nil
}

// ListIssueItemsByStatus returns only items matching the given status.
// Pass SectionStatusValidated to retrieve render-ready content.
func (s *sqliteStore) ListIssueItemsByStatus(ctx context.Context, issueID int64, status string) ([]*IssueItem, error) {
	const q = `
		SELECT id, issue_id, section, seq, title, body_md,
		       COALESCE(source_urls_json, ''), COALESCE(raw_item_ids_json, ''),
		       created_at,
		       status, validated_at, retry_count
		FROM issue_items
		WHERE issue_id = ? AND status = ?
		ORDER BY section, seq, id
	`
	rows, err := s.db.QueryContext(ctx, q, issueID, status)
	if err != nil {
		return nil, fmt.Errorf("list issue_items by status %d/%s: %w", issueID, status, err)
	}
	defer rows.Close()
	var out []*IssueItem
	for rows.Next() {
		var it IssueItem
		var validatedAt sql.NullTime
		if err := rows.Scan(
			&it.ID, &it.IssueID, &it.Section, &it.Seq, &it.Title, &it.BodyMD,
			&it.SourceURLsJSON, &it.RawItemIDsJSON, &it.CreatedAt,
			&it.Status, &validatedAt, &it.RetryCount,
		); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		if validatedAt.Valid {
			t := validatedAt.Time
			it.ValidatedAt = &t
		}
		out = append(out, &it)
	}
	return out, rows.Err()
}

// UpdateIssueItemStatus flips an item's status. When status is
// 'validated' we stamp validated_at. Every call bumps retry_count so
// callers can tell "1st attempt succeeded" from "3rd attempt succeeded
// after two repairs".
func (s *sqliteStore) UpdateIssueItemStatus(ctx context.Context, itemID int64, status string) error {
	const q = `
		UPDATE issue_items
		SET status = ?,
		    validated_at = CASE WHEN ? = 'validated' THEN CURRENT_TIMESTAMP ELSE validated_at END,
		    retry_count = retry_count + 1
		WHERE id = ?
	`
	res, err := s.db.ExecContext(ctx, q, status, status, itemID)
	if err != nil {
		return fmt.Errorf("update issue_item %d status: %w", itemID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("update issue_item %d: not found", itemID)
	}
	return nil
}

// -------- IssueInsight (v1.0.1 per-stage state tracking) --------

// MarkInsightValidated stamps status='validated' + validated_at and
// sets infocard_status as a separate dimension (card render stage can
// be 'pending' while text insight is already 'validated').
func (s *sqliteStore) MarkInsightValidated(ctx context.Context, issueID int64, infocardStatus string) error {
	if infocardStatus == "" {
		infocardStatus = SectionStatusPending
	}
	const q = `
		UPDATE issue_insights
		SET status = 'validated',
		    validated_at = CURRENT_TIMESTAMP,
		    infocard_status = ?
		WHERE issue_id = ?
	`
	res, err := s.db.ExecContext(ctx, q, infocardStatus, issueID)
	if err != nil {
		return fmt.Errorf("mark insight validated (issue=%d): %w", issueID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("mark insight validated: issue %d has no insight row", issueID)
	}
	return nil
}

// -------- ClassifiedItem --------

// InsertClassifiedItems persists the output of classify.
//
// Semantics are "replace the full classify snapshot for one issue":
// before inserting the new rows we DELETE all existing classified_items
// for that issue_id inside the same transaction. This prevents stale
// rows from previous reruns from surviving under a different section and
// polluting repair/status/debugging views.
func (s *sqliteStore) InsertClassifiedItems(ctx context.Context, items []*ClassifiedItem) error {
	if len(items) == 0 {
		return nil
	}
	issueID := items[0].IssueID
	for _, ci := range items[1:] {
		if ci != nil && ci.IssueID != issueID {
			return fmt.Errorf("insert classified_items: mixed issue ids %d and %d", issueID, ci.IssueID)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM classified_items WHERE issue_id = ?`, issueID); err != nil {
		return fmt.Errorf("delete classified_items for issue=%d: %w", issueID, err)
	}
	const q = `
		INSERT INTO classified_items
			(issue_id, section, raw_item_id, rank_score, seq, created_at)
		VALUES (?, ?, ?, ?, ?, COALESCE(?, CURRENT_TIMESTAMP))
		ON CONFLICT(issue_id, section, raw_item_id) DO UPDATE SET
			rank_score = excluded.rank_score,
			seq = excluded.seq
	`
	stmt, err := tx.PrepareContext(ctx, q)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()
	for _, ci := range items {
		var createdAt any
		if !ci.CreatedAt.IsZero() {
			createdAt = ci.CreatedAt
		}
		if _, err := stmt.ExecContext(ctx,
			ci.IssueID, ci.Section, ci.RawItemID, ci.RankScore, ci.Seq, createdAt,
		); err != nil {
			return fmt.Errorf("insert classified_item (issue=%d section=%s raw=%d): %w",
				ci.IssueID, ci.Section, ci.RawItemID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit classified_items: %w", err)
	}
	return nil
}

// ListClassifiedItems returns every classify row for an issue, ordered
// by section then seq. Used by repair to re-run compose for one
// section without touching classify.
func (s *sqliteStore) ListClassifiedItems(ctx context.Context, issueID int64) ([]*ClassifiedItem, error) {
	const q = `
		SELECT id, issue_id, section, raw_item_id,
		       COALESCE(rank_score, 0), seq, created_at
		FROM classified_items
		WHERE issue_id = ?
		ORDER BY section, seq, id
	`
	rows, err := s.db.QueryContext(ctx, q, issueID)
	if err != nil {
		return nil, fmt.Errorf("list classified_items %d: %w", issueID, err)
	}
	defer rows.Close()
	var out []*ClassifiedItem
	for rows.Next() {
		var ci ClassifiedItem
		if err := rows.Scan(&ci.ID, &ci.IssueID, &ci.Section, &ci.RawItemID,
			&ci.RankScore, &ci.Seq, &ci.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, &ci)
	}
	return out, rows.Err()
}

// -------- IssueStage --------

// UpdateStageStatus upserts a stage row. The caller supplies the full
// struct; we treat (issue_id, stage, version) as the natural key. If
// Version is 0 we auto-pick next available (max + 1). On update, we
// patch status / completed_at / error_text (other fields are
// set-once at create time).
func (s *sqliteStore) UpdateStageStatus(ctx context.Context, st *IssueStage) error {
	if st.Version <= 0 {
		var maxV sql.NullInt64
		if err := s.db.QueryRowContext(ctx,
			`SELECT MAX(version) FROM issue_stages WHERE issue_id = ? AND stage = ?`,
			st.IssueID, st.Stage,
		).Scan(&maxV); err != nil {
			return fmt.Errorf("query max version (issue=%d stage=%s): %w", st.IssueID, st.Stage, err)
		}
		st.Version = int(maxV.Int64) + 1
	}

	status := st.Status
	if status == "" {
		status = StageStatusRunning
	}

	var startedAt any
	if !st.StartedAt.IsZero() {
		startedAt = st.StartedAt
	}
	completedAt := nullTimePtr(st.CompletedAt)
	inputHash := nullString(st.InputHash)
	parentVersions := nullString(st.ParentVersions)
	errorText := nullString(st.ErrorText)

	// ON CONFLICT update for re-entrant stage tracking: the same run
	// (same issue, stage, version) calls Update multiple times (running
	// -> succeeded/failed). We keep started_at sticky and patch the
	// rest.
	const q = `
		INSERT INTO issue_stages
			(issue_id, stage, status, input_hash, version, parent_versions,
			 started_at, completed_at, error_text)
		VALUES (?, ?, ?, ?, ?, ?, COALESCE(?, CURRENT_TIMESTAMP), ?, ?)
		ON CONFLICT(issue_id, stage, version) DO UPDATE SET
			status = excluded.status,
			input_hash = COALESCE(excluded.input_hash, issue_stages.input_hash),
			parent_versions = COALESCE(excluded.parent_versions, issue_stages.parent_versions),
			completed_at = COALESCE(excluded.completed_at, issue_stages.completed_at),
			error_text = COALESCE(excluded.error_text, issue_stages.error_text)
	`
	if _, err := s.db.ExecContext(ctx, q,
		st.IssueID, st.Stage, status, inputHash, st.Version, parentVersions,
		startedAt, completedAt, errorText,
	); err != nil {
		return fmt.Errorf("upsert stage (issue=%d stage=%s version=%d): %w",
			st.IssueID, st.Stage, st.Version, err)
	}
	return nil
}

// RecoverStaleRunningStages marks every stage row that has been stuck
// in 'running' for longer than `threshold` as 'failed' with an
// error_text indicating recovery. Returns the number of rows updated.
// Called on pipeline start-up so a previously-crashed run doesn't
// block subsequent runs that target the same (issue, stage) key.
func (s *sqliteStore) RecoverStaleRunningStages(ctx context.Context, threshold time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-threshold)
	const q = `
		UPDATE issue_stages
		SET status = 'failed',
		    completed_at = CURRENT_TIMESTAMP,
		    error_text = COALESCE(error_text, '') || CASE WHEN error_text IS NULL OR error_text = ''
		        THEN 'recovered stale running stage (>' || ? || 's)'
		        ELSE ' | recovered stale running stage (>' || ? || 's)' END
		WHERE status = 'running' AND started_at < ?
	`
	secs := int(threshold.Seconds())
	res, err := s.db.ExecContext(ctx, q, secs, secs, cutoff)
	if err != nil {
		return 0, fmt.Errorf("recover stale running stages: %w", err)
	}
	return res.RowsAffected()
}

// -------- LLMCall --------

// InsertLLMCall appends one row to the audit log. Never returns a
// domain error — log-on-failure is the caller's responsibility.
func (s *sqliteStore) InsertLLMCall(ctx context.Context, call *LLMCall) error {
	const q = `
		INSERT INTO llm_calls
			(issue_id, stage, model, prompt_hash,
			 request_tokens, response_tokens, latency_ms,
			 http_status, error_text, called_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, COALESCE(?, CURRENT_TIMESTAMP))
	`
	var issueID any
	if call.IssueID != nil && *call.IssueID > 0 {
		issueID = *call.IssueID
	}
	var calledAt any
	if !call.CalledAt.IsZero() {
		calledAt = call.CalledAt
	}
	if _, err := s.db.ExecContext(ctx, q,
		issueID, call.Stage, call.Model, nullString(call.PromptHash),
		call.RequestTokens, call.ResponseTokens, call.LatencyMS,
		call.HTTPStatus, nullString(call.ErrorText), calledAt,
	); err != nil {
		return fmt.Errorf("insert llm_call (stage=%s model=%s): %w", call.Stage, call.Model, err)
	}
	return nil
}

// -------- SelfHeal --------

// SelfHealPublishStatus flips an issue.status back to 'published' if
// we have delivery evidence on slack_prod. Call this on orchestrator
// pre-flight — it plugs the gap created by Bug K on databases that
// have already been corrupted.
func (s *sqliteStore) SelfHealPublishStatus(ctx context.Context, domainID string, date time.Time) (int64, error) {
	const q = `
		UPDATE issues
		SET status = 'published',
		    published_at = COALESCE(published_at, CURRENT_TIMESTAMP)
		WHERE domain_id = ?
		  AND issue_date = ?
		  AND status = 'generated'
		  AND EXISTS (
		      SELECT 1 FROM deliveries
		      WHERE deliveries.issue_id = issues.id
		        AND deliveries.channel = 'slack_prod'
		        AND deliveries.status  = 'sent'
		  )
	`
	res, err := s.db.ExecContext(ctx, q, domainID, date.Format("2006-01-02"))
	if err != nil {
		return 0, fmt.Errorf("self-heal publish status %s/%s: %w",
			domainID, date.Format("2006-01-02"), err)
	}
	return res.RowsAffected()
}

// -------- SourceHealth (v1.0.1 Phase 1.3) --------

// UpsertSourceHealth records one ingest result for a given source.
//   - success=true  → last_success_at := NOW, consecutive_failures := 0
//   - success=false → last_error_at   := NOW, consecutive_failures += 1
//
// 读旧行为增量 consecutive_failures (SQLite 原生没 "UPDATE ... + 1" on
// conflict 的 UPSERT 直接语法, 简单清晰起见分两步: SELECT 老 row + INSERT
// OR REPLACE). 失败时保留 last_success_at (仅更新错误字段).
func (s *sqliteStore) UpsertSourceHealth(ctx context.Context, sourceID int64, success bool, errorText string, itemCount int) error {
	// Step 1: 读老行 (consecutive_failures 增量需要).
	var (
		prevConsecFails  int
		prevSuccessAt    sql.NullTime
		prevErrorAt      sql.NullTime
		prevErrorText    sql.NullString
		prevAutoDisabled int
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT consecutive_failures, last_success_at, last_error_at, last_error_text, auto_disabled
		 FROM source_health WHERE source_id = ?`,
		sourceID,
	).Scan(&prevConsecFails, &prevSuccessAt, &prevErrorAt, &prevErrorText, &prevAutoDisabled)
	isNew := errors.Is(err, sql.ErrNoRows)
	if err != nil && !isNew {
		return fmt.Errorf("source_health query (source=%d): %w", sourceID, err)
	}

	now := time.Now().UTC()
	var (
		lastSuccessAt sql.NullTime
		lastErrorAt   sql.NullTime
		lastErrorTxt  sql.NullString
		consecFails   int
	)

	if success {
		lastSuccessAt = sql.NullTime{Time: now, Valid: true}
		// 保留老的 error 字段以便 debug, 只是 consecutive 归零.
		lastErrorAt = prevErrorAt
		lastErrorTxt = prevErrorText
		consecFails = 0
	} else {
		// 失败: 保留老的 last_success_at, 推进 error 字段.
		lastSuccessAt = prevSuccessAt
		lastErrorAt = sql.NullTime{Time: now, Valid: true}
		lastErrorTxt = sql.NullString{String: errorText, Valid: errorText != ""}
		consecFails = prevConsecFails + 1
	}

	// Step 2: upsert (REPLACE 在 SQLite 走 INSERT OR REPLACE semantics).
	const up = `
		INSERT INTO source_health (
		    source_id, last_success_at, last_error_at,
		    consecutive_failures, last_error_text, last_item_count,
		    auto_disabled, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_id) DO UPDATE SET
		    last_success_at      = excluded.last_success_at,
		    last_error_at        = excluded.last_error_at,
		    consecutive_failures = excluded.consecutive_failures,
		    last_error_text      = excluded.last_error_text,
		    last_item_count      = excluded.last_item_count,
		    updated_at           = excluded.updated_at
		    -- auto_disabled 不在此处 upsert, 留给未来告警策略单独 toggle
	`
	_, err = s.db.ExecContext(ctx, up,
		sourceID,
		lastSuccessAt, lastErrorAt,
		consecFails, lastErrorTxt, itemCount,
		prevAutoDisabled, now,
	)
	if err != nil {
		return fmt.Errorf("source_health upsert (source=%d): %w", sourceID, err)
	}
	_ = isNew // reserved for future metrics
	return nil
}

// ListSourceHealth returns every source_health row, ordered by
// consecutive_failures DESC (sickest sources on top) then source_id.
func (s *sqliteStore) ListSourceHealth(ctx context.Context) ([]*SourceHealth, error) {
	const q = `
		SELECT source_id, last_success_at, last_error_at,
		       consecutive_failures, COALESCE(last_error_text, ''),
		       last_item_count, auto_disabled, updated_at
		FROM source_health
		ORDER BY consecutive_failures DESC, source_id ASC
	`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list source_health: %w", err)
	}
	defer rows.Close()

	out := []*SourceHealth{}
	for rows.Next() {
		var sh SourceHealth
		var (
			successAt sql.NullTime
			errorAt   sql.NullTime
			disabled  int
		)
		if err := rows.Scan(
			&sh.SourceID, &successAt, &errorAt,
			&sh.ConsecutiveFailures, &sh.LastErrorText,
			&sh.LastItemCount, &disabled, &sh.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan source_health: %w", err)
		}
		if successAt.Valid {
			t := successAt.Time
			sh.LastSuccessAt = &t
		}
		if errorAt.Valid {
			t := errorAt.Time
			sh.LastErrorAt = &t
		}
		sh.AutoDisabled = disabled != 0
		out = append(out, &sh)
	}
	return out, rows.Err()
}
