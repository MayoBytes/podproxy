package db

import (
	"errors"
	"testing"
	"time"
)

// seedFeed inserts a minimal feed for episode tests that require a parent row.
func seedFeed(t *testing.T, d *DB, id string) {
	t.Helper()
	if err := d.InsertFeed(&Feed{ID: id, Title: id, OriginalURL: "https://example.com"}); err != nil {
		t.Fatalf("seedFeed %q: %v", id, err)
	}
}

func TestUpsertAndGetEpisode(t *testing.T) {
	d := openTestDB(t)
	seedFeed(t, d, "pod")
	ep := &Episode{
		ID:          "pod/ep1",
		FeedID:      "pod",
		Title:       "Episode 1",
		OriginalURL: "https://cdn.example.com/ep1.mp3",
		CacheStatus: "none",
		URLID:       "urlid0001",
	}
	if err := d.UpsertEpisode(ep); err != nil {
		t.Fatalf("UpsertEpisode: %v", err)
	}
	got, err := d.GetEpisodeByURLID("pod", "urlid0001")
	if err != nil {
		t.Fatalf("GetEpisodeByURLID: %v", err)
	}
	if got.Title != "Episode 1" {
		t.Errorf("title: want %q, got %q", "Episode 1", got.Title)
	}
	if got.OriginalURL != "https://cdn.example.com/ep1.mp3" {
		t.Errorf("original_url: want %q, got %q", "https://cdn.example.com/ep1.mp3", got.OriginalURL)
	}
	if got.CacheStatus != "none" {
		t.Errorf("cache_status: want %q, got %q", "none", got.CacheStatus)
	}
	if got.CachedPath != "" {
		t.Errorf("cached_path: want empty, got %q", got.CachedPath)
	}
}

func TestUpsertEpisode_UpdatesMetadataOnConflict(t *testing.T) {
	d := openTestDB(t)
	seedFeed(t, d, "pod")
	ep := &Episode{
		ID: "pod/ep1", FeedID: "pod", Title: "Old Title",
		OriginalURL: "https://cdn.example.com/ep1.mp3",
		CacheStatus: "none", URLID: "uid1",
	}
	d.UpsertEpisode(ep)

	ep.Title = "New Title"
	ep.OriginalURL = "https://cdn.example.com/ep1-v2.mp3"
	if err := d.UpsertEpisode(ep); err != nil {
		t.Fatalf("UpsertEpisode update: %v", err)
	}
	got, _ := d.GetEpisodeByURLID("pod", "uid1")
	if got.Title != "New Title" {
		t.Errorf("title after upsert: want %q, got %q", "New Title", got.Title)
	}
	if got.OriginalURL != "https://cdn.example.com/ep1-v2.mp3" {
		t.Errorf("original_url after upsert: want new URL, got %q", got.OriginalURL)
	}
}

func TestGetEpisodeByURLID_NotFound(t *testing.T) {
	d := openTestDB(t)
	seedFeed(t, d, "pod")
	_, err := d.GetEpisodeByURLID("pod", "nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestListEpisodesByFeed_OrderedByPubDateDesc(t *testing.T) {
	d := openTestDB(t)
	seedFeed(t, d, "pod")

	t1 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)

	d.UpsertEpisode(&Episode{
		ID: "pod/ep1", FeedID: "pod", Title: "Older",
		OriginalURL: "https://cdn.example.com/ep1.mp3",
		PubDate: &t1, CacheStatus: "none", URLID: "uid1",
	})
	d.UpsertEpisode(&Episode{
		ID: "pod/ep2", FeedID: "pod", Title: "Newer",
		OriginalURL: "https://cdn.example.com/ep2.mp3",
		PubDate: &t2, CacheStatus: "none", URLID: "uid2",
	})

	eps, err := d.ListEpisodesByFeed("pod")
	if err != nil {
		t.Fatalf("ListEpisodesByFeed: %v", err)
	}
	if len(eps) != 2 {
		t.Fatalf("want 2 episodes, got %d", len(eps))
	}
	// Newest first (DESC).
	if eps[0].URLID != "uid2" || eps[1].URLID != "uid1" {
		t.Errorf("order: want [uid2, uid1], got [%s, %s]", eps[0].URLID, eps[1].URLID)
	}
}

func TestListEpisodesByFeed_Empty(t *testing.T) {
	d := openTestDB(t)
	seedFeed(t, d, "pod")
	eps, err := d.ListEpisodesByFeed("pod")
	if err != nil {
		t.Fatalf("ListEpisodesByFeed: %v", err)
	}
	if len(eps) != 0 {
		t.Errorf("want 0 episodes, got %d", len(eps))
	}
}

func TestHasInProgressEpisodes(t *testing.T) {
	d := openTestDB(t)
	seedFeed(t, d, "pod")
	d.UpsertEpisode(&Episode{
		ID: "pod/ep1", FeedID: "pod", Title: "Ep1",
		OriginalURL: "https://cdn.example.com/ep1.mp3",
		CacheStatus: "none", URLID: "uid1",
	})

	if got, _ := d.HasInProgressEpisodes("pod"); got {
		t.Error("want false when no in-progress episodes")
	}

	_ = d.UpdateEpisodeCacheStatus("pod/ep1", "in_progress", nil, 0, "")
	if got, _ := d.HasInProgressEpisodes("pod"); !got {
		t.Error("want true when episode is in_progress")
	}

	// Unrelated feed should not be affected.
	if got, _ := d.HasInProgressEpisodes("other"); got {
		t.Error("want false for feed with no episodes")
	}
}

func TestUpdateEpisodeCacheStatus(t *testing.T) {
	d := openTestDB(t)
	seedFeed(t, d, "pod")
	d.UpsertEpisode(&Episode{
		ID: "pod/ep1", FeedID: "pod", Title: "Ep1",
		OriginalURL: "https://cdn.example.com/ep1.mp3",
		CacheStatus: "none", URLID: "uid1",
	})

	path := "/cache/pod/ep1.mp3"
	if err := d.UpdateEpisodeCacheStatus("pod/ep1", "cached", &path, 98765, "audio/mpeg"); err != nil {
		t.Fatalf("UpdateEpisodeCacheStatus: %v", err)
	}

	got, _ := d.GetEpisodeByURLID("pod", "uid1")
	if got.CacheStatus != "cached" {
		t.Errorf("cache_status: want %q, got %q", "cached", got.CacheStatus)
	}
	if got.CachedPath != path {
		t.Errorf("cached_path: want %q, got %q", path, got.CachedPath)
	}
	if got.SizeBytes != 98765 {
		t.Errorf("size_bytes: want 98765, got %d", got.SizeBytes)
	}
	if got.ContentType != "audio/mpeg" {
		t.Errorf("content_type: want %q, got %q", "audio/mpeg", got.ContentType)
	}
}

func TestUpdateEpisodeCacheStatus_ClearPath(t *testing.T) {
	d := openTestDB(t)
	seedFeed(t, d, "pod")
	d.UpsertEpisode(&Episode{
		ID: "pod/ep1", FeedID: "pod", Title: "Ep1",
		OriginalURL: "https://cdn.example.com/ep1.mp3",
		CacheStatus: "none", URLID: "uid1",
	})
	// Mark as failed with no path.
	if err := d.UpdateEpisodeCacheStatus("pod/ep1", "failed", nil, 0, ""); err != nil {
		t.Fatalf("UpdateEpisodeCacheStatus: %v", err)
	}
	got, _ := d.GetEpisodeByURLID("pod", "uid1")
	if got.CacheStatus != "failed" {
		t.Errorf("cache_status: want %q, got %q", "failed", got.CacheStatus)
	}
	if got.CachedPath != "" {
		t.Errorf("cached_path: want empty, got %q", got.CachedPath)
	}
}
