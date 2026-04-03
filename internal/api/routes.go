package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
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
}

func RegisterRoutes(mux *http.ServeMux, database *db.DB, fetcher *feed.Fetcher, prefetcher *feed.Prefetcher, cfg *config.Config) {
	h := &handler{db: database, fetcher: fetcher, prefetcher: prefetcher, cfg: cfg}

	mux.HandleFunc("POST /api/feeds", h.addFeed)
	mux.HandleFunc("GET /api/feeds", h.listFeeds)
	mux.HandleFunc("DELETE /api/feeds/{id}", h.deleteFeed)
	mux.HandleFunc("POST /api/feeds/{id}/refresh", h.refreshFeed)
	mux.HandleFunc("POST /api/feeds/{id}/prefetch", h.prefetchFeed)
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
	err := h.db.DeleteFeed(id)
	if errors.Is(err, db.ErrNotFound) {
		http.Error(w, "feed not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
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

	writeJSON(w, http.StatusOK, map[string]any{
		"id":            id,
		"episodes_seen": len(result.Episodes),
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
