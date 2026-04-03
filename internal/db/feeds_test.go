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
