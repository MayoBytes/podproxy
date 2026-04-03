package feed

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"podproxy/internal/config"
	"podproxy/internal/db"
)

const pollerTestRSS = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Poller Test Podcast</title>
    <link>https://example.com</link>
    <description>Test</description>
    <item>
      <title>Episode One</title>
      <guid>poller-guid-001</guid>
      <enclosure url="https://cdn.example.com/ep1.mp3" type="audio/mpeg" length="12345"/>
    </item>
  </channel>
</rss>`

func newPollerTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func newPollerTestFetcher() *Fetcher {
	return NewFetcher(&config.Config{
		Server:   config.ServerConfig{BaseURL: "http://proxy.local"},
		Defaults: config.DefaultsConfig{RefreshIntervalMinutes: 60},
	})
}

// TestPoller_RefreshStaleFeeds_FetchesNilLastFetched verifies that a feed with
// no prior fetch time is refreshed: episodes are upserted and LastFetchedAt is set.
func TestPoller_RefreshStaleFeeds_FetchesNilLastFetched(t *testing.T) {
	rssSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(pollerTestRSS))
	}))
	t.Cleanup(rssSrv.Close)

	d := newPollerTestDB(t)
	f := &db.Feed{
		ID:                     "poller-pod",
		Title:                  "Poller Pod",
		OriginalURL:            rssSrv.URL,
		RefreshIntervalMinutes: 60,
	}
	if err := d.InsertFeed(f); err != nil {
		t.Fatalf("InsertFeed: %v", err)
	}

	p := NewPoller(d, newPollerTestFetcher())
	p.refreshStaleFeeds()
	p.wg.Wait()

	got, err := d.GetFeed("poller-pod")
	if err != nil {
		t.Fatalf("GetFeed: %v", err)
	}
	if got.LastFetchedAt == nil {
		t.Error("LastFetchedAt should be set after refresh, got nil")
	}

	eps, err := d.ListEpisodesByFeed("poller-pod")
	if err != nil {
		t.Fatalf("ListEpisodesByFeed: %v", err)
	}
	if len(eps) != 1 {
		t.Errorf("want 1 episode after refresh, got %d", len(eps))
	}
}

// TestPoller_RefreshStaleFeeds_SkipsFreshFeed verifies that a feed fetched
// within its refresh interval is not re-fetched.
func TestPoller_RefreshStaleFeeds_SkipsFreshFeed(t *testing.T) {
	callCount := 0
	rssSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(pollerTestRSS))
	}))
	t.Cleanup(rssSrv.Close)

	d := newPollerTestDB(t)
	f := &db.Feed{
		ID:                     "fresh-pod",
		Title:                  "Fresh Pod",
		OriginalURL:            rssSrv.URL,
		RefreshIntervalMinutes: 60,
	}
	if err := d.InsertFeed(f); err != nil {
		t.Fatalf("InsertFeed: %v", err)
	}
	// Mark as very recently fetched.
	if err := d.UpdateFeedFetchedAt("fresh-pod", time.Now()); err != nil {
		t.Fatalf("UpdateFeedFetchedAt: %v", err)
	}

	p := NewPoller(d, newPollerTestFetcher())
	p.refreshStaleFeeds()
	p.wg.Wait()

	if callCount != 0 {
		t.Errorf("fresh feed should not be re-fetched; upstream called %d time(s)", callCount)
	}
}

// TestPoller_RefreshStaleFeeds_FetchesExpiredFeed verifies that a feed whose
// LastFetchedAt is older than its refresh interval is re-fetched.
func TestPoller_RefreshStaleFeeds_FetchesExpiredFeed(t *testing.T) {
	callCount := 0
	rssSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(pollerTestRSS))
	}))
	t.Cleanup(rssSrv.Close)

	d := newPollerTestDB(t)
	f := &db.Feed{
		ID:                     "expired-pod",
		Title:                  "Expired Pod",
		OriginalURL:            rssSrv.URL,
		RefreshIntervalMinutes: 1, // 1-minute interval
	}
	if err := d.InsertFeed(f); err != nil {
		t.Fatalf("InsertFeed: %v", err)
	}
	// Mark as fetched 2 minutes ago — past the 1-minute interval.
	stale := time.Now().Add(-2 * time.Minute)
	if err := d.UpdateFeedFetchedAt("expired-pod", stale); err != nil {
		t.Fatalf("UpdateFeedFetchedAt: %v", err)
	}

	p := NewPoller(d, newPollerTestFetcher())
	p.refreshStaleFeeds()
	p.wg.Wait()

	if callCount != 1 {
		t.Errorf("expired feed should be re-fetched once; upstream called %d time(s)", callCount)
	}
}

// TestPoller_StartStop verifies that Start and Stop do not deadlock or panic.
func TestPoller_StartStop(t *testing.T) {
	d := newPollerTestDB(t)
	p := NewPoller(d, newPollerTestFetcher())
	p.Start()
	p.Stop() // must return promptly
}
