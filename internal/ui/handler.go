package ui

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"podproxy/internal/backup"
	"podproxy/internal/config"
	"podproxy/internal/db"
	"podproxy/internal/feed"
)

//go:embed templates
var tmplFS embed.FS

// feedsTmpl and episodesTmpl are parsed into separate sets so their "body"
// block overrides don't conflict with each other.
var (
	feedsTmpl    *template.Template
	episodesTmpl *template.Template
)

func init() {
	funcs := template.FuncMap{
		"humanSize":     humanSize,
		"humanTime":     humanTime,
		"humanDuration": humanDuration,
		"cacheClass":    cacheStatusClass,
		"unixTime":      unixTime,
	}
	feedsTmpl = template.Must(
		template.New("").Funcs(funcs).ParseFS(tmplFS,
			"templates/base.html", "templates/feeds.html", "templates/migrate.html"),
	)
	episodesTmpl = template.Must(
		template.New("").Funcs(funcs).ParseFS(tmplFS,
			"templates/base.html", "templates/episodes.html"),
	)
}

// RegisterRoutes mounts the HTMX UI under /ui.
func RegisterRoutes(mux *http.ServeMux, database *db.DB, fetcher *feed.Fetcher, prefetcher *feed.Prefetcher, cfg *config.Config, bm *backup.Manager) {
	h := &uiHandler{db: database, fetcher: fetcher, prefetcher: prefetcher, cfg: cfg, backup: bm}
	mux.HandleFunc("GET /ui", h.feedsPage)
	mux.HandleFunc("GET /ui/feeds/{id}/episodes", h.episodesPage)
	mux.HandleFunc("GET /ui/feeds/{id}/episode-list", h.episodeListFragment)
	mux.HandleFunc("POST /ui/feeds/add", h.addFeed)
	mux.HandleFunc("DELETE /ui/feeds/{id}", h.deleteFeed)
	mux.HandleFunc("POST /ui/feeds/{id}/refresh", h.refreshFeed)
	mux.HandleFunc("POST /ui/feeds/{id}/refresh-artwork", h.refreshArtwork)
	mux.HandleFunc("POST /ui/feeds/{id}/toggle-autoprefetch", h.toggleAutoPrefetch)
	mux.HandleFunc("GET /ui/feeds/{id}/migrate", h.migrateForm)
	mux.HandleFunc("POST /ui/feeds/{id}/migrate/preview", h.migratePreview)
	mux.HandleFunc("POST /ui/feeds/{id}/migrate", h.migrateCommit)
	mux.HandleFunc("POST /ui/feeds/{id}/bulk-cache", h.bulkCacheEpisodes)
	mux.HandleFunc("POST /ui/feeds/{id}/bulk-delete", h.bulkDeleteEpisodes)
	mux.HandleFunc("POST /ui/feeds/{id}/episodes/{epid}/cache", h.cacheEpisode)
	mux.HandleFunc("DELETE /ui/feeds/{id}/episodes/{epid}", h.deleteEpisodeCache)
	mux.HandleFunc("POST /ui/backups", h.createBackupUI)
	mux.HandleFunc("GET /ui/backups/{name}", h.downloadBackup)
}

type uiHandler struct {
	db         *db.DB
	fetcher    *feed.Fetcher
	prefetcher *feed.Prefetcher
	cfg        *config.Config
	backup     *backup.Manager
}

// ---------------------------------------------------------------------------
// Full-page handlers
// ---------------------------------------------------------------------------

func (h *uiHandler) feedsPage(w http.ResponseWriter, r *http.Request) {
	data, err := h.buildFeedsData("")
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	if err := feedsTmpl.ExecuteTemplate(w, "base", data); err != nil {
		log.Printf("ui: render feeds page: %v", err)
	}
}

func (h *uiHandler) episodesPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f, err := h.db.GetFeed(id)
	if errors.Is(err, db.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	eps, err := h.db.ListEpisodesByFeed(id)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	data := episodesPageData{
		Feed:          f,
		Episodes:      eps,
		ProxyURL:      fmt.Sprintf("%s/feeds/%s.rss", h.cfg.Server.BaseURL, f.ID),
		HasInProgress: hasInProgress(eps),
	}
	w.Header().Set("Content-Type", "text/html")
	if err := episodesTmpl.ExecuteTemplate(w, "base", data); err != nil {
		log.Printf("ui: render episodes page: %v", err)
	}
}

// ---------------------------------------------------------------------------
// HTMX action handlers — all return the #feed-list fragment for in-place swap
// ---------------------------------------------------------------------------

func (h *uiHandler) addFeed(w http.ResponseWriter, r *http.Request) {
	rawURL := strings.TrimSpace(r.FormValue("url"))
	if rawURL == "" {
		h.renderFeedList(w, "URL is required.", true)
		return
	}

	tmpResult, err := h.fetcher.Fetch("_tmp", rawURL)
	if err != nil {
		h.renderFeedList(w, fmt.Sprintf("Failed to fetch feed: %v", err), true)
		return
	}

	feedID := feed.Slugify(tmpResult.Feed.Title)
	if feedID == "" {
		h.renderFeedList(w, "Could not derive a slug from the feed title.", true)
		return
	}

	existing, err := h.db.GetFeed(feedID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		h.renderFeedList(w, "Database error.", true)
		return
	}
	if existing != nil {
		h.renderFeedList(w, fmt.Sprintf("Feed %q already exists.", feedID), false)
		return
	}

	feedCopy := *tmpResult.Feed
	feedCopy.ID = feedID
	result := &feed.FetchResult{
		Feed:       &feedCopy,
		Episodes:   tmpResult.Episodes,
		RawXML:     tmpResult.RawXML,
		ArtworkURL: tmpResult.ArtworkURL,
	}
	for _, ep := range result.Episodes {
		ep.ID = feedID + "/" + strings.TrimPrefix(ep.ID, "_tmp/")
		ep.FeedID = feedID
	}

	if err := h.db.InsertFeed(result.Feed); err != nil {
		log.Printf("ui: insert feed: %v", err)
		h.renderFeedList(w, "Failed to save feed.", true)
		return
	}
	for _, ep := range result.Episodes {
		if err := h.db.UpsertEpisode(ep); err != nil {
			log.Printf("ui: upsert episode %s: %v", ep.ID, err)
		}
	}
	_ = h.db.UpdateFeedFetchedAt(feedID, time.Now())

	if result.ArtworkURL != "" {
		feedsDir := filepath.Join(h.cfg.Storage.CacheDir, "feeds")
		if _, err := h.fetcher.CacheArtwork(result.ArtworkURL, feedsDir, feedID); err != nil {
			log.Printf("ui: add feed %s: cache artwork: %v", feedID, err)
		}
	}

	h.renderFeedList(w, fmt.Sprintf("Added \"%s\" (%d episode(s)).", result.Feed.Title, len(result.Episodes)), false)
}

func (h *uiHandler) deleteFeed(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if inProgress, err := h.db.HasInProgressEpisodes(id); err != nil {
		h.renderFeedList(w, "Database error.", true)
		return
	} else if inProgress {
		h.renderFeedList(w, "Cannot delete: one or more episodes are currently being downloaded.", true)
		return
	}
	if err := h.db.DeleteFeed(id); errors.Is(err, db.ErrNotFound) {
		h.renderFeedList(w, "Feed not found.", true)
		return
	} else if err != nil {
		h.renderFeedList(w, "Failed to delete feed.", true)
		return
	}
	episodeDir := filepath.Join(h.cfg.Storage.CacheDir, "episodes", id)
	if err := os.RemoveAll(episodeDir); err != nil {
		log.Printf("ui: remove episode cache dir %s: %v", episodeDir, err)
	}
	feedsDir := filepath.Join(h.cfg.Storage.CacheDir, "feeds")
	feedXML := filepath.Join(feedsDir, id+".rss")
	if err := os.Remove(feedXML); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("ui: remove feed xml cache %s: %v", feedXML, err)
	}
	if artworkPath, ok := feed.ArtworkPath(feedsDir, id); ok {
		if err := os.Remove(artworkPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("ui: remove feed artwork %s: %v", artworkPath, err)
		}
	}
	h.renderFeedList(w, fmt.Sprintf("Deleted feed %q.", id), false)
}

func (h *uiHandler) refreshFeed(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f, err := h.db.GetFeed(id)
	if errors.Is(err, db.ErrNotFound) {
		h.renderFeedList(w, "Feed not found.", true)
		return
	}
	if err != nil {
		h.renderFeedList(w, "Database error.", true)
		return
	}

	result, err := h.fetcher.Fetch(f.ID, f.OriginalURL)
	if err != nil {
		h.renderFeedList(w, fmt.Sprintf("Refresh failed: %v", err), true)
		return
	}
	for _, ep := range result.Episodes {
		if err := h.db.UpsertEpisode(ep); err != nil {
			log.Printf("ui: upsert episode %s: %v", ep.ID, err)
		}
	}
	_ = h.db.UpdateFeedFetchedAt(id, time.Now())

	h.renderFeedList(w,
		fmt.Sprintf("Refreshed \"%s\" — %d episode(s) seen.", f.Title, len(result.Episodes)),
		false)
}

func (h *uiHandler) refreshArtwork(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f, err := h.db.GetFeed(id)
	if errors.Is(err, db.ErrNotFound) {
		h.renderFeedList(w, "Feed not found.", true)
		return
	}
	if err != nil {
		h.renderFeedList(w, "Database error.", true)
		return
	}

	result, err := h.fetcher.Fetch(f.ID, f.OriginalURL)
	if err != nil {
		h.renderFeedList(w, fmt.Sprintf("Failed to fetch feed: %v", err), true)
		return
	}
	if result.ArtworkURL == "" {
		h.renderFeedList(w, fmt.Sprintf("Feed %q has no artwork URL.", f.Title), false)
		return
	}

	feedsDir := filepath.Join(h.cfg.Storage.CacheDir, "feeds")
	if existing, ok := feed.ArtworkPath(feedsDir, id); ok {
		if err := os.Remove(existing); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("ui: refresh artwork: remove %s: %v", existing, err)
		}
	}
	if _, err := h.fetcher.CacheArtwork(result.ArtworkURL, feedsDir, id); err != nil {
		h.renderFeedList(w, fmt.Sprintf("Failed to refresh artwork for %q: %v", f.Title, err), true)
		return
	}
	h.renderFeedList(w, fmt.Sprintf("Refreshed artwork for %q.", f.Title), false)
}

// migrateForm renders the empty migration form for a feed.
// Handles GET /ui/feeds/{id}/migrate. The optional new_url query param lets
// the preview's Back button restore whatever the user had typed in.
func (h *uiHandler) migrateForm(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f, err := h.db.GetFeed(id)
	if errors.Is(err, db.ErrNotFound) {
		h.renderFeedList(w, "Feed not found.", true)
		return
	}
	if err != nil {
		h.renderFeedList(w, "Database error.", true)
		return
	}
	prefill := strings.TrimSpace(r.URL.Query().Get("new_url"))
	h.renderMigrateForm(w, f, prefill, "", false)
}

// migratePreview validates a candidate URL and renders the preview fragment.
// Handles POST /ui/feeds/{id}/migrate/preview.
func (h *uiHandler) migratePreview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rawURL := r.FormValue("new_url")

	f, err := h.db.GetFeed(id)
	if errors.Is(err, db.ErrNotFound) {
		h.renderFeedList(w, "Feed not found.", true)
		return
	}
	if err != nil {
		h.renderFeedList(w, "Database error.", true)
		return
	}

	cleaned, err := validateMigrationURLForUI(rawURL)
	if err != nil {
		h.renderMigrateForm(w, f, strings.TrimSpace(rawURL), err.Error(), true)
		return
	}

	preview, _, err := h.fetcher.PreviewMigration(h.db, id, cleaned)
	if errors.Is(err, feed.ErrMigrationNoChange) {
		h.renderMigrateForm(w, f, cleaned, "The new URL is the same as the current URL.", true)
		return
	}
	if err != nil {
		h.renderMigrateForm(w, f, cleaned, fmt.Sprintf("Could not fetch new feed: %v", err), true)
		return
	}
	h.renderMigratePreview(w, f, preview)
}

// migrateCommit applies a previewed migration.
// Handles POST /ui/feeds/{id}/migrate.
func (h *uiHandler) migrateCommit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rawURL := r.FormValue("new_url")
	force := r.FormValue("force") == "true"

	f, err := h.db.GetFeed(id)
	if errors.Is(err, db.ErrNotFound) {
		h.renderFeedList(w, "Feed not found.", true)
		return
	}
	if err != nil {
		h.renderFeedList(w, "Database error.", true)
		return
	}

	cleaned, err := validateMigrationURLForUI(rawURL)
	if err != nil {
		h.renderMigrateForm(w, f, strings.TrimSpace(rawURL), err.Error(), true)
		return
	}

	if inProgress, err := h.db.HasInProgressEpisodes(id); err != nil {
		h.renderFeedList(w, "Database error.", true)
		return
	} else if inProgress {
		h.renderMigrateForm(w, f, cleaned,
			"Cannot migrate: one or more episodes are currently being downloaded. Retry shortly.", true)
		return
	}

	preview, result, err := h.fetcher.PreviewMigration(h.db, id, cleaned)
	if errors.Is(err, feed.ErrMigrationNoChange) {
		h.renderMigrateForm(w, f, cleaned, "The new URL is the same as the current URL.", true)
		return
	}
	if err != nil {
		h.renderMigrateForm(w, f, cleaned, fmt.Sprintf("Could not fetch new feed: %v", err), true)
		return
	}
	if len(preview.Warnings) > 0 && !force {
		h.renderMigratePreview(w, f, preview)
		return
	}

	newTitle := result.Feed.Title
	if newTitle == "" {
		newTitle = f.Title
	}
	if err := h.db.UpdateFeedURLAndTitle(id, cleaned, newTitle); err != nil {
		log.Printf("ui: update feed url/title: %v", err)
		h.renderFeedList(w, "Failed to update feed.", true)
		return
	}

	// Discard the old artwork on every migration — the feed identity has
	// changed, so the cached image no longer represents this feed.
	// applyFeedFetchResult re-caches if the new feed advertises artwork.
	feedsDir := filepath.Join(h.cfg.Storage.CacheDir, "feeds")
	if existing, ok := feed.ArtworkPath(feedsDir, id); ok {
		if err := os.Remove(existing); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("ui: migrate remove old artwork %s: %v", existing, err)
		}
	}

	h.applyFeedFetchResult(id, result)
	log.Printf("ui: migrated feed %s to %s (%d episodes, title=%q)",
		id, cleaned, len(result.Episodes), newTitle)
	h.renderFeedList(w,
		fmt.Sprintf("Migrated %q to new URL (%d episode(s) seen).", newTitle, len(result.Episodes)),
		false)
}

// applyFeedFetchResult persists a fetched feed result to the DB and on-disk
// caches: upserts episodes, bumps last_fetched_at, caches artwork
// (best-effort), and regenerates the cached .rss file. Shared between code
// paths that mutate feed state (currently the migration commit handler).
func (h *uiHandler) applyFeedFetchResult(id string, result *feed.FetchResult) {
	for _, ep := range result.Episodes {
		if err := h.db.UpsertEpisode(ep); err != nil {
			log.Printf("ui: upsert episode %s: %v", ep.ID, err)
		}
	}
	_ = h.db.UpdateFeedFetchedAt(id, time.Now())

	feedsDir := filepath.Join(h.cfg.Storage.CacheDir, "feeds")
	effectiveArtworkURL := ""
	if result.ArtworkURL != "" {
		if _, err := h.fetcher.CacheArtwork(result.ArtworkURL, feedsDir, id); err != nil {
			log.Printf("ui: feed %s: cache artwork: %v", id, err)
		} else {
			effectiveArtworkURL = result.ArtworkURL
		}
	}

	episodes, err := h.db.ListEpisodesByFeed(id)
	if err != nil {
		log.Printf("ui: list episodes for %s: %v", id, err)
		return
	}
	urlMap := make(map[string]string, len(episodes))
	for _, ep := range episodes {
		urlMap[ep.OriginalURL] = ep.URLID
	}
	rewritten := feed.RewriteXML(result.RawXML, id, urlMap, h.cfg.Server.BaseURL, effectiveArtworkURL)
	cachePath := filepath.Join(feedsDir, id+".rss")
	if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err == nil {
		os.WriteFile(cachePath, rewritten, 0644)
	}
}

// validateMigrationURLForUI returns a cleaned URL or a user-facing error
// message suitable for showing in the migrate form.
func validateMigrationURLForUI(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("URL is required.")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("Please enter a valid absolute URL (e.g. https://host.example/feed.rss).")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("URL scheme must be http or https.")
	}
	return raw, nil
}

func (h *uiHandler) toggleAutoPrefetch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f, err := h.db.GetFeed(id)
	if errors.Is(err, db.ErrNotFound) {
		h.renderFeedList(w, "Feed not found.", true)
		return
	} else if err != nil {
		h.renderFeedList(w, "Database error.", true)
		return
	}
	newVal, err := h.db.ToggleFeedAutoPrefetch(id)
	if err != nil {
		h.renderFeedList(w, "Failed to update auto-prefetch setting.", true)
		return
	}
	if newVal {
		h.renderFeedList(w, fmt.Sprintf("Auto-prefetch enabled for %q.", f.Title), false)
	} else {
		h.renderFeedList(w, fmt.Sprintf("Auto-prefetch disabled for %q.", f.Title), false)
	}
}

func (h *uiHandler) cacheEpisode(w http.ResponseWriter, r *http.Request) {
	feedID := r.PathValue("id")
	epURLID := r.PathValue("epid")

	ep, err := h.db.GetEpisodeByURLID(feedID, epURLID)
	if errors.Is(err, db.ErrNotFound) {
		h.renderEpisodeList(w, feedID, "Episode not found.", true)
		return
	}
	if err != nil {
		h.renderEpisodeList(w, feedID, "Database error.", true)
		return
	}

	if ep.CacheStatus == "cached" {
		h.renderEpisodeList(w, feedID, fmt.Sprintf("%q is already cached.", ep.Title), false)
		return
	}
	if ep.CacheStatus == "in_progress" {
		h.renderEpisodeList(w, feedID, "Episode is currently being downloaded.", true)
		return
	}

	if h.prefetcher == nil {
		h.renderEpisodeList(w, feedID, "Prefetcher not available.", true)
		return
	}
	if !h.prefetcher.Enqueue(ep) {
		h.renderEpisodeList(w, feedID, "Cache queue is full, try again shortly.", true)
		return
	}
	h.renderEpisodeList(w, feedID, fmt.Sprintf("Queued %q for caching.", ep.Title), false)
}

func (h *uiHandler) deleteEpisodeCache(w http.ResponseWriter, r *http.Request) {
	feedID := r.PathValue("id")
	epURLID := r.PathValue("epid")

	ep, err := h.db.GetEpisodeByURLID(feedID, epURLID)
	if errors.Is(err, db.ErrNotFound) {
		h.renderEpisodeList(w, feedID, "Episode not found.", true)
		return
	}
	if err != nil {
		h.renderEpisodeList(w, feedID, "Database error.", true)
		return
	}

	if ep.CacheStatus == "in_progress" {
		h.renderEpisodeList(w, feedID, "Cannot delete: download in progress.", true)
		return
	}

	if ep.CachedPath != "" {
		if err := os.Remove(ep.CachedPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("ui: remove cached episode %s: %v", ep.ID, err)
			h.renderEpisodeList(w, feedID, "Failed to delete cached file.", true)
			return
		}
	}
	if err := h.db.UpdateEpisodeCacheStatus(ep.ID, "none", nil, 0, ""); err != nil {
		h.renderEpisodeList(w, feedID, "Failed to update episode status.", true)
		return
	}
	h.renderEpisodeList(w, feedID, fmt.Sprintf("Deleted cached file for %q.", ep.Title), false)
}

// episodeListFragment handles GET /ui/feeds/{id}/episode-list — returns only
// the #episode-list fragment, used for HTMX polling while downloads are active.
func (h *uiHandler) episodeListFragment(w http.ResponseWriter, r *http.Request) {
	h.renderEpisodeList(w, r.PathValue("id"), "", false)
}

// bulkCacheEpisodes handles POST /ui/feeds/{id}/bulk-cache.
// It accepts one or more "ep" form values (episode URLIDs) and enqueues each
// uncached episode for background download.
func (h *uiHandler) bulkCacheEpisodes(w http.ResponseWriter, r *http.Request) {
	feedID := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		h.renderEpisodeList(w, feedID, "Invalid request.", true)
		return
	}
	urlIDs := r.Form["ep"]
	if len(urlIDs) == 0 {
		h.renderEpisodeList(w, feedID, "No episodes selected.", false)
		return
	}
	if h.prefetcher == nil {
		h.renderEpisodeList(w, feedID, "Prefetcher not available.", true)
		return
	}
	queued, skipped, dropped := 0, 0, 0
	for _, urlID := range urlIDs {
		ep, err := h.db.GetEpisodeByURLID(feedID, urlID)
		if errors.Is(err, db.ErrNotFound) {
			continue
		} else if err != nil {
			log.Printf("ui: bulk-cache get episode %s/%s: %v", feedID, urlID, err)
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
	msg := fmt.Sprintf("Queued %d episode(s) for caching.", queued)
	if skipped > 0 {
		msg += fmt.Sprintf(" %d already cached or in progress.", skipped)
	}
	if dropped > 0 {
		msg += fmt.Sprintf(" %d dropped (queue full — try again shortly).", dropped)
	}
	h.renderEpisodeList(w, feedID, msg, false)
}

// bulkDeleteEpisodes handles POST /ui/feeds/{id}/bulk-delete.
// It accepts one or more "ep" form values (episode URLIDs) and deletes the
// cached file for each cached episode, resetting its status to "none".
func (h *uiHandler) bulkDeleteEpisodes(w http.ResponseWriter, r *http.Request) {
	feedID := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		h.renderEpisodeList(w, feedID, "Invalid request.", true)
		return
	}
	urlIDs := r.Form["ep"]
	if len(urlIDs) == 0 {
		h.renderEpisodeList(w, feedID, "No episodes selected.", false)
		return
	}
	deleted, skipped, failed := 0, 0, 0
	for _, urlID := range urlIDs {
		ep, err := h.db.GetEpisodeByURLID(feedID, urlID)
		if errors.Is(err, db.ErrNotFound) {
			continue
		} else if err != nil {
			log.Printf("ui: bulk-delete get episode %s/%s: %v", feedID, urlID, err)
			continue
		}
		if ep.CacheStatus != "cached" {
			skipped++
			continue
		}
		if ep.CachedPath != "" {
			if err := os.Remove(ep.CachedPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Printf("ui: bulk-delete remove %s: %v", ep.ID, err)
				failed++
				continue
			}
		}
		if err := h.db.UpdateEpisodeCacheStatus(ep.ID, "none", nil, 0, ""); err != nil {
			log.Printf("ui: bulk-delete update status %s: %v", ep.ID, err)
			failed++
			continue
		}
		deleted++
	}
	msg := fmt.Sprintf("Deleted %d cached file(s).", deleted)
	if skipped > 0 {
		msg += fmt.Sprintf(" %d skipped (not cached or in progress).", skipped)
	}
	if failed > 0 {
		msg += fmt.Sprintf(" %d could not be deleted.", failed)
	}
	h.renderEpisodeList(w, feedID, msg, failed > 0)
}

// renderEpisodeList writes only the #episode-list fragment (HTMX target).
func (h *uiHandler) renderEpisodeList(w http.ResponseWriter, feedID, message string, isError bool) {
	f, err := h.db.GetFeed(feedID)
	if errors.Is(err, db.ErrNotFound) {
		http.Error(w, "feed not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	eps, err := h.db.ListEpisodesByFeed(feedID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	data := &episodesPageData{
		Feed:          f,
		Episodes:      eps,
		ProxyURL:      fmt.Sprintf("%s/feeds/%s.rss", h.cfg.Server.BaseURL, f.ID),
		Message:       message,
		IsError:       isError,
		HasInProgress: hasInProgress(eps),
	}
	w.Header().Set("Content-Type", "text/html")
	if err := episodesTmpl.ExecuteTemplate(w, "episode-list", data); err != nil {
		log.Printf("ui: render episode-list fragment: %v", err)
	}
}

func hasInProgress(eps []*db.Episode) bool {
	for _, ep := range eps {
		if ep.CacheStatus == "in_progress" {
			return true
		}
	}
	return false
}

// renderMigrateForm writes the migrate-form fragment.
func (h *uiHandler) renderMigrateForm(w http.ResponseWriter, f *db.Feed, newURL, message string, isError bool) {
	data := migrateFormData{Feed: f, NewURL: newURL, Message: message, IsError: isError}
	w.Header().Set("Content-Type", "text/html")
	if err := feedsTmpl.ExecuteTemplate(w, "migrate-form", data); err != nil {
		log.Printf("ui: render migrate-form fragment: %v", err)
	}
}

// renderMigratePreview writes the migrate-preview fragment with the diff.
func (h *uiHandler) renderMigratePreview(w http.ResponseWriter, f *db.Feed, preview *feed.MigrationPreview) {
	data := migratePreviewData{Feed: f, Preview: preview}
	w.Header().Set("Content-Type", "text/html")
	if err := feedsTmpl.ExecuteTemplate(w, "migrate-preview", data); err != nil {
		log.Printf("ui: render migrate-preview fragment: %v", err)
	}
}

// renderFeedList writes only the #feed-list fragment (HTMX target).
func (h *uiHandler) renderFeedList(w http.ResponseWriter, message string, isError bool) {
	data, err := h.buildFeedsData(message)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	data.IsError = isError
	w.Header().Set("Content-Type", "text/html")
	if err := feedsTmpl.ExecuteTemplate(w, "feed-list", data); err != nil {
		log.Printf("ui: render feed-list fragment: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Data helpers
// ---------------------------------------------------------------------------

type backupSectionData struct {
	Backups   []backup.BackupInfo
	Message   string
	IsError   bool
}

type feedsPageData struct {
	Feeds   []*db.FeedWithStats
	Message string
	IsError bool
	BaseURL string
	Backup  backupSectionData
}

type migrateFormData struct {
	Feed    *db.Feed
	NewURL  string
	Message string
	IsError bool
}

type migratePreviewData struct {
	Feed    *db.Feed
	Preview *feed.MigrationPreview
}

type episodesPageData struct {
	Feed          *db.Feed
	Episodes      []*db.Episode
	ProxyURL      string
	Message       string
	IsError       bool
	HasInProgress bool
}

func (h *uiHandler) buildFeedsData(message string) (*feedsPageData, error) {
	feeds, err := h.db.ListFeedsWithStats()
	if err != nil {
		return nil, err
	}
	backups, _ := h.backup.ListBackups()
	return &feedsPageData{
		Feeds:   feeds,
		Message: message,
		BaseURL: h.cfg.Server.BaseURL,
		Backup:  backupSectionData{Backups: backups},
	}, nil
}

// renderBackupSection writes only the #backup-section fragment (HTMX target).
func (h *uiHandler) renderBackupSection(w http.ResponseWriter, message string, isError bool) {
	backups, err := h.backup.ListBackups()
	if err != nil {
		log.Printf("ui: list backups: %v", err)
	}
	data := backupSectionData{Backups: backups, Message: message, IsError: isError}
	w.Header().Set("Content-Type", "text/html")
	if err := feedsTmpl.ExecuteTemplate(w, "backup-section", data); err != nil {
		log.Printf("ui: render backup-section fragment: %v", err)
	}
}

// createBackupUI handles POST /ui/backups.
func (h *uiHandler) createBackupUI(w http.ResponseWriter, r *http.Request) {
	info, err := h.backup.CreateBackup()
	if err != nil {
		log.Printf("ui: create backup: %v", err)
		h.renderBackupSection(w, fmt.Sprintf("Backup failed: %v", err), true)
		return
	}
	log.Printf("ui: created backup %s (%d bytes)", info.Name, info.SizeBytes)
	h.renderBackupSection(w, fmt.Sprintf("Created backup %s (%s).", info.Name, humanSize(info.SizeBytes)), false)
}

// downloadBackup handles GET /ui/backups/{name}.
func (h *uiHandler) downloadBackup(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if strings.ContainsAny(name, "/\\") || !strings.HasSuffix(name, ".db") {
		http.Error(w, "invalid backup name", http.StatusBadRequest)
		return
	}
	dir := h.cfg.Backup.Dir
	if dir == "" {
		dir = filepath.Join(h.cfg.Storage.DataDir, "backups")
	}
	cleanDir := filepath.Clean(dir)
	path := filepath.Join(cleanDir, name)
	if !strings.HasPrefix(path, cleanDir+string(filepath.Separator)) {
		http.Error(w, "invalid backup name", http.StatusBadRequest)
		return
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "could not open backup", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "could not stat backup", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

// ---------------------------------------------------------------------------
// Template helper functions
// ---------------------------------------------------------------------------

func humanSize(b int64) string {
	if b == 0 {
		return "—"
	}
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func humanTime(t *time.Time) string {
	if t == nil {
		return "never"
	}
	d := time.Since(*t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return t.Format("Jan 2, 2006")
	}
}

func humanDuration(secs int) string {
	if secs == 0 {
		return "—"
	}
	h := secs / 3600
	m := (secs % 3600) / 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func unixTime(t *time.Time) int64 {
	if t == nil {
		return -1
	}
	return t.Unix()
}

func cacheStatusClass(status string) string {
	switch status {
	case "cached":
		return "badge-cached"
	case "in_progress":
		return "badge-progress"
	case "failed":
		return "badge-failed"
	default:
		return "badge-none"
	}
}
