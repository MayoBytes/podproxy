package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"podproxy/internal/backup"
	"podproxy/internal/config"
	"podproxy/internal/db"
	"podproxy/internal/feed"
)

type handler struct {
	db         *db.DB
	fetcher    *feed.Fetcher
	prefetcher *feed.Prefetcher
	cfg        *config.Config
	backup     *backup.Manager
}

func RegisterRoutes(mux *http.ServeMux, database *db.DB, fetcher *feed.Fetcher, prefetcher *feed.Prefetcher, cfg *config.Config, bm *backup.Manager) {
	h := &handler{db: database, fetcher: fetcher, prefetcher: prefetcher, cfg: cfg, backup: bm}

	mux.HandleFunc("POST /api/feeds", h.addFeed)
	mux.HandleFunc("GET /api/feeds", h.listFeeds)
	mux.HandleFunc("DELETE /api/feeds/{id}", h.deleteFeed)
	mux.HandleFunc("POST /api/feeds/{id}/refresh", h.refreshFeed)
	mux.HandleFunc("POST /api/feeds/{id}/prefetch", h.prefetchFeed)
	mux.HandleFunc("POST /api/feeds/{id}/bulk-cache", h.bulkCacheFeed)

	mux.HandleFunc("POST /api/backups", h.createBackup)
	mux.HandleFunc("GET /api/backups", h.listBackups)
}

// addFeed handles POST /api/feeds
func (h *handler) addFeed(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.URL == "" {
		http.Error(w, "body must be {\"url\": \"...\"}", http.StatusBadRequest)
		return
	}

	// Fetch the feed once to get its title (used to derive the slug) and
	// episode list. Use a temporary feed ID; we fix up all IDs below without
	// a second HTTP round-trip.
	tmpResult, err := h.fetcher.Fetch("_tmp", body.URL)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to fetch feed: %v", err), http.StatusBadGateway)
		return
	}

	feedID := feed.Slugify(tmpResult.Feed.Title)
	if feedID == "" {
		http.Error(w, "could not derive a slug from feed title", http.StatusBadRequest)
		return
	}

	// Check if this feed ID already exists.
	existing, err := h.db.GetFeed(feedID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if existing != nil {
		proxyURL := fmt.Sprintf("%s/feeds/%s.rss", h.cfg.Server.BaseURL, feedID)
		writeJSON(w, http.StatusOK, map[string]string{
			"id":        feedID,
			"proxy_url": proxyURL,
			"message":   "feed already exists",
		})
		return
	}

	// Re-map the temporary feed ID to the real slug in-place.
	// Episode IDs are "{feedID}/{guid}" and URLIDs are hash(guid) — only the
	// prefix needs updating; the URLID is independent of the feed ID.
	result := tmpResult
	result.Feed.ID = feedID
	for _, ep := range result.Episodes {
		ep.ID = feedID + "/" + strings.TrimPrefix(ep.ID, "_tmp/")
		ep.FeedID = feedID
	}

	if err := h.db.InsertFeed(result.Feed); err != nil {
		log.Printf("insert feed: %v", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	for _, ep := range result.Episodes {
		if err := h.db.UpsertEpisode(ep); err != nil {
			log.Printf("upsert episode %s: %v", ep.ID, err)
		}
	}

	if err := h.db.UpdateFeedFetchedAt(feedID, time.Now()); err != nil {
		log.Printf("update fetched_at: %v", err)
	}

	// Cache artwork best-effort so it's ready before the first serveFeed request.
	if result.ArtworkURL != "" {
		artworksDir := filepath.Join(h.cfg.Storage.CacheDir, "feeds")
		if _, err := h.fetcher.CacheArtwork(result.ArtworkURL, artworksDir, feedID); err != nil {
			log.Printf("api: feed %s: cache artwork: %v", feedID, err)
		}
	}

	log.Printf("api: added feed %s (%q, %d episodes)", feedID, result.Feed.Title, len(result.Episodes))
	proxyURL := fmt.Sprintf("%s/feeds/%s.rss", h.cfg.Server.BaseURL, feedID)
	writeJSON(w, http.StatusCreated, map[string]string{
		"id":        feedID,
		"proxy_url": proxyURL,
	})
}

type feedResponse struct {
	ID                     string `json:"id"`
	Title                  string `json:"title"`
	OriginalURL            string `json:"original_url"`
	ProxyURL               string `json:"proxy_url"`
	RefreshIntervalMinutes int    `json:"refresh_interval_minutes"`
	AutoPrefetch           bool   `json:"auto_prefetch"`
}

// listFeeds handles GET /api/feeds
func (h *handler) listFeeds(w http.ResponseWriter, r *http.Request) {
	feeds, err := h.db.ListFeeds()
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	resp := make([]feedResponse, 0, len(feeds))
	for _, f := range feeds {
		resp = append(resp, feedResponse{
			ID:                     f.ID,
			Title:                  f.Title,
			OriginalURL:            f.OriginalURL,
			ProxyURL:               fmt.Sprintf("%s/feeds/%s.rss", h.cfg.Server.BaseURL, f.ID),
			RefreshIntervalMinutes: f.RefreshIntervalMinutes,
			AutoPrefetch:           f.AutoPrefetch,
		})
	}

	writeJSON(w, http.StatusOK, resp)
}

// deleteFeed handles DELETE /api/feeds/{id}
func (h *handler) deleteFeed(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if inProgress, err := h.db.HasInProgressEpisodes(id); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	} else if inProgress {
		http.Error(w, "feed has episodes currently being cached; retry shortly", http.StatusConflict)
		return
	}
	if err := h.db.DeleteFeed(id); errors.Is(err, db.ErrNotFound) {
		http.Error(w, "feed not found", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	log.Printf("api: deleted feed %s", id)
	episodeDir := filepath.Join(h.cfg.Storage.CacheDir, "episodes", id)
	if err := os.RemoveAll(episodeDir); err != nil {
		log.Printf("api: remove episode cache dir %s: %v", episodeDir, err)
	}
	feedsDir := filepath.Join(h.cfg.Storage.CacheDir, "feeds")
	feedXML := filepath.Join(feedsDir, id+".rss")
	if err := os.Remove(feedXML); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("api: remove feed xml cache %s: %v", feedXML, err)
	}
	if artworkPath, ok := feed.ArtworkPath(feedsDir, id); ok {
		if err := os.Remove(artworkPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("api: remove feed artwork %s: %v", artworkPath, err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// refreshFeed handles POST /api/feeds/{id}/refresh
func (h *handler) refreshFeed(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	f, err := h.db.GetFeed(id)
	if errors.Is(err, db.ErrNotFound) {
		http.Error(w, "feed not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	result, err := h.fetcher.Fetch(f.ID, f.OriginalURL)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to fetch feed: %v", err), http.StatusBadGateway)
		return
	}

	for _, ep := range result.Episodes {
		if err := h.db.UpsertEpisode(ep); err != nil {
			log.Printf("upsert episode %s: %v", ep.ID, err)
		}
	}

	if err := h.db.UpdateFeedFetchedAt(id, time.Now()); err != nil {
		log.Printf("update fetched_at: %v", err)
	}

	// Cache artwork before writing the XML so the cached feed never references
	// /artwork/{id} unless the file is actually present. Only rewrite the URL
	// when caching succeeded.
	artworksDir := filepath.Join(h.cfg.Storage.CacheDir, "feeds")
	effectiveArtworkURL := ""
	if result.ArtworkURL != "" {
		if _, err := h.fetcher.CacheArtwork(result.ArtworkURL, artworksDir, id); err != nil {
			log.Printf("api: feed %s: cache artwork: %v", id, err)
		} else {
			effectiveArtworkURL = result.ArtworkURL
		}
	}

	// Regenerate the cached .rss file so the next GET /feeds/:id.rss
	// serves URLs with the current rewrite logic (e.g. with file extensions).
	episodes, err := h.db.ListEpisodesByFeed(id)
	if err != nil {
		log.Printf("list episodes for %s: %v", id, err)
	} else {
		urlMap := make(map[string]string, len(episodes))
		for _, ep := range episodes {
			urlMap[ep.OriginalURL] = ep.URLID
		}
		rewritten := feed.RewriteXML(result.RawXML, id, urlMap, h.cfg.Server.BaseURL, effectiveArtworkURL)
		cachePath := filepath.Join(h.cfg.Storage.CacheDir, "feeds", id+".rss")
		if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err == nil {
			os.WriteFile(cachePath, rewritten, 0644)
		}
	}

	log.Printf("api: refreshed feed %s (%d episodes)", id, len(result.Episodes))
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            id,
		"episodes_seen": len(result.Episodes),
	})
}

// bulkCacheFeed handles POST /api/feeds/{id}/bulk-cache.
// Body: {"episode_ids": ["urlid1", "urlid2", ...]}
func (h *handler) bulkCacheFeed(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if _, err := h.db.GetFeed(id); errors.Is(err, db.ErrNotFound) {
		http.Error(w, "feed not found", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	var body struct {
		EpisodeIDs []string `json:"episode_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.EpisodeIDs) == 0 {
		http.Error(w, `body must be {"episode_ids": ["..."]}`, http.StatusBadRequest)
		return
	}
	if len(body.EpisodeIDs) > 500 {
		http.Error(w, "episode_ids must contain 500 or fewer entries", http.StatusBadRequest)
		return
	}

	if h.prefetcher == nil {
		http.Error(w, "prefetcher not available", http.StatusServiceUnavailable)
		return
	}

	queued, skipped, dropped := 0, 0, 0
	for _, urlID := range body.EpisodeIDs {
		ep, err := h.db.GetEpisodeByURLID(id, urlID)
		if errors.Is(err, db.ErrNotFound) {
			continue
		} else if err != nil {
			log.Printf("api: bulk-cache get episode %s/%s: %v", id, urlID, err)
			continue
		}
		if ep.CacheStatus == "cached" || ep.CacheStatus == "in_progress" {
			skipped++
			continue
		}
		if h.prefetcher.Enqueue(ep) {
			queued++
		} else {
			dropped++
		}
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"id":      id,
		"queued":  queued,
		"skipped": skipped,
		"dropped": dropped,
	})
}

// prefetchFeed handles POST /api/feeds/{id}/prefetch
// Enqueues all un-cached episodes within the prefetch_max_age_days window for
// background download.
func (h *handler) prefetchFeed(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if _, err := h.db.GetFeed(id); errors.Is(err, db.ErrNotFound) {
		http.Error(w, "feed not found", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	if h.prefetcher == nil {
		http.Error(w, "prefetcher not available", http.StatusServiceUnavailable)
		return
	}
	h.prefetcher.EnqueueFeedEpisodes(id)
	writeJSON(w, http.StatusAccepted, map[string]string{"id": id, "status": "queued"})
}

// createBackup handles POST /api/backups
func (h *handler) createBackup(w http.ResponseWriter, r *http.Request) {
	info, err := h.backup.CreateBackup()
	if err != nil {
		log.Printf("api: create backup: %v", err)
		http.Error(w, "backup failed", http.StatusInternalServerError)
		return
	}
	log.Printf("api: created backup %s (%d bytes)", info.Name, info.SizeBytes)
	writeJSON(w, http.StatusCreated, info)
}

// listBackups handles GET /api/backups
func (h *handler) listBackups(w http.ResponseWriter, r *http.Request) {
	backups, err := h.backup.ListBackups()
	if err != nil {
		log.Printf("api: list backups: %v", err)
		http.Error(w, "could not list backups", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, backups)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
