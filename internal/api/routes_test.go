package api_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"podproxy/internal/api"
	"podproxy/internal/backup"
	"podproxy/internal/config"
	"podproxy/internal/db"
	"podproxy/internal/feed"
)

const apiTestRSS = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>My Test Podcast</title>
    <link>https://example.com</link>
    <description>Test</description>
    <item>
      <title>Episode One</title>
      <guid>guid-001</guid>
      <enclosure url="https://cdn.example.com/ep1.mp3" type="audio/mpeg" length="12345"/>
    </item>
  </channel>
</rss>`

// apiTestEnv holds everything needed to exercise the API handler.
type apiTestEnv struct {
	db     *db.DB
	mux    *http.ServeMux
	cfg    *config.Config
	rssSrv *httptest.Server // upstream RSS server
}

func newAPITestEnv(t *testing.T) *apiTestEnv {
	t.Helper()

	rssSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(apiTestRSS))
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
		// Default to a temp cache dir so handlers that write through to the
		// filesystem (artwork, rewritten .rss) never leak files into the
		// source tree when individual tests forget to set CacheDir.
		Storage:  config.StorageConfig{CacheDir: t.TempDir()},
		Defaults: config.DefaultsConfig{RefreshIntervalMinutes: 60},
	}

	mux := http.NewServeMux()
	api.RegisterRoutes(mux, database, feed.NewFetcher(cfg), nil, cfg, backup.New(database, cfg))

	return &apiTestEnv{db: database, mux: mux, cfg: cfg, rssSrv: rssSrv}
}

func (e *apiTestEnv) do(method, path, body string) *httptest.ResponseRecorder {
	var bodyReader *strings.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	} else {
		bodyReader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, req)
	return w
}

// ---------------------------------------------------------------------------
// POST /api/feeds
// ---------------------------------------------------------------------------

func TestAddFeed_Success(t *testing.T) {
	env := newAPITestEnv(t)
	w := env.do("POST", "/api/feeds", `{"url":"`+env.rssSrv.URL+`"}`)

	if w.Code != http.StatusCreated {
		t.Fatalf("status: want 201, got %d\nbody: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["id"] != "my-test-podcast" {
		t.Errorf("id: want %q, got %q", "my-test-podcast", resp["id"])
	}
	wantProxyPrefix := "http://proxy.local/feeds/my-test-podcast"
	if !strings.HasPrefix(resp["proxy_url"], wantProxyPrefix) {
		t.Errorf("proxy_url: want prefix %q, got %q", wantProxyPrefix, resp["proxy_url"])
	}
}

func TestAddFeed_StoresEpisodesInDB(t *testing.T) {
	env := newAPITestEnv(t)
	env.do("POST", "/api/feeds", `{"url":"`+env.rssSrv.URL+`"}`)

	eps, err := env.db.ListEpisodesByFeed("my-test-podcast")
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

func TestAddFeed_DuplicateReturns200WithMessage(t *testing.T) {
	env := newAPITestEnv(t)
	body := `{"url":"` + env.rssSrv.URL + `"}`
	env.do("POST", "/api/feeds", body) // first
	w := env.do("POST", "/api/feeds", body) // second

	if w.Code != http.StatusOK {
		t.Errorf("duplicate: want 200, got %d", w.Code)
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["message"] != "feed already exists" {
		t.Errorf("message: want %q, got %q", "feed already exists", resp["message"])
	}
	if resp["id"] != "my-test-podcast" {
		t.Errorf("id: want %q, got %q", "my-test-podcast", resp["id"])
	}
}

func TestAddFeed_BadJSON_Returns400(t *testing.T) {
	env := newAPITestEnv(t)
	w := env.do("POST", "/api/feeds", "not json")
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestAddFeed_EmptyURL_Returns400(t *testing.T) {
	env := newAPITestEnv(t)
	w := env.do("POST", "/api/feeds", `{"url":""}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestAddFeed_UnreachableURL_Returns502(t *testing.T) {
	env := newAPITestEnv(t)
	w := env.do("POST", "/api/feeds", `{"url":"http://127.0.0.1:1"}`)
	if w.Code != http.StatusBadGateway {
		t.Errorf("want 502, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// GET /api/feeds
// ---------------------------------------------------------------------------

func TestListFeeds_EmptyReturnsEmptyArray(t *testing.T) {
	env := newAPITestEnv(t)
	w := env.do("GET", "/api/feeds", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	var resp []any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 0 {
		t.Errorf("want empty array, got %d items", len(resp))
	}
}

func TestListFeeds_ReturnsAddedFeed(t *testing.T) {
	env := newAPITestEnv(t)
	env.do("POST", "/api/feeds", `{"url":"`+env.rssSrv.URL+`"}`)

	w := env.do("GET", "/api/feeds", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	var resp []map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 1 {
		t.Fatalf("want 1 feed, got %d", len(resp))
	}
	if resp[0]["id"] != "my-test-podcast" {
		t.Errorf("id: want %q, got %v", "my-test-podcast", resp[0]["id"])
	}
	if resp[0]["proxy_url"] == "" || resp[0]["proxy_url"] == nil {
		t.Error("proxy_url should not be empty")
	}
	if resp[0]["original_url"] == "" || resp[0]["original_url"] == nil {
		t.Error("original_url should not be empty")
	}
}

// ---------------------------------------------------------------------------
// DELETE /api/feeds/{id}
// ---------------------------------------------------------------------------

func TestDeleteFeed_Returns204(t *testing.T) {
	env := newAPITestEnv(t)
	env.do("POST", "/api/feeds", `{"url":"`+env.rssSrv.URL+`"}`)

	w := env.do("DELETE", "/api/feeds/my-test-podcast", "")
	if w.Code != http.StatusNoContent {
		t.Errorf("want 204, got %d", w.Code)
	}
}

func TestDeleteFeed_RemovedFromList(t *testing.T) {
	env := newAPITestEnv(t)
	env.do("POST", "/api/feeds", `{"url":"`+env.rssSrv.URL+`"}`)
	env.do("DELETE", "/api/feeds/my-test-podcast", "")

	w := env.do("GET", "/api/feeds", "")
	var resp []any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 0 {
		t.Errorf("want 0 feeds after delete, got %d", len(resp))
	}
}

func TestDeleteFeed_InProgress_Returns409(t *testing.T) {
	env := newAPITestEnv(t)
	env.do("POST", "/api/feeds", `{"url":"`+env.rssSrv.URL+`"}`)
	eps, _ := env.db.ListEpisodesByFeed("my-test-podcast")
	if len(eps) == 0 {
		t.Skip("no episodes in test feed")
	}
	env.db.UpdateEpisodeCacheStatus(eps[0].ID, "in_progress", nil, 0, "")

	w := env.do("DELETE", "/api/feeds/my-test-podcast", "")
	if w.Code != http.StatusConflict {
		t.Errorf("want 409, got %d", w.Code)
	}
}

func TestDeleteFeed_NotFound_Returns404(t *testing.T) {
	env := newAPITestEnv(t)
	w := env.do("DELETE", "/api/feeds/nonexistent", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
}

func TestDeleteFeed_RemovesArtworkFile(t *testing.T) {
	env := newAPITestEnv(t)
	env.cfg.Storage.CacheDir = t.TempDir()

	env.do("POST", "/api/feeds", `{"url":"`+env.rssSrv.URL+`"}`)

	feedsDir := filepath.Join(env.cfg.Storage.CacheDir, "feeds")
	if err := os.MkdirAll(feedsDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	artworkPath := filepath.Join(feedsDir, "my-test-podcast-artwork.jpg")
	if err := os.WriteFile(artworkPath, []byte("fake-img"), 0644); err != nil {
		t.Fatalf("write artwork: %v", err)
	}

	w := env.do("DELETE", "/api/feeds/my-test-podcast", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d", w.Code)
	}
	if _, err := os.Stat(artworkPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("artwork file should have been removed after delete, stat err: %v", err)
	}
}

// ---------------------------------------------------------------------------
// POST /api/feeds/{id}/refresh
// ---------------------------------------------------------------------------

func TestRefreshFeed_Returns200WithEpisodeCount(t *testing.T) {
	env := newAPITestEnv(t)
	env.do("POST", "/api/feeds", `{"url":"`+env.rssSrv.URL+`"}`)

	w := env.do("POST", "/api/feeds/my-test-podcast/refresh", "")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["id"] != "my-test-podcast" {
		t.Errorf("id: want %q, got %v", "my-test-podcast", resp["id"])
	}
	if int(resp["episodes_seen"].(float64)) < 1 {
		t.Errorf("episodes_seen: want >= 1, got %v", resp["episodes_seen"])
	}
}

func TestRefreshFeed_NotFound_Returns404(t *testing.T) {
	env := newAPITestEnv(t)
	w := env.do("POST", "/api/feeds/nonexistent/refresh", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
}

func TestRefreshFeed_RegeneratesRSSCacheFile(t *testing.T) {
	env := newAPITestEnv(t)
	env.cfg.Storage.CacheDir = t.TempDir()

	env.do("POST", "/api/feeds", `{"url":"`+env.rssSrv.URL+`"}`)

	w := env.do("POST", "/api/feeds/my-test-podcast/refresh", "")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}

	cachePath := filepath.Join(env.cfg.Storage.CacheDir, "feeds", "my-test-podcast.rss")
	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("cache file not written: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, `url="http://proxy.local/episodes/my-test-podcast/`) {
		t.Errorf("cache file missing proxy URL:\n%s", got)
	}
	if !strings.Contains(got, `.mp3"`) {
		t.Errorf("cache file proxy URL missing .mp3 extension:\n%s", got)
	}
	if strings.Contains(got, "cdn.example.com") {
		t.Errorf("cache file still contains original CDN URL:\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// POST /api/feeds/{id}/prefetch
// ---------------------------------------------------------------------------

func TestPrefetchFeed_NotFound_Returns404(t *testing.T) {
	env := newAPITestEnv(t)
	w := env.do("POST", "/api/feeds/nonexistent/prefetch", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
}

func TestPrefetchFeed_NilPrefetcher_Returns503(t *testing.T) {
	env := newAPITestEnv(t) // prefetcher is nil in default env
	env.do("POST", "/api/feeds", `{"url":"`+env.rssSrv.URL+`"}`)

	w := env.do("POST", "/api/feeds/my-test-podcast/prefetch", "")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503, got %d", w.Code)
	}
}

func TestPrefetchFeed_Returns202(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	rssSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(apiTestRSS))
	}))
	t.Cleanup(rssSrv.Close)

	cfg := &config.Config{
		Server:   config.ServerConfig{BaseURL: "http://proxy.local"},
		Storage:  config.StorageConfig{CacheDir: t.TempDir()},
		Defaults: config.DefaultsConfig{RefreshIntervalMinutes: 60, PrefetchConcurrency: 1},
	}

	prefetcher := feed.NewPrefetcher(database, cfg)
	// Don't Start the prefetcher — we only need to verify the HTTP response.
	t.Cleanup(prefetcher.Stop)

	mux := http.NewServeMux()
	api.RegisterRoutes(mux, database, feed.NewFetcher(cfg), prefetcher, cfg, backup.New(database, cfg))

	env := &apiTestEnv{db: database, mux: mux, cfg: cfg, rssSrv: rssSrv}
	env.do("POST", "/api/feeds", `{"url":"`+rssSrv.URL+`"}`)

	w := env.do("POST", "/api/feeds/my-test-podcast/prefetch", "")
	if w.Code != http.StatusAccepted {
		t.Errorf("want 202, got %d\nbody: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["id"] != "my-test-podcast" {
		t.Errorf("id: want %q, got %q", "my-test-podcast", resp["id"])
	}
	if resp["status"] != "queued" {
		t.Errorf("status: want %q, got %q", "queued", resp["status"])
	}
}

// ---------------------------------------------------------------------------
// POST /api/feeds/{id}/bulk-cache
// ---------------------------------------------------------------------------

func TestBulkCacheFeed_NotFound_Returns404(t *testing.T) {
	env := newAPITestEnv(t)
	w := env.do("POST", "/api/feeds/nonexistent/bulk-cache", `{"episode_ids":["abc"]}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
}

func TestBulkCacheFeed_NilPrefetcher_Returns503(t *testing.T) {
	env := newAPITestEnv(t) // prefetcher is nil in default env
	env.do("POST", "/api/feeds", `{"url":"`+env.rssSrv.URL+`"}`)

	w := env.do("POST", "/api/feeds/my-test-podcast/bulk-cache", `{"episode_ids":["abc"]}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503, got %d", w.Code)
	}
}

func TestBulkCacheFeed_BadJSON_Returns400(t *testing.T) {
	env := newAPITestEnv(t)
	env.do("POST", "/api/feeds", `{"url":"`+env.rssSrv.URL+`"}`)

	w := env.do("POST", "/api/feeds/my-test-podcast/bulk-cache", "not json")
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestBulkCacheFeed_EmptyEpisodeIDs_Returns400(t *testing.T) {
	env := newAPITestEnv(t)
	env.do("POST", "/api/feeds", `{"url":"`+env.rssSrv.URL+`"}`)

	w := env.do("POST", "/api/feeds/my-test-podcast/bulk-cache", `{"episode_ids":[]}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestBulkCacheFeed_TooManyEpisodeIDs_Returns400(t *testing.T) {
	env := newAPITestEnv(t)
	env.do("POST", "/api/feeds", `{"url":"`+env.rssSrv.URL+`"}`)

	ids := make([]string, 501)
	for i := range ids {
		ids[i] = fmt.Sprintf(`"ep%d"`, i)
	}
	body := `{"episode_ids":[` + strings.Join(ids, ",") + `]}`
	w := env.do("POST", "/api/feeds/my-test-podcast/bulk-cache", body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestBulkCacheFeed_Returns202WithCounts(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	rssSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(apiTestRSS))
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
	api.RegisterRoutes(mux, database, feed.NewFetcher(cfg), prefetcher, cfg, backup.New(database, cfg))

	env := &apiTestEnv{db: database, mux: mux, cfg: cfg, rssSrv: rssSrv}
	env.do("POST", "/api/feeds", `{"url":"`+rssSrv.URL+`"}`)

	eps, err := database.ListEpisodesByFeed("my-test-podcast")
	if err != nil || len(eps) == 0 {
		t.Fatalf("setup: ListEpisodesByFeed: %v (count=%d)", err, len(eps))
	}

	body := `{"episode_ids":["` + eps[0].URLID + `","nonexistent-urlid"]}`
	w := env.do("POST", "/api/feeds/my-test-podcast/bulk-cache", body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d\nbody: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["id"] != "my-test-podcast" {
		t.Errorf("id: want %q, got %v", "my-test-podcast", resp["id"])
	}
	if int(resp["queued"].(float64)) != 1 {
		t.Errorf("queued: want 1, got %v", resp["queued"])
	}
	if int(resp["skipped"].(float64)) != 0 {
		t.Errorf("skipped: want 0, got %v", resp["skipped"])
	}
}

// ---------------------------------------------------------------------------
// POST /api/backups  &  GET /api/backups
// ---------------------------------------------------------------------------

// newBackupAPIEnv creates a test env with a properly configured backup manager.
func newBackupAPIEnv(t *testing.T) *apiTestEnv {
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
	api.RegisterRoutes(mux, database, feed.NewFetcher(cfg), nil, cfg, backup.New(database, cfg))
	return &apiTestEnv{db: database, mux: mux, cfg: cfg}
}

func TestCreateBackup_Returns201WithInfo(t *testing.T) {
	env := newBackupAPIEnv(t)

	w := env.do("POST", "/api/backups", "")
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d\nbody: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["name"] == "" || resp["name"] == nil {
		t.Error("name should not be empty")
	}
	if sizeBytes, ok := resp["size_bytes"].(float64); !ok || sizeBytes <= 0 {
		t.Errorf("size_bytes should be > 0, got %v", resp["size_bytes"])
	}
	if resp["created_at"] == "" || resp["created_at"] == nil {
		t.Error("created_at should not be empty")
	}
}

func TestListBackups_EmptyInitially(t *testing.T) {
	env := newBackupAPIEnv(t)

	w := env.do("GET", "/api/backups", "")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}

	var resp []any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 0 {
		t.Errorf("want empty list, got %d items", len(resp))
	}
}

func TestListBackups_ReturnsCreatedBackup(t *testing.T) {
	env := newBackupAPIEnv(t)

	env.do("POST", "/api/backups", "")

	w := env.do("GET", "/api/backups", "")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}

	var resp []any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 1 {
		t.Errorf("want 1 backup, got %d", len(resp))
	}
}

func TestBulkCacheFeed_SkipsCachedAndInProgress(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	rssSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(apiTestRSS))
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
	api.RegisterRoutes(mux, database, feed.NewFetcher(cfg), prefetcher, cfg, backup.New(database, cfg))

	env := &apiTestEnv{db: database, mux: mux, cfg: cfg, rssSrv: rssSrv}
	env.do("POST", "/api/feeds", `{"url":"`+rssSrv.URL+`"}`)

	eps, err := database.ListEpisodesByFeed("my-test-podcast")
	if err != nil || len(eps) == 0 {
		t.Fatalf("setup: ListEpisodesByFeed: %v (count=%d)", err, len(eps))
	}
	cachedPath := "/some/path"
	database.UpdateEpisodeCacheStatus(eps[0].ID, "cached", &cachedPath, 1234, "audio/mpeg")

	body := `{"episode_ids":["` + eps[0].URLID + `"]}`
	w := env.do("POST", "/api/feeds/my-test-podcast/bulk-cache", body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d\nbody: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if int(resp["queued"].(float64)) != 0 {
		t.Errorf("queued: want 0, got %v", resp["queued"])
	}
	if int(resp["skipped"].(float64)) != 1 {
		t.Errorf("skipped: want 1, got %v", resp["skipped"])
	}
}

// ---------------------------------------------------------------------------
// POST /api/feeds/{id}/migrate (and /migrate/preview)
// ---------------------------------------------------------------------------

// migrationRSS returns RSS XML with the given title and GUID list, each item
// carrying a single mp3 enclosure. Used to simulate a podcast host returning
// different content from the same URL.
func migrationRSS(title string, guids ...string) string {
	var items strings.Builder
	for i, g := range guids {
		fmt.Fprintf(&items, `
    <item>
      <title>Episode %d</title>
      <guid>%s</guid>
      <enclosure url="https://cdn.example.com/%s.mp3" type="audio/mpeg" length="100"/>
    </item>`, i+1, g, g)
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel>
  <title>%s</title>
  <link>https://example.com</link>
  <description>Test</description>%s
</channel></rss>`, title, items.String())
}

// newMigrationServer returns an httptest server that serves the given RSS
// body. Used to spin up an "old host" and a "new host" within a single test.
func newMigrationServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMigrationPreview_NoChange_Returns400(t *testing.T) {
	env := newAPITestEnv(t)
	env.do("POST", "/api/feeds", `{"url":"`+env.rssSrv.URL+`"}`)

	w := env.do("POST", "/api/feeds/my-test-podcast/migrate/preview",
		`{"new_url":"`+env.rssSrv.URL+`"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for no-change, got %d\nbody: %s", w.Code, w.Body.String())
	}
}

func TestMigrationPreview_NotFound_Returns404(t *testing.T) {
	env := newAPITestEnv(t)
	w := env.do("POST", "/api/feeds/no-such-feed/migrate/preview",
		`{"new_url":"https://example.com/feed.rss"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestMigrationPreview_EmptyBody_Returns400(t *testing.T) {
	env := newAPITestEnv(t)
	env.do("POST", "/api/feeds", `{"url":"`+env.rssSrv.URL+`"}`)

	w := env.do("POST", "/api/feeds/my-test-podcast/migrate/preview", `{}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestMigrationPreview_NonHTTPScheme_Returns400(t *testing.T) {
	env := newAPITestEnv(t)
	env.do("POST", "/api/feeds", `{"url":"`+env.rssSrv.URL+`"}`)

	w := env.do("POST", "/api/feeds/my-test-podcast/migrate/preview",
		`{"new_url":"ftp://example.com/feed.rss"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for non-http(s) scheme, got %d\nbody: %s", w.Code, w.Body.String())
	}
}

func TestMigrationPreview_MalformedURL_Returns400(t *testing.T) {
	env := newAPITestEnv(t)
	env.do("POST", "/api/feeds", `{"url":"`+env.rssSrv.URL+`"}`)

	w := env.do("POST", "/api/feeds/my-test-podcast/migrate/preview",
		`{"new_url":"not-a-url"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for malformed URL, got %d\nbody: %s", w.Code, w.Body.String())
	}
}

func TestMigrationPreview_Unreachable_Returns502(t *testing.T) {
	env := newAPITestEnv(t)
	env.do("POST", "/api/feeds", `{"url":"`+env.rssSrv.URL+`"}`)

	w := env.do("POST", "/api/feeds/my-test-podcast/migrate/preview",
		`{"new_url":"http://127.0.0.1:1/feed.rss"}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d\nbody: %s", w.Code, w.Body.String())
	}
}

func TestMigrationPreview_AllGUIDsMatch_NoWarnings(t *testing.T) {
	oldSrv := newMigrationServer(t, migrationRSS("My Test Podcast", "guid-001", "guid-002"))
	newSrv := newMigrationServer(t, migrationRSS("My Test Podcast", "guid-001", "guid-002"))

	env := newAPITestEnv(t)
	env.do("POST", "/api/feeds", `{"url":"`+oldSrv.URL+`"}`)

	w := env.do("POST", "/api/feeds/my-test-podcast/migrate/preview",
		`{"new_url":"`+newSrv.URL+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	var p struct {
		MatchingGUIDs int      `json:"matching_guids"`
		NewGUIDs      int      `json:"new_guids"`
		Warnings      []string `json:"warnings"`
	}
	json.Unmarshal(w.Body.Bytes(), &p)
	if p.MatchingGUIDs != 2 {
		t.Errorf("matching_guids: want 2, got %d", p.MatchingGUIDs)
	}
	if p.NewGUIDs != 0 {
		t.Errorf("new_guids: want 0, got %d", p.NewGUIDs)
	}
	if len(p.Warnings) != 0 {
		t.Errorf("warnings: want none, got %v", p.Warnings)
	}
}

func TestMigrationPreview_ZeroGUIDOverlap_WarnsAboutDifferentPodcast(t *testing.T) {
	oldSrv := newMigrationServer(t, migrationRSS("My Test Podcast", "old-001", "old-002"))
	newSrv := newMigrationServer(t, migrationRSS("My Test Podcast", "new-001", "new-002"))

	env := newAPITestEnv(t)
	env.do("POST", "/api/feeds", `{"url":"`+oldSrv.URL+`"}`)

	w := env.do("POST", "/api/feeds/my-test-podcast/migrate/preview",
		`{"new_url":"`+newSrv.URL+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var p struct {
		MatchingGUIDs int      `json:"matching_guids"`
		Warnings      []string `json:"warnings"`
	}
	json.Unmarshal(w.Body.Bytes(), &p)
	if p.MatchingGUIDs != 0 {
		t.Errorf("matching_guids: want 0, got %d", p.MatchingGUIDs)
	}
	if len(p.Warnings) == 0 || !strings.Contains(strings.Join(p.Warnings, " "), "different podcast") {
		t.Errorf("want warning about different podcast, got %v", p.Warnings)
	}
}

func TestMigrationCommit_NoWarnings_PreservesEpisodes(t *testing.T) {
	oldSrv := newMigrationServer(t, migrationRSS("My Test Podcast", "guid-001", "guid-002"))
	newSrv := newMigrationServer(t,
		migrationRSS("My Renamed Podcast", "guid-001", "guid-002", "guid-003"))

	env := newAPITestEnv(t)
	env.do("POST", "/api/feeds", `{"url":"`+oldSrv.URL+`"}`)

	// Pre-condition: two episodes, current URL is oldSrv.URL.
	preCount, _ := env.db.CountEpisodes("my-test-podcast")
	if preCount != 2 {
		t.Fatalf("setup: want 2 episodes, got %d", preCount)
	}

	w := env.do("POST", "/api/feeds/my-test-podcast/migrate",
		`{"new_url":"`+newSrv.URL+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}

	// Feed row's original_url and title should be updated; ID unchanged.
	f, err := env.db.GetFeed("my-test-podcast")
	if err != nil {
		t.Fatalf("GetFeed: %v", err)
	}
	if f.OriginalURL != newSrv.URL {
		t.Errorf("original_url: want %q, got %q", newSrv.URL, f.OriginalURL)
	}
	if f.Title != "My Renamed Podcast" {
		t.Errorf("title: want %q, got %q", "My Renamed Podcast", f.Title)
	}

	// Existing two episodes preserved, third upserted → total 3.
	postCount, _ := env.db.CountEpisodes("my-test-podcast")
	if postCount != 3 {
		t.Errorf("episode count: want 3, got %d", postCount)
	}
}

// emptyTitleRSS produces a valid-but-titleless RSS body. Some real-world feeds
// return XML without a <title> element after a botched host migration.
const emptyTitleRSS = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel>
  <link>https://example.com</link>
  <description>Test</description>
  <item>
    <title>Episode One</title>
    <guid>guid-001</guid>
    <enclosure url="https://cdn.example.com/ep1.mp3" type="audio/mpeg" length="100"/>
  </item>
</channel></rss>`

func TestMigrationCommit_EmptyNewTitle_PreservesExistingTitle(t *testing.T) {
	oldSrv := newMigrationServer(t, migrationRSS("My Test Podcast", "guid-001"))
	newSrv := newMigrationServer(t, emptyTitleRSS)

	env := newAPITestEnv(t)
	env.do("POST", "/api/feeds", `{"url":"`+oldSrv.URL+`"}`)

	// Force-migrate (the empty new feed will also raise a GUID-overlap warning).
	w := env.do("POST", "/api/feeds/my-test-podcast/migrate",
		`{"new_url":"`+newSrv.URL+`","force":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}

	f, _ := env.db.GetFeed("my-test-podcast")
	if f.Title != "My Test Podcast" {
		t.Errorf("empty new title wiped existing title: got %q, want %q",
			f.Title, "My Test Podcast")
	}
	if f.OriginalURL != newSrv.URL {
		t.Errorf("original_url not updated: %q", f.OriginalURL)
	}
}

func TestMigrationCommit_WarningsWithoutForce_Returns409(t *testing.T) {
	oldSrv := newMigrationServer(t, migrationRSS("My Test Podcast", "old-001"))
	newSrv := newMigrationServer(t, migrationRSS("My Test Podcast", "new-001"))

	env := newAPITestEnv(t)
	env.do("POST", "/api/feeds", `{"url":"`+oldSrv.URL+`"}`)

	w := env.do("POST", "/api/feeds/my-test-podcast/migrate",
		`{"new_url":"`+newSrv.URL+`"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d\nbody: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Warnings []string `json:"warnings"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Warnings) == 0 {
		t.Errorf("expected warnings in 409 response, got none")
	}

	// Feed row should be untouched.
	f, _ := env.db.GetFeed("my-test-podcast")
	if f.OriginalURL != oldSrv.URL {
		t.Errorf("original_url changed despite 409: %q", f.OriginalURL)
	}
}

func TestMigrationCommit_WarningsWithForce_Succeeds(t *testing.T) {
	oldSrv := newMigrationServer(t, migrationRSS("My Test Podcast", "old-001"))
	newSrv := newMigrationServer(t, migrationRSS("My Test Podcast", "new-001"))

	env := newAPITestEnv(t)
	env.do("POST", "/api/feeds", `{"url":"`+oldSrv.URL+`"}`)

	w := env.do("POST", "/api/feeds/my-test-podcast/migrate",
		`{"new_url":"`+newSrv.URL+`","force":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	f, _ := env.db.GetFeed("my-test-podcast")
	if f.OriginalURL != newSrv.URL {
		t.Errorf("original_url: want %q, got %q", newSrv.URL, f.OriginalURL)
	}
}

func TestMigrationCommit_InProgressDownload_Returns409(t *testing.T) {
	oldSrv := newMigrationServer(t, migrationRSS("My Test Podcast", "guid-001"))
	newSrv := newMigrationServer(t, migrationRSS("My Test Podcast", "guid-001"))

	env := newAPITestEnv(t)
	env.do("POST", "/api/feeds", `{"url":"`+oldSrv.URL+`"}`)

	eps, _ := env.db.ListEpisodesByFeed("my-test-podcast")
	if len(eps) == 0 {
		t.Fatalf("setup: no episodes")
	}
	tmpPath := "/tmp/x"
	env.db.UpdateEpisodeCacheStatus(eps[0].ID, "in_progress", &tmpPath, 0, "audio/mpeg")

	w := env.do("POST", "/api/feeds/my-test-podcast/migrate",
		`{"new_url":"`+newSrv.URL+`"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409 (in-progress), got %d\nbody: %s", w.Code, w.Body.String())
	}
}

func TestMigrationCommit_PreservesFeedIDAndOtherColumns(t *testing.T) {
	oldSrv := newMigrationServer(t, migrationRSS("My Test Podcast", "guid-001"))
	newSrv := newMigrationServer(t, migrationRSS("Renamed", "guid-001"))

	env := newAPITestEnv(t)
	env.do("POST", "/api/feeds", `{"url":"`+oldSrv.URL+`"}`)

	// Flip auto_prefetch on so we can confirm it isn't reset by migration.
	if _, err := env.db.ToggleFeedAutoPrefetch("my-test-podcast"); err != nil {
		t.Fatalf("toggle: %v", err)
	}

	w := env.do("POST", "/api/feeds/my-test-podcast/migrate",
		`{"new_url":"`+newSrv.URL+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	f, _ := env.db.GetFeed("my-test-podcast")
	if f.ID != "my-test-podcast" {
		t.Errorf("feed ID changed: %q", f.ID)
	}
	if !f.AutoPrefetch {
		t.Errorf("auto_prefetch reset by migration")
	}
}

func TestMigrationCommit_RegeneratesRSSCacheFile(t *testing.T) {
	oldSrv := newMigrationServer(t, migrationRSS("My Test Podcast", "guid-001"))
	newSrv := newMigrationServer(t, migrationRSS("My Test Podcast", "guid-001"))

	env := newAPITestEnv(t)
	env.do("POST", "/api/feeds", `{"url":"`+oldSrv.URL+`"}`)
	w := env.do("POST", "/api/feeds/my-test-podcast/migrate",
		`{"new_url":"`+newSrv.URL+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}

	cachePath := filepath.Join(env.cfg.Storage.CacheDir, "feeds", "my-test-podcast.rss")
	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read cached rss: %v", err)
	}
	if !strings.Contains(string(data), "/episodes/my-test-podcast/") {
		t.Errorf("cached rss does not contain rewritten proxy URLs: %s", string(data))
	}
	// Sanity: file is not empty and contains the original GUID.
	if !strings.Contains(string(data), "guid-001") {
		t.Errorf("cached rss missing expected guid: %s", string(data))
	}
}
