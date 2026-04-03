# Podcast Caching Proxy — Project Plan

## Overview

A self-hosted server that acts as a transparent caching proxy between your podcast app and the original RSS feeds. You add a feed once, get back a proxied RSS URL, add that to any standard podcast app, and the server automatically caches every episode you play to local disk. No special client required.

### Design Philosophy

> The best way to ensure your backup stays current is to consume content *from* the backup.

The server sits invisibly in the middle. Your podcast app behaves exactly as normal — it just hits your server instead of the origin CDN. The caching is a side effect of normal playback.

```
Podcast App  ──►  Proxy Server  ──►  Original CDN / RSS Feed
                       │
                  Local Filesystem (NAS)
```

---

## User-Facing Flow

1. `POST /api/feeds` with an RSS URL → server returns a proxied feed URL
2. Add the proxied URL to any standard podcast app
3. Play episodes normally — the server caches them to disk on first play
4. Future plays (or other devices) are served from the local cache
5. Optional: configure auto-prefetch so new episodes are cached before you even open the app

---

## Technology Stack

| Concern | Choice | Rationale |
|---|---|---|
| Language | **Go** | Single deployable binary, excellent stdlib HTTP with native range request support, `io.TeeReader` makes stream-while-saving trivial, strong concurrency primitives |
| Database | **SQLite** (`modernc.org/sqlite`) | Zero external dependencies, perfect for this data volume |
| RSS Parsing | `github.com/mmcdole/gofeed` | Best Go RSS/Atom parser, handles podcast namespace extensions |
| Web UI | **Vanilla HTML + HTMX** | No build step, no Node toolchain on the server, served by the same binary |
| Config | Single `config.yaml` | Simple, version-controllable |
| Deployment | **Docker / docker-compose** | Easy NAS deployment (Synology, TrueNAS, etc.) |

---

## Data Model

```sql
-- feeds
CREATE TABLE feeds (
    id                       TEXT PRIMARY KEY,  -- slug, e.g. "darknet-diaries"
    title                    TEXT,
    original_url             TEXT NOT NULL,     -- the real RSS feed URL
    last_fetched_at          DATETIME,
    refresh_interval_minutes INT DEFAULT 60,
    auto_prefetch            BOOLEAN DEFAULT FALSE
);

-- episodes
CREATE TABLE episodes (
    id           TEXT PRIMARY KEY,              -- namespaced guid: "{feed_id}/{rss_guid}"
    feed_id      TEXT NOT NULL REFERENCES feeds(id),
    title        TEXT,
    original_url TEXT NOT NULL,                 -- original enclosure URL (after redirect resolution)
    cached_path  TEXT,                          -- absolute path on disk, NULL if not cached
    pub_date     DATETIME,
    duration_sec INT,
    size_bytes   INT,
    cache_status TEXT DEFAULT 'none'            -- 'none' | 'in_progress' | 'cached' | 'failed'
);
```

---

## API

### Feed Management

```
POST   /api/feeds          Body: { "url": "https://..." }
                           Returns: { "id": "slug", "proxy_url": "https://your-server/feeds/slug.rss" }

GET    /api/feeds          Returns: list of all feeds with episode counts and cache stats

DELETE /api/feeds/:id      Removes feed and optionally purges cached files

POST   /api/feeds/:id/refresh    Force re-fetch of RSS feed
POST   /api/feeds/:id/prefetch   Queue all un-cached episodes for background download
```

### Proxied Feed (added to podcast app)

```
GET    /feeds/:feed_id.rss
```

Returns a rewritten copy of the original RSS feed where every `<enclosure>` URL has been replaced with a URL pointing back to this server.

### Episode Proxy (injected into rewritten RSS)

```
GET    /episodes/:feed_id/:episode_id
```

Serves the episode audio. Handles HTTP Range requests. On first request, fetches from origin while simultaneously streaming to the client and writing to disk (write-through cache). On subsequent requests, serves directly from disk.

### Optional UI

```
GET    /ui     Simple HTMX web interface: add feeds, view episode cache status, trigger actions
GET    /health Returns server status and disk usage stats
```

---

## Key Subsystems

### 1. RSS Rewriter

When a client requests `/feeds/:id.rss`:

1. Return cached feed XML if fresh (within `refresh_interval_minutes`)
2. Otherwise: fetch original RSS, parse it, rewrite all media URLs
3. URL rewriting targets:
   - `<enclosure url="...">` (standard)
   - `<media:content url="...">` (media RSS)
   - `<podcast:chapters url="...">` (podcast namespace)
4. Serve the modified XML with correct `Content-Type: application/rss+xml`

Rewritten URL pattern: `https://your-server/episodes/{feed_id}/{episode_id}`

### 2. Write-Through Stream Proxy

This is the core of the project. Handler logic for `GET /episodes/:feed_id/:ep_id`:

```
Request arrives
│
├─► Episode cached_status == 'cached'?
│       YES → http.ServeContent(w, r, path, modTime, file)  // handles Range natively
│
└─► NOT cached
        │
        ├─► Another goroutine already fetching this episode?
        │       YES → proxy directly from origin (don't double-write)
        │
        └─► Acquire fetch lock for this episode ID
                │
                Fetch from original URL (follow redirects)
                │
                io.TeeReader(originResponse.Body, diskFileWriter)
                │
                ├──► io.Copy(responseWriter, teeReader)   // stream to client
                └──► diskFileWriter                        // write to cache simultaneously
                │
                On EOF: mark episode as 'cached' in DB, release lock
                On error: mark as 'failed', delete partial file
```

**Range request edge case:** Podcast apps commonly issue `Range: bytes=0-1` as a probe before the real request, or seek mid-episode. Strategy:
- If the file is already cached: `http.ServeContent` handles all range semantics correctly
- If it's the first fetch and a Range header is present: respond to the range for the client, but launch a background goroutine to fetch and cache the full file so future seeks are local

### 3. Background Feed Poller

A goroutine ticker per feed (or a single scheduler):

```
Every refresh_interval_minutes for each feed:
    1. Fetch and parse RSS
    2. Upsert new episodes by GUID (never delete, GUIDs are stable)
    3. If auto_prefetch == true: enqueue new episodes for background download
```

Background download worker pool (configurable concurrency, e.g. 2 workers):
- Pulls from a channel of episode IDs to prefetch
- Uses the same write-through fetch logic as the stream proxy
- Respects a `prefetch_max_age_days` config to avoid downloading ancient back-catalog

---

## Project Structure

```
podcast-proxy/
├── main.go
├── config.yaml
├── go.mod
├── go.sum
│
├── internal/
│   ├── config/
│   │   └── config.go          # YAML config loading and defaults
│   │
│   ├── db/
│   │   ├── db.go              # SQLite connection and migrations
│   │   ├── feeds.go           # Feed CRUD
│   │   └── episodes.go        # Episode CRUD
│   │
│   ├── feed/
│   │   ├── fetcher.go         # HTTP fetch + parse RSS with gofeed
│   │   ├── rewriter.go        # Rewrite enclosure URLs in parsed feed
│   │   └── poller.go          # Background poll loop
│   │
│   ├── proxy/
│   │   ├── handler.go         # HTTP handler for /episodes/:feed/:ep
│   │   ├── cache.go           # Cache hit/miss logic, file path helpers
│   │   └── fetch.go           # Write-through stream fetch with TeeReader
│   │
│   ├── api/
│   │   └── routes.go          # Feed management API handlers
│   │
│   └── ui/
│       ├── handler.go         # HTMX UI handlers
│       └── templates/         # HTML templates
│           ├── base.html
│           ├── feeds.html
│           └── episodes.html
│
├── cache/                     # Default cache directory (gitignored)
├── data/                      # SQLite DB location (gitignored)
│
└── deploy/
    ├── Dockerfile
    └── docker-compose.yml
```

---

## Configuration (`config.yaml`)

```yaml
server:
  port: 8080
  base_url: "http://your-server:8080"  # used to construct proxied URLs

storage:
  cache_dir: "./cache"
  data_dir: "./data"

defaults:
  refresh_interval_minutes: 60
  auto_prefetch: false
  prefetch_max_age_days: 30      # don't prefetch episodes older than this
  prefetch_concurrency: 2        # simultaneous background downloads
```

---

## Build Phases

### Phase 1 — Working Proxy (no caching yet)
Goal: A usable RSS proxy you can actually add to a podcast app by end of phase.

- [ ] Project scaffold: Go modules, config loading, SQLite init + migrations
- [ ] `POST /api/feeds` — fetch RSS, slugify title as ID, store feed + episodes in DB
- [ ] `GET /feeds/:id.rss` — fetch original feed, rewrite enclosure URLs, serve XML
- [ ] `GET /episodes/:feed_id/:ep_id` — pure proxy, no caching (just forward to origin)
- [ ] `GET /api/feeds` — list feeds

**Milestone:** Add a proxied feed to a real podcast app and play an episode end-to-end.

---

### Phase 2 — Write-Through Caching
Goal: Episodes are silently cached on first play.

- [ ] `io.TeeReader` stream proxy with simultaneous disk write
- [ ] In-memory fetch lock map to prevent duplicate concurrent downloads
- [ ] Cache hit detection + `http.ServeContent` for locally cached files
- [ ] Correct `Content-Length` and `Content-Type` headers on cached responses
- [ ] Handle Range requests on un-cached episodes (proxy range to client, background-fetch full file)
- [ ] Background feed poll loop (goroutine ticker per feed)
- [ ] `POST /api/feeds/:id/refresh` endpoint

**Milestone:** Play an episode, then play it again — second play is served from disk.

---

### Phase 3 — Auto-Prefetch & Polish
Goal: New episodes appear in cache before you hit play.

- [ ] Worker pool for background prefetch (bounded channel + N goroutines)
- [ ] `auto_prefetch` config flag, `prefetch_max_age_days` limit
- [ ] `POST /api/feeds/:id/prefetch` to manually trigger
- [ ] Graceful handling of failed downloads (retry with backoff, mark 'failed' in DB)
- [ ] Partial file cleanup on interrupted downloads
- [ ] Follow redirects when resolving enclosure URLs (many CDNs redirect for analytics)

---

### Phase 4 — Web UI & Deployment
Goal: Zero-friction deployment on a NAS.

- [ ] HTMX web UI: feed list, per-feed episode list with cache status indicators
- [ ] Add feed via UI, delete feed via UI, trigger refresh/prefetch via UI
- [ ] Disk usage stats on health/UI page
- [ ] `Dockerfile` (single-stage, scratch or distroless base)
- [ ] `docker-compose.yml` with volume mounts for cache and data dirs
- [ ] README with NAS deployment instructions (Synology, TrueNAS, generic Docker)

---

## Known Tricky Bits

| Issue | Mitigation |
|---|---|
| Range requests on first fetch | Detect Range header; proxy range to client, full-fetch in background goroutine |
| Duplicate concurrent requests for un-cached episode | `sync.Map` of `episodeID → *sync.Mutex`, lock before fetch, unlock on completion |
| RSS GUID uniqueness | GUIDs are not globally unique; namespace as `{feed_id}/{guid}` in DB and file paths |
| CDN redirects on enclosure URLs | `http.Client` with redirect following; store the *final* URL in DB as `original_url` |
| Large files (2–3 hour episodes) | Never buffer in memory; always `io.TeeReader` stream-to-disk |
| Feed returns gzip'd XML | Go's `http.Client` with `Accept-Encoding: gzip` handles this transparently |
| Podcast app sends conditional GET (`If-None-Match`) | Return `304 Not Modified` for the RSS feed when feed hasn't changed |

---

## Out of Scope (for now)

- Transcoding / format conversion
- Automatic episode deletion / storage quota management (manual purge via API is fine)
- Multi-user / auth (single-user self-hosted tool; put it behind a VPN or Tailscale)
- Chapter markers, transcript proxying (episode audio only for now)
