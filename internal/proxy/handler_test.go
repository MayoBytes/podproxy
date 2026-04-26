package proxy_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	proxy.RegisterRoutes(mux, database, feed.NewFetcher(cfg), feed.NewPrefetcher(database, cfg), cfg)

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

	// Write the sentinel cache file first, then mark as fetched at a time
	// strictly in the past so the file's mtime is clearly >= LastFetchedAt.
	// (The handler rejects cache files whose mtime predates LastFetchedAt, to
	// force re-generation when the rewrite logic changes — e.g. itunes:block.)
	cacheDir := filepath.Join(env.cfg.Storage.CacheDir, "feeds")
	os.MkdirAll(cacheDir, 0755)
	sentinel := []byte("<rss>CACHED SENTINEL</rss>")
	os.WriteFile(filepath.Join(cacheDir, "cached-pod.rss"), sentinel, 0644)

	past := time.Now().Add(-time.Second)
	if err := env.db.UpdateFeedFetchedAt("cached-pod", past); err != nil {
		t.Fatalf("UpdateFeedFetchedAt: %v", err)
	}

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
// GET /artwork/{id}
// ---------------------------------------------------------------------------

func TestServeArtwork_NoArtwork_Returns404(t *testing.T) {
	env := newProxyTestEnv(t)
	env.seedFeed(t, "some-podcast")

	w := env.get("/artwork/some-podcast", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
}

func TestServeArtwork_ServesFileWithCorrectContentType(t *testing.T) {
	env := newProxyTestEnv(t)
	env.seedFeed(t, "art-podcast")

	artworkDir := filepath.Join(env.cfg.Storage.CacheDir, "feeds")
	if err := os.MkdirAll(artworkDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	imgData := []byte("fake-png-data")
	if err := os.WriteFile(filepath.Join(artworkDir, "art-podcast-artwork.png"), imgData, 0644); err != nil {
		t.Fatalf("write artwork: %v", err)
	}

	w := env.get("/artwork/art-podcast", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", w.Code, w.Body.String())
	}
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "image/png") {
		t.Errorf("Content-Type: want image/png, got %q", ct)
	}
	if w.Body.String() != string(imgData) {
		t.Errorf("body mismatch: got %q", w.Body.String())
	}
}

func TestServeArtwork_InvalidFeedID_Returns404(t *testing.T) {
	env := newProxyTestEnv(t)
	// Characters outside [a-z0-9-] are rejected to prevent glob injection.
	for _, id := range []string{"UPPER", "has.dot", "has_underscore", "has!bang"} {
		w := env.get("/artwork/"+id, nil)
		if w.Code != http.StatusNotFound {
			t.Errorf("feedID %q: want 404, got %d", id, w.Code)
		}
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

// ---------------------------------------------------------------------------
// Phase 2: write-through caching
// ---------------------------------------------------------------------------

func TestServeEpisode_WritesToCacheOnNormalGet(t *testing.T) {
	const audioContent = "cached-audio-data-phase2"
	originSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write([]byte(audioContent))
	}))
	t.Cleanup(originSrv.Close)

	env := newProxyTestEnv(t)
	env.db.InsertFeed(&db.Feed{ID: "pod6", Title: "Pod6", OriginalURL: "https://x.com", RefreshIntervalMinutes: 60})
	seedEpisode(t, env.db, "pod6", "writetest001", originSrv.URL+"/ep1.mp3")

	w := env.get("/episodes/pod6/writetest001", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != audioContent {
		t.Errorf("body: want %q, got %q", audioContent, w.Body.String())
	}

	// Verify the episode is now marked as cached in the DB.
	ep, err := env.db.GetEpisodeByURLID("pod6", "writetest001")
	if err != nil {
		t.Fatalf("GetEpisodeByURLID: %v", err)
	}
	if ep.CacheStatus != "cached" {
		t.Errorf("cache_status: want %q, got %q", "cached", ep.CacheStatus)
	}
	if ep.CachedPath == "" {
		t.Error("cached_path should be set after write-through")
	}

	// Verify the file exists on disk.
	if _, err := os.Stat(ep.CachedPath); os.IsNotExist(err) {
		t.Errorf("cache file not found at %s", ep.CachedPath)
	}
}

func TestServeEpisode_ServesCachedFile(t *testing.T) {
	const cachedContent = "content-from-cache-not-origin"
	// The origin server may receive a fire-and-forget analytics ping from
	// reportUpstreamPlay, so we no longer assert it is never called.
	originSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusPartialContent)
	}))
	t.Cleanup(originSrv.Close)

	env := newProxyTestEnv(t)
	env.db.InsertFeed(&db.Feed{ID: "pod7", Title: "Pod7", OriginalURL: "https://x.com", RefreshIntervalMinutes: 60})
	seedEpisode(t, env.db, "pod7", "cachedhit01", originSrv.URL+"/ep1.mp3")

	// Write a sentinel file to the cache directory and mark the episode cached.
	cacheDir := filepath.Join(env.cfg.Storage.CacheDir, "episodes", "pod7")
	os.MkdirAll(cacheDir, 0755)
	cachePath := filepath.Join(cacheDir, "test-episode-cachedhit01.mp3")
	os.WriteFile(cachePath, []byte(cachedContent), 0644)

	epID := "pod7/ep-cachedhit01"
	if err := env.db.UpdateEpisodeCacheStatus(epID, "cached", &cachePath, int64(len(cachedContent)), "audio/mpeg"); err != nil {
		t.Fatalf("UpdateEpisodeCacheStatus: %v", err)
	}

	w := env.get("/episodes/pod7/cachedhit01", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != cachedContent {
		t.Errorf("body: want cached content, got %q", w.Body.String())
	}
}

func TestServeEpisode_CachedFile_ServesByRange(t *testing.T) {
	const fullContent = "0123456789abcdef"

	env := newProxyTestEnv(t)
	env.db.InsertFeed(&db.Feed{ID: "pod8", Title: "Pod8", OriginalURL: "https://x.com", RefreshIntervalMinutes: 60})
	seedEpisode(t, env.db, "pod8", "rangehit001", "http://unused.invalid/ep.mp3")

	cacheDir := filepath.Join(env.cfg.Storage.CacheDir, "episodes", "pod8")
	os.MkdirAll(cacheDir, 0755)
	cachePath := filepath.Join(cacheDir, "test-episode-rangehit001.mp3")
	os.WriteFile(cachePath, []byte(fullContent), 0644)

	epID := "pod8/ep-rangehit001"
	if err := env.db.UpdateEpisodeCacheStatus(epID, "cached", &cachePath, int64(len(fullContent)), "audio/mpeg"); err != nil {
		t.Fatalf("UpdateEpisodeCacheStatus: %v", err)
	}

	w := env.get("/episodes/pod8/rangehit001", map[string]string{"Range": "bytes=0-3"})

	if w.Code != http.StatusPartialContent {
		t.Errorf("status: want 206, got %d", w.Code)
	}
	if w.Body.String() != "0123" {
		t.Errorf("body: want %q, got %q", "0123", w.Body.String())
	}
}

func TestServeEpisode_RangeRequest_TriggersBackgroundFetch(t *testing.T) {
	const fullContent = "full-audio-content-abcdef"
	originSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		if r.Header.Get("Range") != "" {
			w.Header().Set("Content-Range", "bytes 0-3/"+strconv.Itoa(len(fullContent)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write([]byte(fullContent[:4]))
		} else {
			w.Write([]byte(fullContent))
		}
	}))
	t.Cleanup(originSrv.Close)

	env := newProxyTestEnv(t)
	env.db.InsertFeed(&db.Feed{ID: "pod9", Title: "Pod9", OriginalURL: "https://x.com", RefreshIntervalMinutes: 60})
	seedEpisode(t, env.db, "pod9", "bgfetch0001", originSrv.URL+"/ep1.mp3")

	w := env.get("/episodes/pod9/bgfetch0001", map[string]string{"Range": "bytes=0-3"})

	if w.Code != http.StatusPartialContent {
		t.Errorf("status: want 206, got %d", w.Code)
	}

	// Poll for the background goroutine to finish caching the full episode.
	deadline := time.Now().Add(5 * time.Second)
	var ep *db.Episode
	for time.Now().Before(deadline) {
		ep, _ = env.db.GetEpisodeByURLID("pod9", "bgfetch0001")
		if ep != nil && ep.CacheStatus == "cached" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ep == nil || ep.CacheStatus != "cached" {
		t.Errorf("background fetch did not cache episode within timeout; cache_status=%q", func() string {
			if ep != nil {
				return ep.CacheStatus
			}
			return "<nil>"
		}())
	}
}

func TestServeEpisode_CachedFileMissing_ResetsAndProxies(t *testing.T) {
	const originContent = "proxied-after-cache-miss"
	originSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write([]byte(originContent))
	}))
	t.Cleanup(originSrv.Close)

	env := newProxyTestEnv(t)
	env.db.InsertFeed(&db.Feed{ID: "pod10", Title: "Pod10", OriginalURL: "https://x.com", RefreshIntervalMinutes: 60})
	seedEpisode(t, env.db, "pod10", "staleache01", originSrv.URL+"/ep1.mp3")

	// Mark episode as cached but point to a non-existent file.
	nonExistentPath := filepath.Join(env.cfg.Storage.CacheDir, "episodes", "pod10", "gone.mp3")
	epID := "pod10/ep-staleache01"
	if err := env.db.UpdateEpisodeCacheStatus(epID, "cached", &nonExistentPath, 1000, "audio/mpeg"); err != nil {
		t.Fatalf("UpdateEpisodeCacheStatus: %v", err)
	}

	w := env.get("/episodes/pod10/staleache01", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != originContent {
		t.Errorf("body: want proxied content, got %q", w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Client disconnect during write-through caching
// ---------------------------------------------------------------------------

// failingResponseWriter simulates a client that closes the connection
// mid-stream: Write always returns an error, as if the socket is broken.
type failingResponseWriter struct {
	header http.Header
}

func (f *failingResponseWriter) Header() http.Header        { return f.header }
func (f *failingResponseWriter) WriteHeader(int)            {}
func (f *failingResponseWriter) Write([]byte) (int, error)  { return 0, errors.New("broken pipe") }

// TestServeEpisode_ClientDisconnect_ResetsStatusToNone checks that when a
// client closes the connection during a write-through download, the episode
// status is reset to "none" (retryable) rather than "failed" (permanent).
// This guards against the timing race between a Write error on the response
// writer and r.Context().Err() propagating from the HTTP server.
func TestServeEpisode_ClientDisconnect_ResetsStatusToNone(t *testing.T) {
	originSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write([]byte("audio-bytes"))
	}))
	t.Cleanup(originSrv.Close)

	env := newProxyTestEnv(t)
	env.db.InsertFeed(&db.Feed{ID: "pod-dc", Title: "PodDC", OriginalURL: "https://x.com", RefreshIntervalMinutes: 60})
	seedEpisode(t, env.db, "pod-dc", "disconnectep1", originSrv.URL+"/ep1.mp3")

	req := httptest.NewRequest("GET", "/episodes/pod-dc/disconnectep1", nil)
	env.mux.ServeHTTP(&failingResponseWriter{header: make(http.Header)}, req)

	ep, err := env.db.GetEpisodeByURLID("pod-dc", "disconnectep1")
	if err != nil {
		t.Fatalf("GetEpisodeByURLID: %v", err)
	}
	if ep.CacheStatus != "none" {
		t.Errorf("cache_status: want %q after client disconnect, got %q", "none", ep.CacheStatus)
	}
}

// TestServeEpisode_ConcurrentClientDisconnects_AllResetToNone reproduces the
// original bug: three episodes downloaded in parallel all end up "failed"
// instead of "none" when the clients disconnect. Each episode must be
// independently retryable after all three requests complete.
func TestServeEpisode_ConcurrentClientDisconnects_AllResetToNone(t *testing.T) {
	const n = 3
	originSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write([]byte("audio-bytes"))
	}))
	t.Cleanup(originSrv.Close)

	env := newProxyTestEnv(t)
	env.db.InsertFeed(&db.Feed{ID: "pod-conc", Title: "PodConc", OriginalURL: "https://x.com", RefreshIntervalMinutes: 60})

	urlIDs := [n]string{"concep0001", "concep0002", "concep0003"}
	for _, id := range urlIDs {
		seedEpisode(t, env.db, "pod-conc", id, originSrv.URL+"/"+id+".mp3")
	}

	var wg sync.WaitGroup
	for _, id := range urlIDs {
		wg.Add(1)
		go func(urlID string) {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/episodes/pod-conc/"+urlID, nil)
			env.mux.ServeHTTP(&failingResponseWriter{header: make(http.Header)}, req)
		}(id)
	}
	wg.Wait()

	for _, id := range urlIDs {
		ep, err := env.db.GetEpisodeByURLID("pod-conc", id)
		if err != nil {
			t.Fatalf("GetEpisodeByURLID(%q): %v", id, err)
		}
		if ep.CacheStatus != "none" {
			t.Errorf("episode %q: cache_status want %q, got %q", id, "none", ep.CacheStatus)
		}
	}
}

// ---------------------------------------------------------------------------
// Upstream play reporting (analytics)
// ---------------------------------------------------------------------------

// TestServeCachedEpisode_ReportsUpstreamPlay verifies that serving a cached
// episode fires a fire-and-forget GET bytes=0-1 to the original URL so that
// the podcast host's analytics register the listen. It also checks that the
// client's User-Agent is forwarded.
func TestServeCachedEpisode_ReportsUpstreamPlay(t *testing.T) {
	type capturedReq struct {
		rangeHeader string
		userAgent   string
	}
	reqCh := make(chan capturedReq, 1)

	originSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case reqCh <- capturedReq{
			rangeHeader: r.Header.Get("Range"),
			userAgent:   r.Header.Get("User-Agent"),
		}:
		default:
		}
		w.WriteHeader(http.StatusPartialContent)
	}))
	t.Cleanup(originSrv.Close)

	env := newProxyTestEnv(t)
	env.db.InsertFeed(&db.Feed{ID: "pod-rpt", Title: "PodRpt", OriginalURL: "https://x.com", RefreshIntervalMinutes: 60})
	seedEpisode(t, env.db, "pod-rpt", "reportep001", originSrv.URL+"/ep.mp3")

	cacheDir := filepath.Join(env.cfg.Storage.CacheDir, "episodes", "pod-rpt")
	os.MkdirAll(cacheDir, 0755)
	cachePath := filepath.Join(cacheDir, "test-episode-reportep001.mp3")
	os.WriteFile(cachePath, []byte("cached-audio"), 0644)
	epID := "pod-rpt/ep-reportep001"
	if err := env.db.UpdateEpisodeCacheStatus(epID, "cached", &cachePath, 12, "audio/mpeg"); err != nil {
		t.Fatalf("UpdateEpisodeCacheStatus: %v", err)
	}

	w := env.get("/episodes/pod-rpt/reportep001", map[string]string{"User-Agent": "Overcast/1234"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}

	select {
	case got := <-reqCh:
		if got.rangeHeader != "bytes=0-1" {
			t.Errorf("upstream report Range: want %q, got %q", "bytes=0-1", got.rangeHeader)
		}
		if got.userAgent != "Overcast/1234" {
			t.Errorf("upstream report User-Agent: want %q, got %q", "Overcast/1234", got.userAgent)
		}
	case <-time.After(2 * time.Second):
		t.Error("upstream play report goroutine did not fire within timeout")
	}
}

// TestServeCachedEpisode_NoReportForMidSeek verifies that Range requests with
// a non-zero start offset (mid-episode seeks) do NOT trigger an upstream play
// report, preventing over-counting of listens.
func TestServeCachedEpisode_NoReportForMidSeek(t *testing.T) {
	called := make(chan struct{}, 1)
	originSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case called <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusPartialContent)
	}))
	t.Cleanup(originSrv.Close)

	env := newProxyTestEnv(t)
	env.db.InsertFeed(&db.Feed{ID: "pod-seek", Title: "PodSeek", OriginalURL: "https://x.com", RefreshIntervalMinutes: 60})
	seedEpisode(t, env.db, "pod-seek", "seekep00001", originSrv.URL+"/ep.mp3")

	cacheDir := filepath.Join(env.cfg.Storage.CacheDir, "episodes", "pod-seek")
	os.MkdirAll(cacheDir, 0755)
	cachePath := filepath.Join(cacheDir, "test-episode-seekep00001.mp3")
	os.WriteFile(cachePath, []byte("0123456789abcdef"), 0644)
	epID := "pod-seek/ep-seekep00001"
	if err := env.db.UpdateEpisodeCacheStatus(epID, "cached", &cachePath, 16, "audio/mpeg"); err != nil {
		t.Fatalf("UpdateEpisodeCacheStatus: %v", err)
	}

	env.get("/episodes/pod-seek/seekep00001", map[string]string{"Range": "bytes=500-999"})

	// Give any hypothetical goroutine a moment to fire, then assert silence.
	select {
	case <-called:
		t.Error("upstream play report should not fire for a mid-episode seek")
	case <-time.After(100 * time.Millisecond):
		// Expected: no upstream call.
	}
}
