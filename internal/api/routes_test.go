package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"podproxy/internal/api"
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
		Defaults: config.DefaultsConfig{RefreshIntervalMinutes: 60},
	}

	mux := http.NewServeMux()
	api.RegisterRoutes(mux, database, feed.NewFetcher(cfg), cfg)

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

func TestDeleteFeed_NotFound_Returns404(t *testing.T) {
	env := newAPITestEnv(t)
	w := env.do("DELETE", "/api/feeds/nonexistent", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
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
