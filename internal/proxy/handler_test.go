package proxy_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"podproxy/internal/config"
	"podproxy/internal/db"
	"podproxy/internal/feed"
	"podproxy/internal/proxy"
)

const proxyTestRSS = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Proxy Test Podcast</title>
    <link>https://example.com</link>
    <description>Test</description>
    <item>
      <title>Episode One</title>
      <guid>guid-proxy-001</guid>
      <enclosure url="https://cdn.example.com/ep1.mp3" type="audio/mpeg" length="12345"/>
    </item>
  </channel>
</rss>`

// proxyTestEnv wires up a proxy handler against a real SQLite DB and an
// in-process httptest server acting as the upstream RSS source.
type proxyTestEnv struct {
	db     *db.DB
	mux    *http.ServeMux
	cfg    *config.Config
	rssSrv *httptest.Server
}

func newProxyTestEnv(t *testing.T) *proxyTestEnv {
	t.Helper()

	rssSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(proxyTestRSS))
	}))
	t.Cleanup(rssSrv.Close)

	tmp := t.TempDir()
	database, err := db.Open(filepath.Join(tmp, "data"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	cfg := &config.Config{
		Server: config.ServerConfig{
			Port:    8080,
			BaseURL: "http://proxy.local",
		},
		Storage: config.StorageConfig{
			CacheDir: filepath.Join(tmp, "cache"),
			DataDir:  filepath.Join(tmp, "data"),
		},
		Defaults: config.DefaultsConfig{RefreshIntervalMinutes: 60},
	}

	mux := http.NewServeMux()
	proxy.RegisterRoutes(mux, database, feed.NewFetcher(cfg), cfg)

	return &proxyTestEnv{db: database, mux: mux, cfg: cfg, rssSrv: rssSrv}
}

func (e *proxyTestEnv) get(path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, req)
	return w
}

func (e *proxyTestEnv) head(path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("HEAD", path, nil)
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, req)
	return w
}

// seedFeed inserts a feed pointing at the upstream RSS test server.
func (e *proxyTestEnv) seedFeed(t *testing.T, id string) {
	t.Helper()
	f := &db.Feed{
		ID:                     id,
		Title:                  id,
		OriginalURL:            e.rssSrv.URL,
		RefreshIntervalMinutes: 60,
	}
	if err := e.db.InsertFeed(f); err != nil {
		t.Fatalf("seedFeed %q: %v", id, err)
	}
}

// ---------------------------------------------------------------------------
// GET /feeds/{id}.rss
// ---------------------------------------------------------------------------

func TestServeFeed_FetchesAndRewritesXML(t *testing.T) {
	env := newProxyTestEnv(t)
	env.seedFeed(t, "proxy-test-podcast")

	w := env.get("/feeds/proxy-test-podcast.rss", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/rss+xml") {
		t.Errorf("Content-Type: want application/rss+xml, got %q", ct)
	}
	body := w.Body.String()
	if strings.Contains(body, "cdn.example.com") {
		t.Errorf("original CDN URL should have been rewritten; body snippet:\n%.200s", body)
	}
	if !strings.Contains(body, "http://proxy.local/episodes/proxy-test-podcast/") {
		t.Errorf("proxied episode URL not found; body snippet:\n%.200s", body)
	}
}

func TestServeFeed_WritesRSSCacheToDisk(t *testing.T) {
	env := newProxyTestEnv(t)
	env.seedFeed(t, "proxy-test-podcast")

	env.get("/feeds/proxy-test-podcast.rss", nil)

	cachePath := filepath.Join(env.cfg.Storage.CacheDir, "feeds", "proxy-test-podcast.rss")
	if _, err := os.Stat(cachePath); os.IsNotExist(err) {
		t.Errorf("cache file not written to %s", cachePath)
	}
}

func TestServeFeed_ServesCachedFileWhenFresh(t *testing.T) {
	env := newProxyTestEnv(t)
	env.seedFeed(t, "cached-pod")

	// Mark as recently fetched.
	now := time.Now()
	if err := env.db.UpdateFeedFetchedAt("cached-pod", now); err != nil {
		t.Fatalf("UpdateFeedFetchedAt: %v", err)
	}

	// Write a sentinel cache file.
	cacheDir := filepath.Join(env.cfg.Storage.CacheDir, "feeds")
	os.MkdirAll(cacheDir, 0755)
	sentinel := []byte("<rss>CACHED SENTINEL</rss>")
	os.WriteFile(filepath.Join(cacheDir, "cached-pod.rss"), sentinel, 0644)

	w := env.get("/feeds/cached-pod.rss", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "CACHED SENTINEL") {
		t.Errorf("expected cached content; got: %s", w.Body.String())
	}
}

func TestServeFeed_NoRssSuffix_Returns404(t *testing.T) {
	env := newProxyTestEnv(t)
	w := env.get("/feeds/some-podcast", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
}

func TestServeFeed_UnknownFeed_Returns404(t *testing.T) {
	env := newProxyTestEnv(t)
	w := env.get("/feeds/nonexistent.rss", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// GET /episodes/{feed_id}/{ep_id}
// ---------------------------------------------------------------------------

func seedEpisode(t *testing.T, d *db.DB, feedID, urlID, originURL string) {
	t.Helper()
	ep := &db.Episode{
		ID:          feedID + "/ep-" + urlID,
		FeedID:      feedID,
		Title:       "Test Episode",
		OriginalURL: originURL,
		CacheStatus: "none",
		URLID:       urlID,
	}
	if err := d.UpsertEpisode(ep); err != nil {
		t.Fatalf("seedEpisode: %v", err)
	}
}

func TestServeEpisode_ProxiesToOrigin(t *testing.T) {
	const audioContent = "fake-audio-bytes-1234"
	originSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write([]byte(audioContent))
	}))
	defer originSrv.Close()

	env := newProxyTestEnv(t)
	env.db.InsertFeed(&db.Feed{ID: "pod", Title: "Pod", OriginalURL: "https://x.com", RefreshIntervalMinutes: 60})
	seedEpisode(t, env.db, "pod", "episodeid01", originSrv.URL+"/ep1.mp3")

	w := env.get("/episodes/pod/episodeid01", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "audio/mpeg" {
		t.Errorf("Content-Type: want audio/mpeg, got %q", ct)
	}
	if w.Body.String() != audioContent {
		t.Errorf("body: want %q, got %q", audioContent, w.Body.String())
	}
}

func TestServeEpisode_ForwardsRangeHeader(t *testing.T) {
	var receivedRange string
	originSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedRange = r.Header.Get("Range")
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Content-Range", "bytes 0-1/1000")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte("fa"))
	}))
	defer originSrv.Close()

	env := newProxyTestEnv(t)
	env.db.InsertFeed(&db.Feed{ID: "pod2", Title: "Pod2", OriginalURL: "https://x.com", RefreshIntervalMinutes: 60})
	seedEpisode(t, env.db, "pod2", "rangeid0001", originSrv.URL+"/ep1.mp3")

	w := env.get("/episodes/pod2/rangeid0001", map[string]string{"Range": "bytes=0-1"})

	if w.Code != http.StatusPartialContent {
		t.Errorf("status: want 206, got %d", w.Code)
	}
	if receivedRange != "bytes=0-1" {
		t.Errorf("Range header forwarded to origin: want %q, got %q", "bytes=0-1", receivedRange)
	}
	if cr := w.Header().Get("Content-Range"); cr != "bytes 0-1/1000" {
		t.Errorf("Content-Range: want %q, got %q", "bytes 0-1/1000", cr)
	}
}

func TestServeEpisode_ProxiesResponseHeaders(t *testing.T) {
	originSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Content-Length", "100")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", `"abc123"`)
		w.Write(make([]byte, 100))
	}))
	defer originSrv.Close()

	env := newProxyTestEnv(t)
	env.db.InsertFeed(&db.Feed{ID: "pod3", Title: "Pod3", OriginalURL: "https://x.com", RefreshIntervalMinutes: 60})
	seedEpisode(t, env.db, "pod3", "hdrtest0001", originSrv.URL+"/ep1.mp3")

	w := env.get("/episodes/pod3/hdrtest0001", nil)

	if w.Header().Get("Accept-Ranges") != "bytes" {
		t.Errorf("Accept-Ranges: want bytes, got %q", w.Header().Get("Accept-Ranges"))
	}
	if w.Header().Get("ETag") != `"abc123"` {
		t.Errorf("ETag: want %q, got %q", `"abc123"`, w.Header().Get("ETag"))
	}
}

func TestServeEpisode_HEAD_ReturnsHeadersNoBody(t *testing.T) {
	originSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Content-Length", "500")
		if r.Method != http.MethodHead {
			w.Write(make([]byte, 500))
		}
	}))
	defer originSrv.Close()

	env := newProxyTestEnv(t)
	env.db.InsertFeed(&db.Feed{ID: "pod4", Title: "Pod4", OriginalURL: "https://x.com", RefreshIntervalMinutes: 60})
	seedEpisode(t, env.db, "pod4", "headid00001", originSrv.URL+"/ep1.mp3")

	w := env.head("/episodes/pod4/headid00001")

	if w.Code != http.StatusOK {
		t.Errorf("status: want 200, got %d", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD: body must be empty, got %d bytes", w.Body.Len())
	}
}

func TestServeEpisode_UnknownEpisode_Returns404(t *testing.T) {
	env := newProxyTestEnv(t)
	env.db.InsertFeed(&db.Feed{ID: "pod5", Title: "Pod5", OriginalURL: "https://x.com", RefreshIntervalMinutes: 60})

	w := env.get("/episodes/pod5/doesnotexist", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
}
