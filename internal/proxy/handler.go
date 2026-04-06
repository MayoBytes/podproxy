package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"podproxy/internal/config"
	"podproxy/internal/db"
	"podproxy/internal/feed"
)

type handler struct {
	db         *db.DB
	fetcher    *feed.Fetcher
	prefetcher *feed.Prefetcher
	cfg        *config.Config
	client     *http.Client
	fetchLocks sync.Map // key: episode.ID → struct{}; guards concurrent cache writes
}

func RegisterRoutes(mux *http.ServeMux, database *db.DB, fetcher *feed.Fetcher, prefetcher *feed.Prefetcher, cfg *config.Config) {
	h := &handler{
		db:         database,
		fetcher:    fetcher,
		prefetcher: prefetcher,
		cfg:        cfg,
		// Shared client with a header timeout so a slow CDN can't hang a goroutine
		// forever. WriteTimeout on the server is 0 (streaming), so we guard the
		// outbound side here instead.
		//
		// HTTP/2 is disabled deliberately: multiplexing concurrent episode requests
		// over a single TCP connection causes some CDNs (e.g. Cloudflare-backed
		// hosts) to reset the connection when they detect multiple authenticated
		// streams, failing all in-flight requests. HTTP/1.1 gives each request its
		// own connection and avoids this.
		client: &http.Client{
			Transport: &http.Transport{
				ResponseHeaderTimeout: 30 * time.Second,
				TLSNextProto:          make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
			},
		},
	}

	mux.HandleFunc("GET /feeds/", h.serveFeed)
	mux.HandleFunc("GET /episodes/{feed_id}/{ep_id}", h.serveEpisode)
	mux.HandleFunc("HEAD /episodes/{feed_id}/{ep_id}", h.serveEpisode)
}

// serveFeed handles GET /feeds/{feed_id}.rss
// Serves a rewritten copy of the original RSS with enclosure URLs replaced by
// proxy URLs. The rewritten XML is cached on disk and refreshed only when the
// feed's refresh_interval_minutes has elapsed.
func (h *handler) serveFeed(w http.ResponseWriter, r *http.Request) {
	// Extract feed ID from /feeds/{feed_id}.rss — ServeMux can't wildcard mid-segment.
	segment := r.URL.Path[len("/feeds/"):]
	feedID := strings.TrimSuffix(segment, ".rss")
	if feedID == segment { // no .rss suffix
		http.NotFound(w, r)
		return
	}

	f, err := h.db.GetFeed(feedID)
	if errors.Is(err, db.ErrNotFound) {
		http.Error(w, "feed not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	cachePath := filepath.Join(h.cfg.Storage.CacheDir, "feeds", feedID+".rss")
	staleDur := time.Duration(f.RefreshIntervalMinutes) * time.Minute

	// Serve from disk cache if the feed is still fresh AND the cache file was
	// written after the last DB refresh. The poller and UI refresh handlers update
	// LastFetchedAt without rewriting the disk cache, so a stale file (mtime
	// before LastFetchedAt) would omit proxy URLs for newly-discovered episodes.
	if f.LastFetchedAt != nil && time.Since(*f.LastFetchedAt) < staleDur {
		if info, err := os.Stat(cachePath); err == nil && !info.ModTime().Before(*f.LastFetchedAt) {
			if data, err := os.ReadFile(cachePath); err == nil {
				log.Printf("feed cache hit %s", feedID)
				w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				w.Write(data)
				return
			}
		}
		// Cache file missing or older than the last DB refresh — fall through to re-fetch.
		log.Printf("feed cache stale or missing for %s, re-fetching", feedID)
	}

	// Fetch fresh XML from origin. Fetch also parses episodes so we can upsert
	// any new ones (making them rewritable immediately).
	result, err := h.fetcher.Fetch(feedID, f.OriginalURL)
	if err != nil {
		log.Printf("fetch feed %s: %v", feedID, err)
		http.Error(w, "failed to fetch feed", http.StatusBadGateway)
		return
	}

	for _, ep := range result.Episodes {
		if err := h.db.UpsertEpisode(ep); err != nil {
			log.Printf("upsert episode %s: %v", ep.ID, err)
		}
	}
	if err := h.db.UpdateFeedFetchedAt(feedID, time.Now()); err != nil {
		log.Printf("update fetched_at %s: %v", feedID, err)
	}

	// Build a map of original enclosure URL → url_id for rewriting.
	episodes, err := h.db.ListEpisodesByFeed(feedID)
	if err != nil {
		log.Printf("list episodes for %s: %v", feedID, err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	urlMap := make(map[string]string, len(episodes))
	for _, ep := range episodes {
		urlMap[ep.OriginalURL] = ep.URLID
	}

	rewritten := feed.RewriteXML(result.RawXML, feedID, urlMap, h.cfg.Server.BaseURL)

	// Persist the rewritten XML so subsequent requests within the refresh window
	// are served from disk.
	if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
		log.Printf("mkdirall feed cache %s: %v", feedID, err)
	} else if err := os.WriteFile(cachePath, rewritten, 0644); err != nil {
		log.Printf("write feed cache %s: %v", feedID, err)
	}

	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(rewritten)))
	w.Write(rewritten)
}

// serveEpisode handles GET|HEAD /episodes/{feed_id}/{ep_id}
//
// Phase 2 caching logic:
//   - Cached episode  → http.ServeContent (handles Range natively)
//   - HEAD request    → direct proxy (no caching attempt)
//   - Range request (uncached, lock acquired) → proxy range to client + background full fetch
//   - Another goroutine already caching → direct proxy
//   - Normal GET (uncached, lock acquired) → write-through: TeeReader to client + disk
func (h *handler) serveEpisode(w http.ResponseWriter, r *http.Request) {
	feedID := r.PathValue("feed_id")
	epID := strings.TrimSuffix(r.PathValue("ep_id"), filepath.Ext(r.PathValue("ep_id")))

	ep, err := h.db.GetEpisodeByURLID(feedID, epID)
	if errors.Is(err, db.ErrNotFound) {
		http.Error(w, "episode not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	// Fast path: episode is fully cached on disk.
	if ep.CacheStatus == "cached" && ep.CachedPath != "" {
		log.Printf("proxy: cache hit %s/%s", feedID, epID)
		h.serveCachedEpisode(w, r, ep)
		return
	}

	// In-progress: prefetcher is already downloading this episode — proxy directly
	// rather than starting a competing write-through fetch.
	if ep.CacheStatus == "in_progress" {
		log.Printf("proxy: episode %s/%s in_progress, proxying directly", feedID, epID)
		h.proxyDirect(w, r, ep)
		return
	}

	// HEAD on an uncached episode: proxy headers from origin, no caching.
	if r.Method == http.MethodHead {
		h.proxyDirect(w, r, ep)
		return
	}

	// Try to become the goroutine responsible for caching this episode.
	if !h.tryFetchLock(ep.ID) {
		// Another goroutine is already writing this episode to disk — proxy directly.
		log.Printf("proxy: episode %s/%s locked by concurrent fetch, proxying directly", feedID, epID)
		h.proxyDirect(w, r, ep)
		return
	}

	if r.Header.Get("Range") != "" {
		// Range request: proxy the range to the client now. The lock is held
		// for the duration of proxyDirect (which may take a while for large
		// ranges), then backgroundFetch runs. Other clients hitting this episode
		// concurrently will see the lock and fall through to proxyDirect themselves.
		log.Printf("proxy: range request for uncached %s/%s, proxying range + background fetch", feedID, epID)
		h.proxyDirect(w, r, ep)
		go func() {
			defer h.releaseFetchLock(ep.ID)
			h.backgroundFetch(ep)
		}()
		return
	}

	// Write-through: stream to the client and write to disk simultaneously.
	// If the origin closes the connection early, enqueue the episode in the
	// prefetcher so it is retried in the background with backoff.
	log.Printf("proxy: write-through fetch %s/%s", feedID, epID)
	needsRetry := h.writeThroughFetch(w, r, ep)
	h.releaseFetchLock(ep.ID)
	if needsRetry {
		h.prefetcher.Enqueue(ep)
	}
}

// serveCachedEpisode serves an episode from the local disk cache.
// http.ServeContent handles all Range / conditional-GET semantics.
func (h *handler) serveCachedEpisode(w http.ResponseWriter, r *http.Request, ep *db.Episode) {
	f, err := os.Open(ep.CachedPath)
	if err != nil {
		// Cache file disappeared — reset status so the next request re-fetches.
		log.Printf("proxy: cached file missing for %s, resetting to none: %v", ep.ID, err)
		_ = h.db.UpdateEpisodeCacheStatus(ep.ID, "none", nil, 0, "")
		h.proxyDirect(w, r, ep)
		return
	}
	defer f.Close()

	if ep.ContentType != "" {
		w.Header().Set("Content-Type", ep.ContentType)
	}

	var modTime time.Time
	if ep.PubDate != nil {
		modTime = *ep.PubDate
	}
	http.ServeContent(w, r, filepath.Base(ep.CachedPath), modTime, f)
}

// proxyDirect forwards the request (including any Range header) straight to the
// origin CDN without touching the cache.
func (h *handler) proxyDirect(w http.ResponseWriter, r *http.Request, ep *db.Episode) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, ep.OriginalURL, nil)
	if err != nil {
		http.Error(w, "bad origin URL", http.StatusInternalServerError)
		return
	}
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		log.Printf("proxy episode %s/%s: %v", ep.FeedID, ep.URLID, err)
		http.Error(w, "failed to fetch episode from origin", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for _, key := range []string{
		"Content-Type", "Content-Length", "Content-Range",
		"Accept-Ranges", "Last-Modified", "ETag",
	} {
		if v := resp.Header.Get(key); v != "" {
			w.Header().Set(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		if _, err := io.Copy(w, resp.Body); err != nil {
			log.Printf("stream episode %s/%s: %v", ep.FeedID, ep.URLID, err)
		}
	}
}

// writeThroughFetch fetches from origin, streams to the client, and writes to
// disk simultaneously via io.TeeReader. On success the episode is marked cached.
// Returns true if the origin closed the connection early and the episode should
// be re-queued in the prefetcher for a background retry.
func (h *handler) writeThroughFetch(w http.ResponseWriter, r *http.Request, ep *db.Episode) (needsRetry bool) {
	cachePath := h.episodeCachePath(ep)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, ep.OriginalURL, nil)
	if err != nil {
		http.Error(w, "bad origin URL", http.StatusInternalServerError)
		return
	}

	resp, err := h.client.Do(req)
	if err != nil {
		log.Printf("fetch episode %s: %v", ep.ID, err)
		http.Error(w, "failed to fetch episode from origin", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Non-200: forward as-is, don't cache.
		log.Printf("proxy: write-through %s: origin returned %d, not caching", ep.ID, resp.StatusCode)
		for _, key := range []string{"Content-Type", "Content-Length"} {
			if v := resp.Header.Get(key); v != "" {
				w.Header().Set(key, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return
	}

	_ = h.db.UpdateEpisodeCacheStatus(ep.ID, "in_progress", nil, 0, "")
	contentType := resp.Header.Get("Content-Type")

	for _, key := range []string{
		"Content-Type", "Content-Length", "Accept-Ranges", "Last-Modified", "ETag",
	} {
		if v := resp.Header.Get(key); v != "" {
			w.Header().Set(key, v)
		}
	}

	// TeeReader: reading from tee copies resp.Body to w (client); cacheBody writes
	// each chunk from the same read into the cache file simultaneously.
	// Track write errors to w separately: if the client closes the connection,
	// w.Write will fail before r.Context() is cancelled (there is a race between
	// the write error and the context cancellation propagating), so we check both.
	cw := &clientWriter{ResponseWriter: w}
	if err := h.cacheBody(ep, cachePath, contentType, io.TeeReader(resp.Body, cw)); err != nil {
		if r.Context().Err() != nil || cw.wroteErr {
			// Client disconnected — reset so the next request retries.
			log.Printf("proxy: client disconnected during write-through %s, resetting to none", ep.ID)
			_ = h.db.UpdateEpisodeCacheStatus(ep.ID, "none", nil, 0, "")
		} else {
			// Origin closed the connection early (e.g. CDN rate-limiting concurrent
			// streams). Reset to none and signal the caller to enqueue a background retry.
			log.Printf("proxy: origin closed early during write-through %s, queuing background retry: %v", ep.ID, err)
			_ = h.db.UpdateEpisodeCacheStatus(ep.ID, "none", nil, 0, "")
			return true
		}
	}
	return false
}

// clientWriter wraps http.ResponseWriter and records whether any Write call
// returned an error, indicating a client-side disconnect.
type clientWriter struct {
	http.ResponseWriter
	wroteErr bool
}

func (cw *clientWriter) Write(p []byte) (int, error) {
	n, err := cw.ResponseWriter.Write(p)
	if err != nil {
		cw.wroteErr = true
	}
	return n, err
}

// backgroundFetch downloads the full episode to disk without streaming to any
// client. Used after a Range request so future requests can be served locally.
func (h *handler) backgroundFetch(ep *db.Episode) {
	log.Printf("proxy: background fetch starting %s", ep.ID)
	cachePath := h.episodeCachePath(ep)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ep.OriginalURL, nil)
	if err != nil {
		return
	}

	resp, err := h.client.Do(req)
	if err != nil {
		log.Printf("bg fetch %s: %v", ep.ID, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("bg fetch %s: origin returned %d", ep.ID, resp.StatusCode)
		return
	}

	_ = h.db.UpdateEpisodeCacheStatus(ep.ID, "in_progress", nil, 0, "")
	contentType := resp.Header.Get("Content-Type")

	if err := h.cacheBody(ep, cachePath, contentType, resp.Body); err != nil {
		log.Printf("bg fetch %s: %v", ep.ID, err)
		_ = h.db.UpdateEpisodeCacheStatus(ep.ID, "failed", nil, 0, "")
	}
}

// cacheBody writes all bytes from body into a temp file, atomically renames it
// to cachePath, and records the cached state in the DB on success.
// On error, the temp file is cleaned up but the DB status is NOT updated —
// the caller is responsible for setting the appropriate failure status.
func (h *handler) cacheBody(ep *db.Episode, cachePath, contentType string, body io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(cachePath), ".tmp-")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := f.Name()
	committed := false
	defer func() {
		f.Close() // no-op if already closed by OS after successful rename
		if !committed {
			os.Remove(tmpPath)
		}
	}()

	if _, err := io.Copy(f, body); err != nil {
		return fmt.Errorf("write body: %w", err)
	}
	// On Linux, renaming an open file is safe; the defer closes it afterwards.
	if err := os.Rename(tmpPath, cachePath); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	committed = true

	var sizeBytes int64
	if info, err := os.Stat(cachePath); err == nil {
		sizeBytes = info.Size()
	}
	_ = h.db.UpdateEpisodeCacheStatus(ep.ID, "cached", &cachePath, sizeBytes, contentType)
	log.Printf("cached episode %s (%d bytes)", ep.ID, sizeBytes)
	return nil
}

// episodeCachePath returns the local disk path for an episode's audio file.
// The filename is "{slugified-title}-{url_id}{ext}" for human readability.
// The slug is capped at 80 bytes to keep filenames well under the 255-byte
// filesystem limit when combined with the url_id and extension.
func (h *handler) episodeCachePath(ep *db.Episode) string {
	slug := feed.Slugify(ep.Title)
	if slug == "" {
		slug = "episode"
	}
	const maxSlugLen = 80
	if len(slug) > maxSlugLen {
		slug = slug[:maxSlugLen]
	}
	ext := feed.EpisodeFileExt(ep.OriginalURL)
	name := slug + "-" + ep.URLID + ext
	return filepath.Join(h.cfg.Storage.CacheDir, "episodes", ep.FeedID, name)
}

// tryFetchLock attempts to acquire the fetch lock for the given episode ID.
// Returns true if the lock was acquired (this goroutine is now the cacher).
func (h *handler) tryFetchLock(id string) bool {
	_, loaded := h.fetchLocks.LoadOrStore(id, struct{}{})
	return !loaded
}

func (h *handler) releaseFetchLock(id string) {
	h.fetchLocks.Delete(id)
}
