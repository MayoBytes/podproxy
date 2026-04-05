package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type Feed struct {
	ID                    string
	Title                 string
	OriginalURL           string
	LastFetchedAt         *time.Time
	RefreshIntervalMinutes int
	AutoPrefetch          bool
}

var ErrNotFound = errors.New("not found")

func (db *DB) InsertFeed(f *Feed) error {
	_, err := db.Exec(`
		INSERT INTO feeds (id, title, original_url, refresh_interval_minutes, auto_prefetch)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		f.ID, f.Title, f.OriginalURL, f.RefreshIntervalMinutes, f.AutoPrefetch,
	)
	return err
}

func (db *DB) GetFeed(id string) (*Feed, error) {
	row := db.QueryRow(`
		SELECT id, title, original_url, last_fetched_at, refresh_interval_minutes, auto_prefetch
		FROM feeds WHERE id = ?`, id)
	return scanFeed(row)
}

func (db *DB) ListFeeds() ([]*Feed, error) {
	rows, err := db.Query(`
		SELECT id, title, original_url, last_fetched_at, refresh_interval_minutes, auto_prefetch
		FROM feeds ORDER BY title`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var feeds []*Feed
	for rows.Next() {
		f, err := scanFeed(rows)
		if err != nil {
			return nil, err
		}
		feeds = append(feeds, f)
	}
	return feeds, rows.Err()
}

func (db *DB) UpdateFeedFetchedAt(id string, t time.Time) error {
	_, err := db.Exec(`UPDATE feeds SET last_fetched_at = ? WHERE id = ?`, t, id)
	return err
}

func (db *DB) ToggleFeedAutoPrefetch(id string) (bool, error) {
	res, err := db.Exec(`UPDATE feeds SET auto_prefetch = NOT auto_prefetch WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, fmt.Errorf("feed %w", ErrNotFound)
	}
	var newVal bool
	err = db.QueryRow(`SELECT auto_prefetch FROM feeds WHERE id = ?`, id).Scan(&newVal)
	return newVal, err
}

// DeleteFeed removes a feed and all its episodes atomically.
// Returns ErrNotFound if no feed with that ID exists.
func (db *DB) DeleteFeed(id string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Belt-and-suspenders: delete episodes explicitly in case the schema was
	// created before ON DELETE CASCADE was added.
	if _, err := tx.Exec(`DELETE FROM episodes WHERE feed_id = ?`, id); err != nil {
		return err
	}

	res, err := tx.Exec(`DELETE FROM feeds WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("feed %w", ErrNotFound)
	}
	return tx.Commit()
}

// FeedWithStats extends Feed with aggregated episode statistics.
type FeedWithStats struct {
	*Feed
	EpisodeCount int
	CachedCount  int
	TotalBytes   int64
}

// ListFeedsWithStats returns all feeds ordered by title, each annotated with
// episode counts and total cached bytes derived from the episodes table.
func (db *DB) ListFeedsWithStats() ([]*FeedWithStats, error) {
	rows, err := db.Query(`
		SELECT f.id, f.title, f.original_url, f.last_fetched_at,
		       f.refresh_interval_minutes, f.auto_prefetch,
		       COUNT(e.id)                                                        AS episode_count,
		       COALESCE(SUM(CASE WHEN e.cache_status='cached' THEN 1 ELSE 0 END),0) AS cached_count,
		       COALESCE(SUM(CASE WHEN e.cache_status='cached' THEN e.size_bytes ELSE 0 END), 0) AS total_bytes
		FROM feeds f
		LEFT JOIN episodes e ON e.feed_id = f.id
		GROUP BY f.id
		ORDER BY f.title`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []*FeedWithStats
	for rows.Next() {
		var fws FeedWithStats
		var f Feed
		var lastFetched sql.NullTime
		if err := rows.Scan(&f.ID, &f.Title, &f.OriginalURL, &lastFetched,
			&f.RefreshIntervalMinutes, &f.AutoPrefetch,
			&fws.EpisodeCount, &fws.CachedCount, &fws.TotalBytes); err != nil {
			return nil, err
		}
		if lastFetched.Valid {
			f.LastFetchedAt = &lastFetched.Time
		}
		fws.Feed = &f
		result = append(result, &fws)
	}
	return result, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanFeed(s scanner) (*Feed, error) {
	var f Feed
	var lastFetched sql.NullTime
	err := s.Scan(&f.ID, &f.Title, &f.OriginalURL, &lastFetched, &f.RefreshIntervalMinutes, &f.AutoPrefetch)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("feed %w", ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if lastFetched.Valid {
		f.LastFetchedAt = &lastFetched.Time
	}
	return &f, nil
}
