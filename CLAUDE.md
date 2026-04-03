# podproxy — CLAUDE.md

## Project Overview

A self-hosted Go podcast caching proxy. Podcast apps add a proxied RSS URL; the server rewrites enclosure URLs to point at itself, then stream-caches episodes to disk on first play. Future plays (and other devices) are served from local cache.

All four build phases are complete. Current work is post-phase-4 improvements.

## Architecture

```
Podcast App → /feeds/:id.rss       → rewritten RSS with proxy URLs
Podcast App → /episodes/:feed/:ep  → write-through stream cache
```

**Key subsystems:**

| Package | Role |
|---|---|
| `internal/config` | YAML config loading with defaults |
| `internal/db` | SQLite CRUD for feeds and episodes |
| `internal/feed` | RSS fetching/parsing, background poller, prefetch worker pool |
| `internal/proxy` | Episode handler: cache hit → `http.ServeContent`; miss → `io.TeeReader` stream-while-caching |
| `internal/api` | REST API handlers (`/api/feeds`, `/health`) |
| `internal/ui` | HTMX web UI with embedded HTML templates |

## Data Model

Two SQLite tables: `feeds` and `episodes`. Episode `cache_status` is one of: `none`, `in_progress`, `cached`, `failed`.

Episode files live at `{cache_dir}/episodes/{feed_id}/{url_id}`. Feed XML is cached at `{cache_dir}/feeds/{feed_id}.xml`.

## API Endpoints

```
POST   /api/feeds                  Add a feed by RSS URL
GET    /api/feeds                  List all feeds with stats
DELETE /api/feeds/:id              Remove feed (+ optionally purge files)
POST   /api/feeds/:id/refresh      Force RSS re-fetch
POST   /api/feeds/:id/prefetch     Queue uncached episodes for background download
GET    /feeds/:id.rss              Proxied RSS feed (used by podcast apps)
GET    /episodes/:feed_id/:ep_id   Episode audio proxy / cache server
GET    /health                     Server status and disk usage
GET    /ui                         HTMX web interface
```

## Build & Run

```bash
go build -o podproxy .
./podproxy                     # looks for config.yaml in CWD; falls back to defaults
```

Config file is optional — defaults work for local development (port 8080, `./cache`, `./data`).

## Docker

```bash
cp deploy/config.yaml.example config.yaml   # edit base_url
docker compose -f deploy/docker-compose.yml up -d
```

Volumes: `podproxy-data` (SQLite) and `podproxy-cache` (episode audio). Dockerfile is multi-stage Alpine; CGO is disabled (pure Go SQLite via `modernc.org/sqlite`).

## Key Implementation Details

- **Write-through caching:** `io.TeeReader` streams origin response to client and disk simultaneously. Never buffers full file in memory.
- **Range requests on uncached episodes:** Range is proxied to the client; a background goroutine fetches and caches the full file.
- **Concurrency:** A `sync.Map` of per-episode mutexes prevents duplicate concurrent writes.
- **Prefetch worker pool:** Bounded channel + N goroutines (default 2). Retries with 5s/30s backoff.
- **Feed poller:** Single goroutine ticks every minute, checks which feeds are past their `refresh_interval_minutes`.

## Tech Stack

- **Language:** Go (single static binary)
- **DB:** SQLite via `modernc.org/sqlite` (no CGO)
- **RSS:** `github.com/mmcdole/gofeed`
- **UI:** Vanilla HTML + HTMX (no build step)
- **Config:** `gopkg.in/yaml.v3`
