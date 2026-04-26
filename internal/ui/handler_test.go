package ui_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"podproxy/internal/backup"
	"podproxy/internal/config"
	"podproxy/internal/db"
	"podproxy/internal/feed"
	"podproxy/internal/ui"
)

// newBaseTestComponents creates the RSS test server and opens a test database,
// registering cleanup for both. Called by the two env constructors below.
func newBaseTestComponents(t *testing.T) (*db.DB, *httptest.Server) {
	t.Helper()
	rssSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(uiTestRSS))
	}))
	t.Cleanup(rssSrv.Close)
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database, rssSrv
}

// newUITestEnvWithPrefetcher is like newUITestEnv but registers a live prefetcher.
func newUITestEnvWithPrefetcher(t *testing.T) *uiTestEnv {
	t.Helper()
	database, rssSrv := newBaseTestComponents(t)
	cfg := &config.Config{
		Server:   config.ServerConfig{Port: 8080, BaseURL: "http://proxy.local"},
		Storage:  config.StorageConfig{CacheDir: t.TempDir()},
		Defaults: config.DefaultsConfig{RefreshIntervalMinutes: 60, PrefetchConcurrency: 1},
	}
	prefetcher := feed.NewPrefetcher(database, cfg)
	t.Cleanup(prefetcher.Stop)
	mux := http.NewServeMux()
	ui.RegisterRoutes(mux, database, feed.NewFetcher(cfg), prefetcher, cfg, backup.New(database, cfg))
	return &uiTestEnv{db: database, mux: mux, cfg: cfg, rssSrv: rssSrv}
}

const uiTestRSS = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>UI Test Podcast</title>
    <link>https://example.com</link>
    <description>Test</description>
    <item>
      <title>Episode One</title>
      <guid>ui-guid-001</guid>
      <enclosure url="https://cdn.example.com/ep1.mp3" type="audio/mpeg" length="12345"/>
    </item>
  </channel>
</rss>`

type uiTestEnv struct {
	db     *db.DB
	mux    *http.ServeMux
	cfg    *config.Config
	rssSrv *httptest.Server
}

func newUITestEnv(t *testing.T) *uiTestEnv {
	t.Helper()
	database, rssSrv := newBaseTestComponents(t)
	cfg := &config.Config{
		Server:   config.ServerConfig{Port: 8080, BaseURL: "http://proxy.local"},
		Storage:  config.StorageConfig{CacheDir: t.TempDir()},
		Defaults: config.DefaultsConfig{RefreshIntervalMinutes: 60},
	}
	mux := http.NewServeMux()
	ui.RegisterRoutes(mux, database, feed.NewFetcher(cfg), nil, cfg, backup.New(database, cfg))
	return &uiTestEnv{db: database, mux: mux, cfg: cfg, rssSrv: rssSrv}
}

// do performs a request with no body.
func (e *uiTestEnv) do(method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, req)
	return w
}

// doForm performs a request with a form-encoded body.
func (e *uiTestEnv) doForm(method, path string, values url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, req)
	return w
}

// ---------------------------------------------------------------------------
// GET /ui
// ---------------------------------------------------------------------------

func TestFeedsPage_ReturnsOK(t *testing.T) {
	env := newUITestEnv(t)
	w := env.do("GET", "/ui")

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type: want text/html, got %q", ct)
	}
	if !strings.Contains(w.Body.String(), "PodProxy") {
		t.Error("response body should contain page title")
	}
}

func TestFeedsPage_ShowsAddedFeed(t *testing.T) {
	env := newUITestEnv(t)
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

	w := env.do("GET", "/ui")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "UI Test Podcast") {
		t.Error("feed title should appear on page after being added")
	}
}

// ---------------------------------------------------------------------------
// GET /ui/feeds/{id}/episodes
// ---------------------------------------------------------------------------

func TestEpisodesPage_NotFound_Returns404(t *testing.T) {
	env := newUITestEnv(t)
	w := env.do("GET", "/ui/feeds/nonexistent/episodes")
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
}

func TestEpisodesPage_ReturnsOK(t *testing.T) {
	env := newUITestEnv(t)
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

	w := env.do("GET", "/ui/feeds/ui-test-podcast/episodes")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "UI Test Podcast") {
		t.Error("episodes page should show feed title")
	}
	if !strings.Contains(body, "Episode One") {
		t.Error("episodes page should list episodes")
	}
	if !strings.Contains(body, "proxy.local/feeds/ui-test-podcast.rss") {
		t.Error("episodes page should show proxy URL")
	}
}

// ---------------------------------------------------------------------------
// POST /ui/feeds/add
// ---------------------------------------------------------------------------

func TestUIAddFeed_Success(t *testing.T) {
	env := newUITestEnv(t)
	w := env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "UI Test Podcast") {
		t.Error("success message should reference feed title")
	}
	if !strings.Contains(w.Body.String(), "alert-ok") {
		t.Error("response should contain success alert class")
	}
}

func TestUIAddFeed_StoresEpisodesInDB(t *testing.T) {
	env := newUITestEnv(t)
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

	eps, err := env.db.ListEpisodesByFeed("ui-test-podcast")
	if err != nil {
		t.Fatalf("ListEpisodesByFeed: %v", err)
	}
	if len(eps) != 1 {
		t.Fatalf("want 1 episode in DB, got %d", len(eps))
	}
	if eps[0].OriginalURL != "https://cdn.example.com/ep1.mp3" {
		t.Errorf("episode url: want %q, got %q", "https://cdn.example.com/ep1.mp3", eps[0].OriginalURL)
	}
}

func TestUIAddFeed_EmptyURL_ShowsError(t *testing.T) {
	env := newUITestEnv(t)
	w := env.doForm("POST", "/ui/feeds/add", url.Values{"url": {""}})

	if w.Code != http.StatusOK {
		t.Fatalf("want 200 (rendered fragment), got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "alert-err") {
		t.Error("empty URL should render error alert")
	}
}

func TestUIAddFeed_UnreachableURL_ShowsError(t *testing.T) {
	env := newUITestEnv(t)
	w := env.doForm("POST", "/ui/feeds/add", url.Values{"url": {"http://127.0.0.1:1"}})

	if w.Code != http.StatusOK {
		t.Fatalf("want 200 (rendered fragment), got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "alert-err") {
		t.Error("unreachable URL should render error alert")
	}
}

func TestUIAddFeed_Duplicate_ShowsMessage(t *testing.T) {
	env := newUITestEnv(t)
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})
	w := env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "already exists") {
		t.Error("duplicate add should show 'already exists' message")
	}
}

// ---------------------------------------------------------------------------
// DELETE /ui/feeds/{id}
// ---------------------------------------------------------------------------

func TestUIDeleteFeed_Success(t *testing.T) {
	env := newUITestEnv(t)
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

	w := env.do("DELETE", "/ui/feeds/ui-test-podcast")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Deleted") {
		t.Error("delete response should confirm deletion")
	}

	// Feed should be gone from the DB.
	_, err := env.db.GetFeed("ui-test-podcast")
	if err == nil {
		t.Error("feed should have been removed from DB after delete")
	}
}

func TestUIDeleteFeed_InProgress_ShowsError(t *testing.T) {
	env := newUITestEnv(t)
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})
	eps, _ := env.db.ListEpisodesByFeed("ui-test-podcast")
	if len(eps) == 0 {
		t.Skip("no episodes in test feed")
	}
	if err := env.db.UpdateEpisodeCacheStatus(eps[0].ID, "in_progress", nil, 0, ""); err != nil {
		t.Fatalf("setup: UpdateEpisodeCacheStatus: %v", err)
	}

	w := env.do("DELETE", "/ui/feeds/ui-test-podcast")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 (rendered fragment), got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "alert-err") {
		t.Error("delete with in-progress episode should render error alert")
	}

	// Feed should still exist in DB.
	if _, err := env.db.GetFeed("ui-test-podcast"); errors.Is(err, db.ErrNotFound) {
		t.Error("feed should not have been deleted")
	}
}

func TestUIDeleteFeed_NotFound_ShowsError(t *testing.T) {
	env := newUITestEnv(t)
	w := env.do("DELETE", "/ui/feeds/nonexistent")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 (rendered fragment), got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "alert-err") {
		t.Error("deleting non-existent feed should render error alert")
	}
}

func TestUIDeleteFeed_RemovesArtworkFile(t *testing.T) {
	env := newUITestEnv(t)
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

	feedsDir := filepath.Join(env.cfg.Storage.CacheDir, "feeds")
	if err := os.MkdirAll(feedsDir, 0o755); err != nil {
		t.Fatalf("mkdir feedsDir: %v", err)
	}
	artPath := filepath.Join(feedsDir, "ui-test-podcast-artwork.jpg")
	if err := os.WriteFile(artPath, []byte("fake-art"), 0o644); err != nil {
		t.Fatalf("write artwork: %v", err)
	}

	w := env.do("DELETE", "/ui/feeds/ui-test-podcast")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}

	if _, err := os.Stat(artPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("artwork file should have been deleted on feed delete, stat err: %v", err)
	}
}

// ---------------------------------------------------------------------------
// POST /ui/feeds/{id}/refresh
// ---------------------------------------------------------------------------

func TestUIRefreshFeed_Success(t *testing.T) {
	env := newUITestEnv(t)
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

	w := env.doForm("POST", "/ui/feeds/ui-test-podcast/refresh", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Refreshed") {
		t.Error("refresh response should confirm refresh")
	}
}

func TestUIRefreshFeed_NotFound_ShowsError(t *testing.T) {
	env := newUITestEnv(t)
	w := env.doForm("POST", "/ui/feeds/nonexistent/refresh", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 (rendered fragment), got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "alert-err") {
		t.Error("refreshing non-existent feed should render error alert")
	}
}

// ---------------------------------------------------------------------------
// POST /ui/feeds/{id}/refresh-artwork
// ---------------------------------------------------------------------------

func TestUIRefreshArtwork_NotFound_ShowsError(t *testing.T) {
	env := newUITestEnv(t)
	w := env.doForm("POST", "/ui/feeds/nonexistent/refresh-artwork", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 (rendered fragment), got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "alert-err") {
		t.Error("refreshing artwork for non-existent feed should render error alert")
	}
}

func TestUIRefreshArtwork_NoArtworkURL_ShowsMessage(t *testing.T) {
	env := newUITestEnv(t)
	// uiTestRSS has no itunes:image, so ArtworkURL will be empty.
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

	w := env.doForm("POST", "/ui/feeds/ui-test-podcast/refresh-artwork", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 (rendered fragment), got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "alert-err") {
		t.Error("missing artwork URL should not be an error")
	}
	if !strings.Contains(body, "no artwork") {
		t.Errorf("response should mention no artwork URL; got: %s", body)
	}
}

func TestUIRefreshArtwork_RefreshesArtworkFile(t *testing.T) {
	artSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("artwork-bytes"))
	}))
	t.Cleanup(artSrv.Close)

	rssSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd">
  <channel>
    <title>Art Refresh Test</title>
    <link>https://example.com</link>
    <description>Test</description>
    <itunes:image href="%s/cover.png"/>
    <item>
      <title>Episode One</title>
      <guid>art-refresh-001</guid>
      <enclosure url="https://cdn.example.com/ep1.mp3" type="audio/mpeg" length="100"/>
    </item>
  </channel>
</rss>`, artSrv.URL)
	}))
	t.Cleanup(rssSrv.Close)

	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	cfg := &config.Config{
		Server:   config.ServerConfig{Port: 8080, BaseURL: "http://proxy.local"},
		Storage:  config.StorageConfig{CacheDir: t.TempDir()},
		Defaults: config.DefaultsConfig{RefreshIntervalMinutes: 60},
	}
	mux := http.NewServeMux()
	ui.RegisterRoutes(mux, database, feed.NewFetcher(cfg), nil, cfg, backup.New(database, cfg))
	env := &uiTestEnv{db: database, mux: mux, cfg: cfg, rssSrv: rssSrv}

	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {rssSrv.URL}})

	w := env.doForm("POST", "/ui/feeds/art-refresh-test/refresh-artwork", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Refreshed artwork") {
		t.Errorf("response should confirm artwork refresh; got: %s", w.Body.String())
	}

	feedsDir := filepath.Join(cfg.Storage.CacheDir, "feeds")
	artPath, ok := feed.ArtworkPath(feedsDir, "art-refresh-test")
	if !ok {
		t.Fatal("artwork file should exist after refresh")
	}
	data, err := os.ReadFile(artPath)
	if err != nil {
		t.Fatalf("read artwork file: %v", err)
	}
	if string(data) != "artwork-bytes" {
		t.Errorf("artwork content: want %q, got %q", "artwork-bytes", string(data))
	}
}

// ---------------------------------------------------------------------------
// POST /ui/feeds/{id}/toggle-autoprefetch
// ---------------------------------------------------------------------------

func TestUIToggleAutoPrefetch_NotFound_ShowsError(t *testing.T) {
	env := newUITestEnv(t)
	w := env.doForm("POST", "/ui/feeds/nonexistent/toggle-autoprefetch", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 (rendered fragment), got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "alert-err") {
		t.Error("toggling non-existent feed should render error alert")
	}
}

func TestUIToggleAutoPrefetch_EnablesThenDisables(t *testing.T) {
	env := newUITestEnv(t)
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

	// First toggle: off → on
	w := env.doForm("POST", "/ui/feeds/ui-test-podcast/toggle-autoprefetch", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "enabled") {
		t.Error("first toggle should report enabled")
	}

	// Second toggle: on → off
	w = env.doForm("POST", "/ui/feeds/ui-test-podcast/toggle-autoprefetch", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "disabled") {
		t.Error("second toggle should report disabled")
	}
}

// ---------------------------------------------------------------------------
// POST /ui/feeds/{id}/episodes/{epid}/cache
// ---------------------------------------------------------------------------

func TestUICacheEpisode_NotFound_ShowsError(t *testing.T) {
	env := newUITestEnv(t)
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

	w := env.do("POST", "/ui/feeds/ui-test-podcast/episodes/nonexistent/cache")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 (rendered fragment), got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "alert-err") {
		t.Error("unknown episode should render error alert")
	}
}

func TestUICacheEpisode_AlreadyCached_ShowsMessage(t *testing.T) {
	env := newUITestEnv(t)
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

	eps, err := env.db.ListEpisodesByFeed("ui-test-podcast")
	if err != nil || len(eps) == 0 {
		t.Fatalf("setup: ListEpisodesByFeed: %v (count=%d)", err, len(eps))
	}
	cachedPath := "/some/path"
	if err := env.db.UpdateEpisodeCacheStatus(eps[0].ID, "cached", &cachedPath, 1234, "audio/mpeg"); err != nil {
		t.Fatalf("setup: UpdateEpisodeCacheStatus: %v", err)
	}

	w := env.do("POST", "/ui/feeds/ui-test-podcast/episodes/"+eps[0].URLID+"/cache")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 (rendered fragment), got %d", w.Code)
	}
	body := w.Body.String()
	// Should be informational (not an error) and not queue the episode.
	if strings.Contains(body, "alert-err") {
		t.Error("already-cached episode should not render an error alert")
	}
	if !strings.Contains(body, "already cached") {
		t.Error("response should mention episode is already cached")
	}
}

func TestUICacheEpisode_InProgress_ShowsError(t *testing.T) {
	env := newUITestEnv(t)
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

	eps, err := env.db.ListEpisodesByFeed("ui-test-podcast")
	if err != nil || len(eps) == 0 {
		t.Fatalf("setup: ListEpisodesByFeed: %v (count=%d)", err, len(eps))
	}
	if err := env.db.UpdateEpisodeCacheStatus(eps[0].ID, "in_progress", nil, 0, ""); err != nil {
		t.Fatalf("setup: UpdateEpisodeCacheStatus: %v", err)
	}

	w := env.do("POST", "/ui/feeds/ui-test-podcast/episodes/"+eps[0].URLID+"/cache")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 (rendered fragment), got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "alert-err") {
		t.Error("in-progress episode should render error alert")
	}
}

// ---------------------------------------------------------------------------
// DELETE /ui/feeds/{id}/episodes/{epid}
// ---------------------------------------------------------------------------

func TestUIDeleteEpisodeCache_NotFound_ShowsError(t *testing.T) {
	env := newUITestEnv(t)
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

	w := env.do("DELETE", "/ui/feeds/ui-test-podcast/episodes/nonexistent")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 (rendered fragment), got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "alert-err") {
		t.Error("unknown episode should render error alert")
	}
}

// TestUIDeleteEpisodeCache_InProgress_ShowsError verifies the bug fix: a
// download in flight must not be deletable, as removing the file under an
// active io.TeeReader write would corrupt the cache entry.
func TestUIDeleteEpisodeCache_InProgress_ShowsError(t *testing.T) {
	env := newUITestEnv(t)
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

	eps, err := env.db.ListEpisodesByFeed("ui-test-podcast")
	if err != nil || len(eps) == 0 {
		t.Fatalf("setup: ListEpisodesByFeed: %v (count=%d)", err, len(eps))
	}
	if err := env.db.UpdateEpisodeCacheStatus(eps[0].ID, "in_progress", nil, 0, ""); err != nil {
		t.Fatalf("setup: UpdateEpisodeCacheStatus: %v", err)
	}

	w := env.do("DELETE", "/ui/feeds/ui-test-podcast/episodes/"+eps[0].URLID)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 (rendered fragment), got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "alert-err") {
		t.Error("deleting an in-progress episode should render error alert")
	}

	// Status must be unchanged in the DB.
	updated, err := env.db.GetEpisodeByURLID("ui-test-podcast", eps[0].URLID)
	if err != nil {
		t.Fatalf("GetEpisodeByURLID after rejected delete: %v", err)
	}
	if updated.CacheStatus != "in_progress" {
		t.Errorf("cache status should still be in_progress, got %q", updated.CacheStatus)
	}
}

func TestUIDeleteEpisodeCache_Cached_DeletesFileAndResetsStatus(t *testing.T) {
	env := newUITestEnv(t)
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

	eps, err := env.db.ListEpisodesByFeed("ui-test-podcast")
	if err != nil || len(eps) == 0 {
		t.Fatalf("setup: ListEpisodesByFeed: %v (count=%d)", err, len(eps))
	}

	// Create a real file to simulate a cached episode.
	f, err := os.CreateTemp(t.TempDir(), "ep-*.mp3")
	if err != nil {
		t.Fatalf("setup: create temp file: %v", err)
	}
	cachedPath := f.Name()
	f.Close()

	if err := env.db.UpdateEpisodeCacheStatus(eps[0].ID, "cached", &cachedPath, 999, "audio/mpeg"); err != nil {
		t.Fatalf("setup: UpdateEpisodeCacheStatus: %v", err)
	}

	w := env.do("DELETE", "/ui/feeds/ui-test-podcast/episodes/"+eps[0].URLID)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "alert-ok") {
		t.Error("successful delete should render success alert")
	}

	// File must be gone.
	if _, err := os.Stat(cachedPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cached file should have been deleted, got stat err: %v", err)
	}

	// DB status must be reset to none.
	updated, err := env.db.GetEpisodeByURLID("ui-test-podcast", eps[0].URLID)
	if err != nil {
		t.Fatalf("GetEpisodeByURLID after delete: %v", err)
	}
	if updated.CacheStatus != "none" {
		t.Errorf("cache status should be none after delete, got %q", updated.CacheStatus)
	}
}

// ---------------------------------------------------------------------------
// GET /ui/feeds/{id}/episode-list
// ---------------------------------------------------------------------------

func TestEpisodeListFragment_Returns200(t *testing.T) {
	env := newUITestEnv(t)
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

	w := env.do("GET", "/ui/feeds/ui-test-podcast/episode-list")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type: want text/html, got %q", ct)
	}
	if !strings.Contains(w.Body.String(), "episode-list") {
		t.Error("response should contain the episode-list fragment")
	}
}

func TestEpisodeListFragment_NotFound_Returns404(t *testing.T) {
	env := newUITestEnv(t)
	w := env.do("GET", "/ui/feeds/nonexistent/episode-list")
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// POST /ui/feeds/{id}/bulk-cache
// ---------------------------------------------------------------------------

func TestUIBulkCacheEpisodes_NilPrefetcher_ShowsError(t *testing.T) {
	env := newUITestEnv(t) // prefetcher is nil
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

	eps, err := env.db.ListEpisodesByFeed("ui-test-podcast")
	if err != nil || len(eps) == 0 {
		t.Fatalf("setup: ListEpisodesByFeed: %v (count=%d)", err, len(eps))
	}

	w := env.doForm("POST", "/ui/feeds/ui-test-podcast/bulk-cache", url.Values{"ep": {eps[0].URLID}})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 (rendered fragment), got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "alert-err") {
		t.Error("nil prefetcher should render error alert")
	}
}

func TestUIBulkCacheEpisodes_NoEpisodesSelected_ShowsMessage(t *testing.T) {
	env := newUITestEnvWithPrefetcher(t)
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

	w := env.doForm("POST", "/ui/feeds/ui-test-podcast/bulk-cache", url.Values{})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 (rendered fragment), got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "No episodes selected") {
		t.Error("response should mention no episodes were selected")
	}
}

func TestUIBulkCacheEpisodes_QueuesUncachedEpisodes(t *testing.T) {
	env := newUITestEnvWithPrefetcher(t)
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

	eps, err := env.db.ListEpisodesByFeed("ui-test-podcast")
	if err != nil || len(eps) == 0 {
		t.Fatalf("setup: ListEpisodesByFeed: %v (count=%d)", err, len(eps))
	}

	w := env.doForm("POST", "/ui/feeds/ui-test-podcast/bulk-cache", url.Values{"ep": {eps[0].URLID}})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "alert-err") {
		t.Error("successful bulk-cache should not render error alert")
	}
	if !strings.Contains(body, "Queued 1 episode") {
		t.Errorf("response should confirm queued count; got: %s", body)
	}
}

func TestUIBulkCacheEpisodes_SkipsCachedEpisodes(t *testing.T) {
	env := newUITestEnvWithPrefetcher(t)
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

	eps, err := env.db.ListEpisodesByFeed("ui-test-podcast")
	if err != nil || len(eps) == 0 {
		t.Fatalf("setup: ListEpisodesByFeed: %v (count=%d)", err, len(eps))
	}
	cachedPath := "/some/path"
	if err := env.db.UpdateEpisodeCacheStatus(eps[0].ID, "cached", &cachedPath, 1234, "audio/mpeg"); err != nil {
		t.Fatalf("setup: UpdateEpisodeCacheStatus: %v", err)
	}

	w := env.doForm("POST", "/ui/feeds/ui-test-podcast/bulk-cache", url.Values{"ep": {eps[0].URLID}})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "already cached or in progress") {
		t.Errorf("response should mention skipped episodes; got: %s", body)
	}
}

func TestUIBulkCacheEpisodes_SkipsInProgressEpisodes(t *testing.T) {
	env := newUITestEnvWithPrefetcher(t)
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

	eps, err := env.db.ListEpisodesByFeed("ui-test-podcast")
	if err != nil || len(eps) == 0 {
		t.Fatalf("setup: ListEpisodesByFeed: %v (count=%d)", err, len(eps))
	}
	if err := env.db.UpdateEpisodeCacheStatus(eps[0].ID, "in_progress", nil, 0, ""); err != nil {
		t.Fatalf("setup: UpdateEpisodeCacheStatus: %v", err)
	}

	w := env.doForm("POST", "/ui/feeds/ui-test-podcast/bulk-cache", url.Values{"ep": {eps[0].URLID}})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "already cached or in progress") {
		t.Errorf("response should mention skipped in-progress episode; got: %s", body)
	}
	// Status must not have changed.
	updated, err := env.db.GetEpisodeByURLID("ui-test-podcast", eps[0].URLID)
	if err != nil {
		t.Fatalf("GetEpisodeByURLID: %v", err)
	}
	if updated.CacheStatus != "in_progress" {
		t.Errorf("cache status should still be in_progress, got %q", updated.CacheStatus)
	}
}

// ---------------------------------------------------------------------------
// POST /ui/feeds/{id}/bulk-delete
// ---------------------------------------------------------------------------

func TestUIBulkDeleteEpisodes_NoEpisodesSelected_ShowsMessage(t *testing.T) {
	env := newUITestEnv(t)
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

	w := env.doForm("POST", "/ui/feeds/ui-test-podcast/bulk-delete", url.Values{})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 (rendered fragment), got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "No episodes selected") {
		t.Error("response should mention no episodes were selected")
	}
}

func TestUIBulkDeleteEpisodes_DeletesCachedEpisodes(t *testing.T) {
	env := newUITestEnv(t)
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

	eps, err := env.db.ListEpisodesByFeed("ui-test-podcast")
	if err != nil || len(eps) == 0 {
		t.Fatalf("setup: ListEpisodesByFeed: %v (count=%d)", err, len(eps))
	}

	// Create a real file to simulate a cached episode.
	f, err := os.CreateTemp(t.TempDir(), "ep-*.mp3")
	if err != nil {
		t.Fatalf("setup: create temp file: %v", err)
	}
	cachedPath := f.Name()
	f.Close()

	if err := env.db.UpdateEpisodeCacheStatus(eps[0].ID, "cached", &cachedPath, 999, "audio/mpeg"); err != nil {
		t.Fatalf("setup: UpdateEpisodeCacheStatus: %v", err)
	}

	w := env.doForm("POST", "/ui/feeds/ui-test-podcast/bulk-delete", url.Values{"ep": {eps[0].URLID}})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "alert-err") {
		t.Error("successful bulk-delete should not render error alert")
	}
	if !strings.Contains(body, "Deleted 1 cached file") {
		t.Errorf("response should confirm deletion count; got: %s", body)
	}

	// File must be gone.
	if _, err := os.Stat(cachedPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cached file should have been deleted, stat err: %v", err)
	}

	// DB status must be reset to none.
	updated, err := env.db.GetEpisodeByURLID("ui-test-podcast", eps[0].URLID)
	if err != nil {
		t.Fatalf("GetEpisodeByURLID after bulk-delete: %v", err)
	}
	if updated.CacheStatus != "none" {
		t.Errorf("cache status should be none after delete, got %q", updated.CacheStatus)
	}
}

// TestUIBulkDeleteEpisodes_SkipsNonCachedEpisodes verifies that the server
// ignores episode IDs whose status is not "cached" — guarding against a client
// sending a mixed selection or a stale payload.
func TestUIBulkDeleteEpisodes_SkipsNonCachedEpisodes(t *testing.T) {
	for _, status := range []string{"none", "failed"} {
		t.Run(status, func(t *testing.T) {
			env := newUITestEnv(t)
			env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

			eps, err := env.db.ListEpisodesByFeed("ui-test-podcast")
			if err != nil || len(eps) == 0 {
				t.Fatalf("setup: ListEpisodesByFeed: %v (count=%d)", err, len(eps))
			}
			if status != "none" {
				if err := env.db.UpdateEpisodeCacheStatus(eps[0].ID, status, nil, 0, ""); err != nil {
					t.Fatalf("setup: UpdateEpisodeCacheStatus: %v", err)
				}
			}

			w := env.doForm("POST", "/ui/feeds/ui-test-podcast/bulk-delete", url.Values{"ep": {eps[0].URLID}})
			if w.Code != http.StatusOK {
				t.Fatalf("want 200, got %d\nbody: %s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			if !strings.Contains(body, "skipped") {
				t.Errorf("response should mention skipped episode; got: %s", body)
			}
			// Status must be unchanged.
			updated, err := env.db.GetEpisodeByURLID("ui-test-podcast", eps[0].URLID)
			if err != nil {
				t.Fatalf("GetEpisodeByURLID: %v", err)
			}
			if updated.CacheStatus != status {
				t.Errorf("cache status should still be %q, got %q", status, updated.CacheStatus)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// POST /ui/backups  &  GET /ui/backups/{name}
// ---------------------------------------------------------------------------

// newUIBackupTestEnv creates a test env with a properly isolated backup dir.
func newUIBackupTestEnv(t *testing.T) *uiTestEnv {
	t.Helper()
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	cfg := &config.Config{
		Server:   config.ServerConfig{BaseURL: "http://proxy.local"},
		Storage:  config.StorageConfig{DataDir: t.TempDir()},
		Defaults: config.DefaultsConfig{RefreshIntervalMinutes: 60},
		Backup:   config.BackupConfig{MaxBackups: 5},
	}
	mux := http.NewServeMux()
	ui.RegisterRoutes(mux, database, feed.NewFetcher(cfg), nil, cfg, backup.New(database, cfg))
	return &uiTestEnv{db: database, mux: mux, cfg: cfg}
}

func TestFeedsPage_ShowsBackupSection(t *testing.T) {
	env := newUITestEnv(t)
	w := env.do("GET", "/ui")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "backup-section") {
		t.Error("feeds page should contain backup-section")
	}
	if !strings.Contains(body, "Database Backups") {
		t.Error("feeds page should contain backup section heading")
	}
	if !strings.Contains(body, "Create Backup") {
		t.Error("feeds page should contain create backup button")
	}
}

func TestUICreateBackup_Returns200AndShowsSuccessMessage(t *testing.T) {
	env := newUIBackupTestEnv(t)

	w := env.doForm("POST", "/ui/backups", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type: want text/html, got %q", ct)
	}
	body := w.Body.String()
	if strings.Contains(body, "alert-err") {
		t.Error("create backup should not render error alert")
	}
	if !strings.Contains(body, "Created backup") {
		t.Errorf("response should confirm backup creation; got: %s", body)
	}
	if !strings.Contains(body, ".db") {
		t.Errorf("response should include backup filename; got: %s", body)
	}
}

func TestUICreateBackup_NewBackupAppearsOnFeedsPage(t *testing.T) {
	env := newUIBackupTestEnv(t)

	env.doForm("POST", "/ui/backups", nil)

	w := env.do("GET", "/ui")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), ".db") {
		t.Error("feeds page should show the created backup filename")
	}
}

func TestUIDownloadBackup_NotFound_Returns404(t *testing.T) {
	env := newUIBackupTestEnv(t)
	w := env.do("GET", "/ui/backups/nonexistent.db")
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
}

func TestUIDownloadBackup_InvalidExtension_Returns400(t *testing.T) {
	env := newUIBackupTestEnv(t)
	w := env.do("GET", "/ui/backups/backup.sql")
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestUIDownloadBackup_Returns200WithFile(t *testing.T) {
	env := newUIBackupTestEnv(t)

	// Create a backup via the UI endpoint.
	createW := env.doForm("POST", "/ui/backups", nil)
	if createW.Code != http.StatusOK {
		t.Fatalf("setup: create backup: want 200, got %d\nbody: %s", createW.Code, createW.Body.String())
	}

	// Resolve the backup name via a fresh manager reading the same dir.
	bm := backup.New(env.db, env.cfg)
	backups, err := bm.ListBackups()
	if err != nil || len(backups) == 0 {
		t.Fatalf("setup: ListBackups: %v (count=%d)", err, len(backups))
	}
	name := backups[0].Name

	w := env.do("GET", "/ui/backups/"+name)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type: want application/octet-stream, got %q", ct)
	}
	cd := w.Header().Get("Content-Disposition")
	if !strings.Contains(cd, name) {
		t.Errorf("Content-Disposition should contain filename %q, got %q", name, cd)
	}
	if w.Body.Len() == 0 {
		t.Error("response body should not be empty")
	}
}

// TestUIBulkDeleteEpisodes_SkipsInProgressEpisodes verifies that an in-flight
// download is not interrupted: the file write is active and removing it would
// corrupt the cache entry.
func TestUIBulkDeleteEpisodes_SkipsInProgressEpisodes(t *testing.T) {
	env := newUITestEnv(t)
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

	eps, err := env.db.ListEpisodesByFeed("ui-test-podcast")
	if err != nil || len(eps) == 0 {
		t.Fatalf("setup: ListEpisodesByFeed: %v (count=%d)", err, len(eps))
	}
	if err := env.db.UpdateEpisodeCacheStatus(eps[0].ID, "in_progress", nil, 0, ""); err != nil {
		t.Fatalf("setup: UpdateEpisodeCacheStatus: %v", err)
	}

	w := env.doForm("POST", "/ui/feeds/ui-test-podcast/bulk-delete", url.Values{"ep": {eps[0].URLID}})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "skipped") {
		t.Errorf("response should mention skipped in-progress episode; got: %s", body)
	}
	// Status must be unchanged.
	updated, err := env.db.GetEpisodeByURLID("ui-test-podcast", eps[0].URLID)
	if err != nil {
		t.Fatalf("GetEpisodeByURLID: %v", err)
	}
	if updated.CacheStatus != "in_progress" {
		t.Errorf("cache status should still be in_progress, got %q", updated.CacheStatus)
	}
}

// TestUIBulkDeleteEpisodes_MixedSelection verifies that a payload containing
// both cached and non-cached episode IDs reports the correct deleted/skipped
// counts and only resets the status of episodes that were actually deleted.
func TestUIBulkDeleteEpisodes_MixedSelection(t *testing.T) {
	env := newUITestEnv(t)
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

	eps, err := env.db.ListEpisodesByFeed("ui-test-podcast")
	if err != nil || len(eps) == 0 {
		t.Fatalf("setup: ListEpisodesByFeed: %v (count=%d)", err, len(eps))
	}

	// Insert a second episode manually so we have one cached and one not.
	ep2 := &db.Episode{
		ID:          "ui-test-podcast/ep2",
		FeedID:      "ui-test-podcast",
		URLID:       "ep2",
		Title:       "Episode Two",
		OriginalURL: "https://cdn.example.com/ep2.mp3",
		CacheStatus: "none",
	}
	if err := env.db.UpsertEpisode(ep2); err != nil {
		t.Fatalf("setup: UpsertEpisode: %v", err)
	}

	// Create a real file and mark eps[0] as cached.
	f, err := os.CreateTemp(t.TempDir(), "ep-*.mp3")
	if err != nil {
		t.Fatalf("setup: create temp file: %v", err)
	}
	cachedPath := f.Name()
	f.Close()
	if err := env.db.UpdateEpisodeCacheStatus(eps[0].ID, "cached", &cachedPath, 999, "audio/mpeg"); err != nil {
		t.Fatalf("setup: UpdateEpisodeCacheStatus: %v", err)
	}

	// Submit both IDs: one cached (should delete) and one none (should skip).
	w := env.doForm("POST", "/ui/feeds/ui-test-podcast/bulk-delete",
		url.Values{"ep": {eps[0].URLID, ep2.URLID}})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Deleted 1 cached file") {
		t.Errorf("response should confirm 1 deletion; got: %s", body)
	}
	if !strings.Contains(body, "skipped") {
		t.Errorf("response should report skipped count; got: %s", body)
	}

	// Cached episode's file and DB status must be reset.
	if _, err := os.Stat(cachedPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cached file should have been deleted, stat err: %v", err)
	}
	updatedCached, err := env.db.GetEpisodeByURLID("ui-test-podcast", eps[0].URLID)
	if err != nil {
		t.Fatalf("GetEpisodeByURLID after delete: %v", err)
	}
	if updatedCached.CacheStatus != "none" {
		t.Errorf("deleted episode status should be none, got %q", updatedCached.CacheStatus)
	}

	// Non-cached episode must be untouched.
	updatedSkipped, err := env.db.GetEpisodeByURLID("ui-test-podcast", ep2.URLID)
	if err != nil {
		t.Fatalf("GetEpisodeByURLID for skipped: %v", err)
	}
	if updatedSkipped.CacheStatus != "none" {
		t.Errorf("skipped episode status should still be none, got %q", updatedSkipped.CacheStatus)
	}
}
