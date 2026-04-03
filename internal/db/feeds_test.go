package db

import (
	"errors"
	"testing"
	"time"
)

func TestInsertAndGetFeed(t *testing.T) {
	d := openTestDB(t)
	f := &Feed{
		ID:                     "test-podcast",
		Title:                  "Test Podcast",
		OriginalURL:            "https://example.com/feed.rss",
		RefreshIntervalMinutes: 60,
	}
	if err := d.InsertFeed(f); err != nil {
		t.Fatalf("InsertFeed: %v", err)
	}
	got, err := d.GetFeed("test-podcast")
	if err != nil {
		t.Fatalf("GetFeed: %v", err)
	}
	if got.Title != "Test Podcast" {
		t.Errorf("title: want %q, got %q", "Test Podcast", got.Title)
	}
	if got.OriginalURL != "https://example.com/feed.rss" {
		t.Errorf("original_url: want %q, got %q", "https://example.com/feed.rss", got.OriginalURL)
	}
	if got.RefreshIntervalMinutes != 60 {
		t.Errorf("refresh_interval_minutes: want 60, got %d", got.RefreshIntervalMinutes)
	}
	if got.LastFetchedAt != nil {
		t.Errorf("last_fetched_at: want nil, got %v", got.LastFetchedAt)
	}
}

func TestInsertFeed_DuplicateDoesNothing(t *testing.T) {
	d := openTestDB(t)
	f1 := &Feed{ID: "dup", Title: "Original Title", OriginalURL: "https://example.com"}
	if err := d.InsertFeed(f1); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	f2 := &Feed{ID: "dup", Title: "Changed Title", OriginalURL: "https://other.com"}
	if err := d.InsertFeed(f2); err != nil {
		t.Fatalf("duplicate insert: %v", err)
	}
	// ON CONFLICT DO NOTHING: original row must be unchanged.
	got, _ := d.GetFeed("dup")
	if got.Title != "Original Title" {
		t.Errorf("title after duplicate: want %q, got %q", "Original Title", got.Title)
	}
}

func TestGetFeed_NotFound(t *testing.T) {
	d := openTestDB(t)
	_, err := d.GetFeed("nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestListFeeds_OrderedByTitle(t *testing.T) {
	d := openTestDB(t)
	for _, title := range []string{"Zeta", "Alpha", "Mango"} {
		d.InsertFeed(&Feed{ID: title, Title: title, OriginalURL: "https://example.com"})
	}
	feeds, err := d.ListFeeds()
	if err != nil {
		t.Fatalf("ListFeeds: %v", err)
	}
	if len(feeds) != 3 {
		t.Fatalf("want 3 feeds, got %d", len(feeds))
	}
	titles := []string{feeds[0].Title, feeds[1].Title, feeds[2].Title}
	want := []string{"Alpha", "Mango", "Zeta"}
	for i, w := range want {
		if titles[i] != w {
			t.Errorf("feeds[%d].Title: want %q, got %q", i, w, titles[i])
		}
	}
}

func TestListFeeds_Empty(t *testing.T) {
	d := openTestDB(t)
	feeds, err := d.ListFeeds()
	if err != nil {
		t.Fatalf("ListFeeds: %v", err)
	}
	if len(feeds) != 0 {
		t.Errorf("want 0 feeds, got %d", len(feeds))
	}
}

func TestUpdateFeedFetchedAt(t *testing.T) {
	d := openTestDB(t)
	d.InsertFeed(&Feed{ID: "f1", Title: "F1", OriginalURL: "https://example.com"})
	now := time.Now().UTC().Truncate(time.Second)
	if err := d.UpdateFeedFetchedAt("f1", now); err != nil {
		t.Fatalf("UpdateFeedFetchedAt: %v", err)
	}
	got, _ := d.GetFeed("f1")
	if got.LastFetchedAt == nil {
		t.Fatal("LastFetchedAt is nil after update")
	}
	if !got.LastFetchedAt.Equal(now) {
		t.Errorf("LastFetchedAt: want %v, got %v", now, *got.LastFetchedAt)
	}
}

func TestDeleteFeed(t *testing.T) {
	d := openTestDB(t)
	d.InsertFeed(&Feed{ID: "del", Title: "Del", OriginalURL: "https://example.com"})
	if err := d.DeleteFeed("del"); err != nil {
		t.Fatalf("DeleteFeed: %v", err)
	}
	_, err := d.GetFeed("del")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: want ErrNotFound, got %v", err)
	}
}

func TestDeleteFeed_NotFound(t *testing.T) {
	d := openTestDB(t)
	err := d.DeleteFeed("ghost")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestDeleteFeed_CascadesEpisodes(t *testing.T) {
	d := openTestDB(t)
	d.InsertFeed(&Feed{ID: "pod", Title: "Pod", OriginalURL: "https://example.com"})
	d.UpsertEpisode(&Episode{
		ID:          "pod/ep1",
		FeedID:      "pod",
		Title:       "Ep1",
		OriginalURL: "https://cdn.example.com/ep1.mp3",
		CacheStatus: "none",
		URLID:       "abc12345",
	})

	if err := d.DeleteFeed("pod"); err != nil {
		t.Fatalf("DeleteFeed: %v", err)
	}
	eps, err := d.ListEpisodesByFeed("pod")
	if err != nil {
		t.Fatalf("ListEpisodesByFeed after delete: %v", err)
	}
	if len(eps) != 0 {
		t.Errorf("want 0 episodes after feed delete, got %d", len(eps))
	}
}

// ---------------------------------------------------------------------------
// ListFeedsWithStats
// ---------------------------------------------------------------------------

func TestListFeedsWithStats_Empty(t *testing.T) {
	d := openTestDB(t)
	feeds, err := d.ListFeedsWithStats()
	if err != nil {
		t.Fatalf("ListFeedsWithStats: %v", err)
	}
	if len(feeds) != 0 {
		t.Errorf("want 0 feeds, got %d", len(feeds))
	}
}

func TestListFeedsWithStats_NoEpisodes(t *testing.T) {
	d := openTestDB(t)
	d.InsertFeed(&Feed{ID: "pod", Title: "Pod", OriginalURL: "https://example.com"})

	feeds, err := d.ListFeedsWithStats()
	if err != nil {
		t.Fatalf("ListFeedsWithStats: %v", err)
	}
	if len(feeds) != 1 {
		t.Fatalf("want 1 feed, got %d", len(feeds))
	}
	if feeds[0].EpisodeCount != 0 {
		t.Errorf("episode_count: want 0, got %d", feeds[0].EpisodeCount)
	}
	if feeds[0].CachedCount != 0 {
		t.Errorf("cached_count: want 0, got %d", feeds[0].CachedCount)
	}
	if feeds[0].TotalBytes != 0 {
		t.Errorf("total_bytes: want 0, got %d", feeds[0].TotalBytes)
	}
}

func TestListFeedsWithStats_AggregatesCorrectly(t *testing.T) {
	d := openTestDB(t)
	d.InsertFeed(&Feed{ID: "pod", Title: "Pod", OriginalURL: "https://example.com"})

	d.UpsertEpisode(&Episode{ID: "pod/ep1", FeedID: "pod", Title: "Ep1", OriginalURL: "https://cdn.example.com/ep1.mp3", CacheStatus: "none", URLID: "u001"})
	d.UpsertEpisode(&Episode{ID: "pod/ep2", FeedID: "pod", Title: "Ep2", OriginalURL: "https://cdn.example.com/ep2.mp3", CacheStatus: "none", URLID: "u002"})
	d.UpsertEpisode(&Episode{ID: "pod/ep3", FeedID: "pod", Title: "Ep3", OriginalURL: "https://cdn.example.com/ep3.mp3", CacheStatus: "none", URLID: "u003"})

	p1 := "/cache/ep1.mp3"
	d.UpdateEpisodeCacheStatus("pod/ep1", "cached", &p1, 1024, "audio/mpeg")
	p2 := "/cache/ep2.mp3"
	d.UpdateEpisodeCacheStatus("pod/ep2", "cached", &p2, 2048, "audio/mpeg")
	// ep3 stays "none"

	feeds, err := d.ListFeedsWithStats()
	if err != nil {
		t.Fatalf("ListFeedsWithStats: %v", err)
	}
	if len(feeds) != 1 {
		t.Fatalf("want 1 feed, got %d", len(feeds))
	}
	f := feeds[0]
	if f.EpisodeCount != 3 {
		t.Errorf("episode_count: want 3, got %d", f.EpisodeCount)
	}
	if f.CachedCount != 2 {
		t.Errorf("cached_count: want 2, got %d", f.CachedCount)
	}
	if f.TotalBytes != 3072 {
		t.Errorf("total_bytes: want 3072, got %d", f.TotalBytes)
	}
}

func TestListFeedsWithStats_OrderedByTitle(t *testing.T) {
	d := openTestDB(t)
	d.InsertFeed(&Feed{ID: "z-pod", Title: "Zeta", OriginalURL: "https://example.com/z"})
	d.InsertFeed(&Feed{ID: "a-pod", Title: "Alpha", OriginalURL: "https://example.com/a"})
	d.InsertFeed(&Feed{ID: "m-pod", Title: "Mango", OriginalURL: "https://example.com/m"})

	feeds, err := d.ListFeedsWithStats()
	if err != nil {
		t.Fatalf("ListFeedsWithStats: %v", err)
	}
	if len(feeds) != 3 {
		t.Fatalf("want 3 feeds, got %d", len(feeds))
	}
	want := []string{"Alpha", "Mango", "Zeta"}
	for i, w := range want {
		if feeds[i].Feed.Title != w {
			t.Errorf("feeds[%d].Title: want %q, got %q", i, w, feeds[i].Feed.Title)
		}
	}
}
