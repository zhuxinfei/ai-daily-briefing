-- ---------------------------------------------------------------------------
-- Index classified_items(raw_item_id) — 2026-09
--
-- PruneRawItems deletes from raw_items, which is the parent of
-- classified_items.raw_item_id. With foreign_keys=1 SQLite verifies that
-- constraint for every deleted parent row, and with no index on the child key
-- that check is a full scan of classified_items per row deleted.
--
-- Measured through the production driver on the production schema, deleting
-- 20k raw_items:
--
--     child rows   no index   with this index
--     1,400         0.83 s        0.097 s
--     30,000        7.12 s        0.104 s
--
-- So without it the daily prune gets linearly slower every day as history
-- accumulates, and the cost is paid on every run forever. SQLite's own
-- foreign-keys documentation recommends indexing child key columns for exactly
-- this reason, so this also speeds up any future parent delete or update.
-- ---------------------------------------------------------------------------

CREATE INDEX IF NOT EXISTS idx_classified_items_raw_item
    ON classified_items(raw_item_id);
