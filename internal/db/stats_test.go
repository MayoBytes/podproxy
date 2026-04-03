package db

import "testing"

func TestGetGlobalStats_Empty(t *testing.T) {
	d := openTestDB(t)
	stats, err := d.GetGlobalStats()
	if err != nil {
		t.Fatalf("GetGlobalStats: %v", err)
	}
	if stats.FeedCount != 0 {
		t.Errorf("feed_count: want 0, got %d", stats.FeedCount)
	}
	if stats.EpisodeCount != 0 {
		t.Errorf("episode_count: want 0, got %d", stats.EpisodeCount)
	}
	if stats.CachedCount != 0 {
		t.Errorf("cached_count: want 0, got %d", stats.CachedCount)
	}
	if stats.DiskBytes != 0 {
		t.Errorf("disk_bytes: want 0, got %d", stats.DiskBytes)
	}
}

func TestGetGlobalStats_Counts(t *testing.T) {
	d := openTestDB(t)
	d.InsertFeed(&Feed{ID: "pod1", Title: "Pod 1", OriginalURL: "https://example.com/1"})
	d.InsertFeed(&Feed{ID: "pod2", Title: "Pod 2", OriginalURL: "https://example.com/2"})

	d.UpsertEpisode(&Episode{ID: "pod1/ep1", FeedID: "pod1", Title: "Ep1", OriginalURL: "https://cdn.example.com/ep1.mp3", CacheStatus: "none", URLID: "u001"})
	d.UpsertEpisode(&Episode{ID: "pod1/ep2", FeedID: "pod1", Title: "Ep2", OriginalURL: "https://cdn.example.com/ep2.mp3", CacheStatus: "none", URLID: "u002"})

	cachedPath := "/cache/ep1.mp3"
	d.UpdateEpisodeCacheStatus("pod1/ep1", "cached", &cachedPath, 3000, "audio/mpeg")

	stats, err := d.GetGlobalStats()
	if err != nil {
		t.Fatalf("GetGlobalStats: %v", err)
	}
	if stats.FeedCount != 2 {
		t.Errorf("feed_count: want 2, got %d", stats.FeedCount)
	}
	if stats.EpisodeCount != 2 {
		t.Errorf("episode_count: want 2, got %d", stats.EpisodeCount)
	}
	if stats.CachedCount != 1 {
		t.Errorf("cached_count: want 1, got %d", stats.CachedCount)
	}
	if stats.DiskBytes != 3000 {
		t.Errorf("disk_bytes: want 3000, got %d", stats.DiskBytes)
	}
}

func TestGetGlobalStats_OnlyCountsCachedBytes(t *testing.T) {
	d := openTestDB(t)
	d.InsertFeed(&Feed{ID: "pod", Title: "Pod", OriginalURL: "https://example.com"})

	d.UpsertEpisode(&Episode{ID: "pod/ep1", FeedID: "pod", Title: "Ep1", OriginalURL: "https://cdn.example.com/ep1.mp3", CacheStatus: "none", URLID: "u001"})
	d.UpsertEpisode(&Episode{ID: "pod/ep2", FeedID: "pod", Title: "Ep2", OriginalURL: "https://cdn.example.com/ep2.mp3", CacheStatus: "none", URLID: "u002"})
	d.UpsertEpisode(&Episode{ID: "pod/ep3", FeedID: "pod", Title: "Ep3", OriginalURL: "https://cdn.example.com/ep3.mp3", CacheStatus: "none", URLID: "u003"})

	cachedPath := "/cache/ep1.mp3"
	d.UpdateEpisodeCacheStatus("pod/ep1", "cached", &cachedPath, 5000, "audio/mpeg")
	d.UpdateEpisodeCacheStatus("pod/ep2", "failed", nil, 0, "")
	d.UpdateEpisodeCacheStatus("pod/ep3", "in_progress", nil, 0, "")

	stats, err := d.GetGlobalStats()
	if err != nil {
		t.Fatalf("GetGlobalStats: %v", err)
	}
	if stats.CachedCount != 1 {
		t.Errorf("cached_count: want 1, got %d", stats.CachedCount)
	}
	if stats.DiskBytes != 5000 {
		t.Errorf("disk_bytes: want 5000, got %d", stats.DiskBytes)
	}
}
