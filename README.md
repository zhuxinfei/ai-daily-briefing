# Briefing v3

Self-hosted, domain-parametric daily briefing system. First case: AI industry.

## Status

Day 1 MVP in progress. See `docs/specs/2026-04-10-briefing-v3-design.md`.

## Architecture

Single Go binary + single SQLite file. No Docker, Redis, Postgres, or external services.

- `cmd/briefing/` — CLI entry with subcommands (run / export / migrate)
- `internal/store/` — SQLite data layer + migrations
- `internal/ingest/` — data source adapters (GitHub Trending, RSS, Folo, ...)
- `internal/extract/` — source article body extraction
- `internal/compose/` — raw items → issue items (classify + group into sections)
- `internal/generate/` — LLM insight and takeaways generation
- `internal/render/` — output formatting (Slack blocks / Feishu doc blocks / HTML)
- `internal/publish/` — distribution channels (Slack / Feishu doc / Feishu bot)
- `config/` — YAML configs per domain
- `data/briefing.db` — SQLite main store (not in git)
- `migrations/` — SQL schema migrations
- `scripts/` — ops helpers (backfill, cron entry)
- `docs/specs/` — design documents

## Domains

- Current: `ai`
- Future: `web3`, `biotech`, ... (add `config/{domain}.yaml` + rows in `domains` / `sources` tables, no code change)

## Run

```bash
# build
go build -o bin/briefing ./cmd/briefing

# run pipeline for today
./bin/briefing run --domain ai

# run for specific date
./bin/briefing run --domain ai --date 2026-04-11
```

## Disaster recovery

The 中转 system at `/root/CloudFlare-AI-Insight-Daily/` remains untouched as a fallback. Tag `stable-2026-04-10-pre-briefing-v3` in that repo marks the safe rollback point.
