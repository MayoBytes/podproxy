package feed

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"podproxy/internal/config"
	"podproxy/internal/db"
)

func newPrefetcherTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func newPrefetcherTestConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Storage:  config.StorageConfig{CacheDir: t.TempDir()},
		Defaults: config.DefaultsConfig{PrefetchConcurrency: 1, PrefetchMaxAgeDays: 0},
	}
}

func seedPrefetcherFeed(t *testing.T, d *db.DB) {
	t.Helper()
	if err := d.InsertFeed(&db.Feed{ID: "pod", Title: "Pod", OriginalURL: "https://example.com"}); err != nil {
		t.Fatalf("InsertFeed: %v", err)
	}
}

// TestPrefetcher_Enqueue_ReturnsFalseWhenFull verifies that Enqueue returns
// false once the internal queue channel is at capacity.
func TestPrefetcher_Enqueue_ReturnsFalseWhenFull(t *testing.T) {
	d := newPrefetcherTestDB(t)
	cfg := newPrefetcherTestConfig(t)
	p := NewPrefetcher(d, cfg)
	t.Cleanup(p.Stop)

	ep := &db.Episode{ID: "pod/ep1", FeedID: "pod", URLID: "uid1", OriginalURL: "https://example.com/ep.mp3", CacheStatus: "none"}

	for i := 0; i < prefetchQueueSize; i++ {
		p.Enqueue(ep)
	}
	if p.Enqueue(ep) {
		t.Error("Enqueue should return false when queue is full")
	}
}

// TestPrefetcher_EnqueueFeedEpisodes_SkipsCachedAndInProgress verifies that
// episodes with cache_status "cached" or "in_progress" are not re-enqueued.
func TestPrefetcher_EnqueueFeedEpisodes_SkipsCachedAndInProgress(t *testing.T) {
	d := newPrefetcherTestDB(t)
	cfg := newPrefetcherTestConfig(t)
	p := NewPrefetcher(d, cfg)
	t.Cleanup(p.Stop)

	seedPrefetcherFeed(t, d)
	d.UpsertEpisode(&db.Episode{ID: "pod/ep1", FeedID: "pod", Title: "Ep1", OriginalURL: "https://example.com/ep1.mp3", CacheStatus: "none", URLID: "uid1"})
	d.UpsertEpisode(&db.Episode{ID: "pod/ep2", FeedID: "pod", Title: "Ep2", OriginalURL: "https://example.com/ep2.mp3", CacheStatus: "none", URLID: "uid2"})

	cachedPath := "/some/path"
	d.UpdateEpisodeCacheStatus("pod/ep1", "cached", &cachedPath, 1000, "audio/mpeg")
	d.UpdateEpisodeCacheStatus("pod/ep2", "in_progress", nil, 0, "")

	p.EnqueueFeedEpisodes("pod")

	select {
	case <-p.queue:
		t.Error("no episodes should be enqueued: both are cached/in_progress")
	default:
		// correct
	}
}

// TestPrefetcher_EnqueueFeedEpisodes_SkipsOlderThanMaxAge verifies that
// episodes whose pub_date precedes the prefetch_max_age_days cutoff are skipped.
func TestPrefetcher_EnqueueFeedEpisodes_SkipsOlderThanMaxAge(t *testing.T) {
	d := newPrefetcherTestDB(t)
	cfg := newPrefetcherTestConfig(t)
	cfg.Defaults.PrefetchMaxAgeDays = 7
	p := NewPrefetcher(d, cfg)
	t.Cleanup(p.Stop)

	seedPrefetcherFeed(t, d)
	old := time.Now().AddDate(0, 0, -30) // 30 days ago, past 7-day cutoff
	d.UpsertEpisode(&db.Episode{
		ID: "pod/ep1", FeedID: "pod", Title: "Old Ep",
		OriginalURL: "https://example.com/ep1.mp3",
		CacheStatus: "none", URLID: "uid1", PubDate: &old,
	})

	p.EnqueueFeedEpisodes("pod")

	select {
	case <-p.queue:
		t.Error("episode older than max age should not be enqueued")
	default:
		// correct
	}
}

// TestPrefetcher_EnqueueFeedEpisodes_IncludesRecentEpisodes verifies that
// uncached episodes within the max-age window are enqueued.
func TestPrefetcher_EnqueueFeedEpisodes_IncludesRecentEpisodes(t *testing.T) {
	d := newPrefetcherTestDB(t)
	cfg := newPrefetcherTestConfig(t)
	cfg.Defaults.PrefetchMaxAgeDays = 30
	p := NewPrefetcher(d, cfg)
	t.Cleanup(p.Stop)

	seedPrefetcherFeed(t, d)
	recent := time.Now().AddDate(0, 0, -1) // yesterday
	d.UpsertEpisode(&db.Episode{
		ID: "pod/ep1", FeedID: "pod", Title: "Recent Ep",
		OriginalURL: "https://example.com/ep1.mp3",
		CacheStatus: "none", URLID: "uid1", PubDate: &recent,
	})

	p.EnqueueFeedEpisodes("pod")

	select {
	case ep := <-p.queue:
		if ep.URLID != "uid1" {
			t.Errorf("expected uid1, got %s", ep.URLID)
		}
	default:
		t.Error("recent uncached episode should have been enqueued")
	}
}

// TestPrefetcher_EnqueueFeedEpisodes_ZeroMaxAge_IncludesAll verifies that
// PrefetchMaxAgeDays=0 disables the age filter entirely.
func TestPrefetcher_EnqueueFeedEpisodes_ZeroMaxAge_IncludesAll(t *testing.T) {
	d := newPrefetcherTestDB(t)
	cfg := newPrefetcherTestConfig(t) // PrefetchMaxAgeDays=0
	p := NewPrefetcher(d, cfg)
	t.Cleanup(p.Stop)

	seedPrefetcherFeed(t, d)
	ancient := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	d.UpsertEpisode(&db.Episode{
		ID: "pod/ep1", FeedID: "pod", Title: "Ancient Ep",
		OriginalURL: "https://example.com/ep1.mp3",
		CacheStatus: "none", URLID: "uid1", PubDate: &ancient,
	})

	p.EnqueueFeedEpisodes("pod")

	select {
	case <-p.queue:
		// correct: age filter is off
	default:
		t.Error("with PrefetchMaxAgeDays=0, all uncached episodes should be enqueued regardless of age")
	}
}

// TestPrefetcher_Download_CachesEpisode runs the worker pool end-to-end:
// a real HTTP server serves episode audio, the prefetcher downloads it, and
// the episode is marked "cached" in the DB with the correct metadata.
func TestPrefetcher_Download_CachesEpisode(t *testing.T) {
	const body = "fake audio data for testing"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	d := newPrefetcherTestDB(t)
	cfg := newPrefetcherTestConfig(t)
	seedPrefetcherFeed(t, d)
	d.UpsertEpisode(&db.Episode{
		ID: "pod/ep1", FeedID: "pod", Title: "Episode One",
		OriginalURL: srv.URL + "/ep1.mp3",
		CacheStatus: "none", URLID: "uid1",
	})

	p := NewPrefetcher(d, cfg)
	p.Start()
	t.Cleanup(p.Stop)

	p.EnqueueFeedEpisodes("pod")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ep, err := d.GetEpisodeByURLID("pod", "uid1")
		if err == nil && ep.CacheStatus == "cached" {
			if ep.CachedPath == "" {
				t.Error("cached_path should not be empty after caching")
			}
			if ep.SizeBytes != int64(len(body)) {
				t.Errorf("size_bytes: want %d, got %d", len(body), ep.SizeBytes)
			}
			if ep.ContentType != "audio/mpeg" {
				t.Errorf("content_type: want %q, got %q", "audio/mpeg", ep.ContentType)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("episode was not cached within 5 seconds")
}

// TestPrefetcher_Download_RetriesOnFailure verifies that a transient origin
// error triggers a retry: first request returns 500, second returns 200 and
// the episode is cached.
func TestPrefetcher_Download_RetriesOnFailure(t *testing.T) {
	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if callCount.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write([]byte("audio content"))
	}))
	t.Cleanup(srv.Close)

	d := newPrefetcherTestDB(t)
	cfg := newPrefetcherTestConfig(t)
	seedPrefetcherFeed(t, d)
	d.UpsertEpisode(&db.Episode{
		ID: "pod/ep1", FeedID: "pod", Title: "Episode One",
		OriginalURL: srv.URL + "/ep1.mp3",
		CacheStatus: "none", URLID: "uid1",
	})

	// Zero out retry delays so the test completes quickly.
	origDelays := retryDelays
	retryDelays = []time.Duration{0, 0}
	t.Cleanup(func() { retryDelays = origDelays })

	p := NewPrefetcher(d, cfg)
	p.Start()
	t.Cleanup(p.Stop)

	p.EnqueueFeedEpisodes("pod")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ep, err := d.GetEpisodeByURLID("pod", "uid1")
		if err == nil && ep.CacheStatus == "cached" {
			if n := callCount.Load(); n < 2 {
				t.Errorf("expected at least 2 upstream calls (1 fail + 1 success), got %d", n)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("episode was not cached after retried download within 5 seconds")
}

// TestPrefetcher_Download_MarksFailedAfterAllRetries verifies that once all
// retry attempts are exhausted the episode is marked "failed" in the DB.
func TestPrefetcher_Download_MarksFailedAfterAllRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	d := newPrefetcherTestDB(t)
	cfg := newPrefetcherTestConfig(t)
	seedPrefetcherFeed(t, d)
	d.UpsertEpisode(&db.Episode{
		ID: "pod/ep1", FeedID: "pod", Title: "Episode One",
		OriginalURL: srv.URL + "/ep1.mp3",
		CacheStatus: "none", URLID: "uid1",
	})

	origDelays := retryDelays
	retryDelays = []time.Duration{0, 0}
	t.Cleanup(func() { retryDelays = origDelays })

	p := NewPrefetcher(d, cfg)
	p.Start()
	t.Cleanup(p.Stop)

	p.EnqueueFeedEpisodes("pod")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ep, err := d.GetEpisodeByURLID("pod", "uid1")
		if err == nil && ep.CacheStatus == "failed" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("episode was not marked failed within 5 seconds")
}

// TestPrefetcher_SkipsAlreadyCachedOnWorkerPickup verifies that if an episode
// becomes cached between being enqueued and being processed by a worker, the
// worker skips the download without making any HTTP request.
func TestPrefetcher_SkipsAlreadyCachedOnWorkerPickup(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Write([]byte("audio"))
	}))
	t.Cleanup(srv.Close)

	d := newPrefetcherTestDB(t)
	cfg := newPrefetcherTestConfig(t)
	seedPrefetcherFeed(t, d)
	d.UpsertEpisode(&db.Episode{
		ID: "pod/ep1", FeedID: "pod", Title: "Ep1",
		OriginalURL: srv.URL + "/ep1.mp3",
		CacheStatus: "none", URLID: "uid1",
	})

	// Mark episode as cached before the worker processes it.
	fakePath := "/already/cached/ep1.mp3"
	d.UpdateEpisodeCacheStatus("pod/ep1", "cached", &fakePath, 999, "audio/mpeg")

	p := NewPrefetcher(d, cfg)
	p.Start()
	t.Cleanup(p.Stop)

	// Manually enqueue (bypassing the cache check in EnqueueFeedEpisodes).
	ep, _ := d.GetEpisodeByURLID("pod", "uid1")
	ep.CacheStatus = "none" // simulate stale in-memory state at enqueue time
	p.Enqueue(ep)

	// Give the worker time to process the item.
	time.Sleep(100 * time.Millisecond)

	if callCount != 0 {
		t.Errorf("worker should skip already-cached episode; origin called %d time(s)", callCount)
	}
}

// TestPrefetcher_StartStop verifies that Start and Stop do not deadlock or panic.
func TestPrefetcher_StartStop(t *testing.T) {
	d := newPrefetcherTestDB(t)
	cfg := newPrefetcherTestConfig(t)
	p := NewPrefetcher(d, cfg)
	p.Start()
	p.Stop()
}
