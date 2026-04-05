package ui

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	}
	feedsTmpl = template.Must(
		template.New("").Funcs(funcs).ParseFS(tmplFS,
			"templates/base.html", "templates/feeds.html"),
	)
	episodesTmpl = template.Must(
		template.New("").Funcs(funcs).ParseFS(tmplFS,
			"templates/base.html", "templates/episodes.html"),
	)
}

// RegisterRoutes mounts the HTMX UI under /ui.
func RegisterRoutes(mux *http.ServeMux, database *db.DB, fetcher *feed.Fetcher, prefetcher *feed.Prefetcher, cfg *config.Config) {
	h := &uiHandler{db: database, fetcher: fetcher, prefetcher: prefetcher, cfg: cfg}
	mux.HandleFunc("GET /ui", h.feedsPage)
	mux.HandleFunc("GET /ui/feeds/{id}/episodes", h.episodesPage)
	mux.HandleFunc("POST /ui/feeds/add", h.addFeed)
	mux.HandleFunc("DELETE /ui/feeds/{id}", h.deleteFeed)
	mux.HandleFunc("POST /ui/feeds/{id}/refresh", h.refreshFeed)
	mux.HandleFunc("POST /ui/feeds/{id}/prefetch", h.prefetchFeed)
	mux.HandleFunc("POST /ui/feeds/{id}/episodes/{epid}/cache", h.cacheEpisode)
	mux.HandleFunc("DELETE /ui/feeds/{id}/episodes/{epid}", h.deleteEpisodeCache)
}

type uiHandler struct {
	db         *db.DB
	fetcher    *feed.Fetcher
	prefetcher *feed.Prefetcher
	cfg        *config.Config
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
		Feed:     f,
		Episodes: eps,
		ProxyURL: fmt.Sprintf("%s/feeds/%s.rss", h.cfg.Server.BaseURL, f.ID),
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
		Feed:     &feedCopy,
		Episodes: tmpResult.Episodes,
		RawXML:   tmpResult.RawXML,
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
	feedXML := filepath.Join(h.cfg.Storage.CacheDir, "feeds", id+".rss")
	if err := os.Remove(feedXML); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("ui: remove feed xml cache %s: %v", feedXML, err)
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

func (h *uiHandler) prefetchFeed(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := h.db.GetFeed(id); errors.Is(err, db.ErrNotFound) {
		h.renderFeedList(w, "Feed not found.", true)
		return
	} else if err != nil {
		h.renderFeedList(w, "Database error.", true)
		return
	}

	if h.prefetcher == nil {
		h.renderFeedList(w, "Prefetcher not available.", true)
		return
	}
	h.prefetcher.EnqueueFeedEpisodes(id)
	h.renderFeedList(w, fmt.Sprintf("Queued %q for prefetch.", id), false)
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
		Feed:     f,
		Episodes: eps,
		ProxyURL: fmt.Sprintf("%s/feeds/%s.rss", h.cfg.Server.BaseURL, f.ID),
		Message:  message,
		IsError:  isError,
	}
	w.Header().Set("Content-Type", "text/html")
	if err := episodesTmpl.ExecuteTemplate(w, "episode-list", data); err != nil {
		log.Printf("ui: render episode-list fragment: %v", err)
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

type feedsPageData struct {
	Feeds   []*db.FeedWithStats
	Message string
	IsError bool
	BaseURL string
}

type episodesPageData struct {
	Feed     *db.Feed
	Episodes []*db.Episode
	ProxyURL string
	Message  string
	IsError  bool
}

func (h *uiHandler) buildFeedsData(message string) (*feedsPageData, error) {
	feeds, err := h.db.ListFeedsWithStats()
	if err != nil {
		return nil, err
	}
	return &feedsPageData{
		Feeds:   feeds,
		Message: message,
		BaseURL: h.cfg.Server.BaseURL,
	}, nil
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
