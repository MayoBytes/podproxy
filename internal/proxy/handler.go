package proxy

import (
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"podproxy/internal/config"
	"podproxy/internal/db"
	"podproxy/internal/feed"
)

type handler struct {
	db      *db.DB
	fetcher *feed.Fetcher
	cfg     *config.Config
	client  *http.Client
}

func RegisterRoutes(mux *http.ServeMux, database *db.DB, fetcher *feed.Fetcher, cfg *config.Config) {
	h := &handler{
		db:      database,
		fetcher: fetcher,
		cfg:     cfg,
		// Shared client with a header timeout so a slow CDN can't hang a goroutine
		// forever. WriteTimeout on the server is 0 (streaming), so we guard the
		// outbound side here instead.
		client: &http.Client{
			Transport: &http.Transport{
				ResponseHeaderTimeout: 30 * time.Second,
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

	// Serve from disk cache if the feed is still fresh.
	if f.LastFetchedAt != nil && time.Since(*f.LastFetchedAt) < staleDur {
		if data, err := os.ReadFile(cachePath); err == nil {
			w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.Write(data)
			return
		}
		// Cache file missing despite DB saying fresh — fall through to re-fetch.
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
	if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err == nil {
		os.WriteFile(cachePath, rewritten, 0644)
	}

	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(rewritten)))
	w.Write(rewritten)
}

// serveEpisode handles GET /episodes/{feed_id}/{ep_id}
// Phase 1: pure transparent proxy, no caching.
func (h *handler) serveEpisode(w http.ResponseWriter, r *http.Request) {
	feedID := r.PathValue("feed_id")
	epID := r.PathValue("ep_id")

	ep, err := h.db.GetEpisodeByURLID(feedID, epID)
	if errors.Is(err, db.ErrNotFound) {
		http.Error(w, "episode not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	// Build a proxied request to the origin, forwarding relevant headers.
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
		log.Printf("proxy episode %s/%s: %v", feedID, epID, err)
		http.Error(w, "failed to fetch episode from origin", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers.
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
			log.Printf("stream episode %s/%s: %v", feedID, epID, err)
		}
	}
}
