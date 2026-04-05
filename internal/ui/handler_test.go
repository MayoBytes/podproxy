package ui_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"podproxy/internal/config"
	"podproxy/internal/db"
	"podproxy/internal/feed"
	"podproxy/internal/ui"
)

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

	cfg := &config.Config{
		Server: config.ServerConfig{
			Port:    8080,
			BaseURL: "http://proxy.local",
		},
		Storage:  config.StorageConfig{CacheDir: t.TempDir()},
		Defaults: config.DefaultsConfig{RefreshIntervalMinutes: 60},
	}

	mux := http.NewServeMux()
	ui.RegisterRoutes(mux, database, feed.NewFetcher(cfg), nil, cfg)

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
	env.db.UpdateEpisodeCacheStatus(eps[0].ID, "in_progress", nil, 0, "")

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
// POST /ui/feeds/{id}/prefetch
// ---------------------------------------------------------------------------

func TestUIPrefetchFeed_NotFound_ShowsError(t *testing.T) {
	env := newUITestEnv(t)
	w := env.doForm("POST", "/ui/feeds/nonexistent/prefetch", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 (rendered fragment), got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "alert-err") {
		t.Error("prefetching non-existent feed should render error alert")
	}
}

func TestUIPrefetchFeed_NilPrefetcher_ShowsError(t *testing.T) {
	env := newUITestEnv(t) // prefetcher is nil
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {env.rssSrv.URL}})

	w := env.doForm("POST", "/ui/feeds/ui-test-podcast/prefetch", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "alert-err") {
		t.Error("nil prefetcher should render error alert")
	}
}

func TestUIPrefetchFeed_Success(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	rssSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(uiTestRSS))
	}))
	t.Cleanup(rssSrv.Close)

	cfg := &config.Config{
		Server:   config.ServerConfig{BaseURL: "http://proxy.local"},
		Storage:  config.StorageConfig{CacheDir: t.TempDir()},
		Defaults: config.DefaultsConfig{RefreshIntervalMinutes: 60, PrefetchConcurrency: 1},
	}

	prefetcher := feed.NewPrefetcher(database, cfg)
	t.Cleanup(prefetcher.Stop)

	mux := http.NewServeMux()
	ui.RegisterRoutes(mux, database, feed.NewFetcher(cfg), prefetcher, cfg)

	env := &uiTestEnv{db: database, mux: mux, cfg: cfg, rssSrv: rssSrv}
	env.doForm("POST", "/ui/feeds/add", url.Values{"url": {rssSrv.URL}})

	w := env.doForm("POST", "/ui/feeds/ui-test-podcast/prefetch", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "alert-ok") {
		t.Error("prefetch success should render success alert")
	}
	if !strings.Contains(w.Body.String(), "Queued") {
		t.Error("prefetch success should mention queuing")
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
