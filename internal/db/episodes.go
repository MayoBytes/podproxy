package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type Episode struct {
	ID          string
	FeedID      string
	Title       string
	OriginalURL string
	CachedPath  string
	PubDate     *time.Time
	DurationSec int
	SizeBytes   int64
	CacheStatus string
	ContentType string
	URLID       string
}

func (db *DB) UpsertEpisode(e *Episode) error {
	_, err := db.Exec(`
		INSERT INTO episodes (id, feed_id, title, original_url, pub_date, duration_sec, size_bytes, cache_status, url_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			title        = excluded.title,
			original_url = excluded.original_url,
			pub_date     = excluded.pub_date,
			duration_sec = excluded.duration_sec,
			size_bytes   = CASE WHEN episodes.cache_status = 'cached' THEN episodes.size_bytes ELSE excluded.size_bytes END`,
		e.ID, e.FeedID, e.Title, e.OriginalURL, e.PubDate, e.DurationSec, e.SizeBytes, e.CacheStatus, e.URLID,
	)
	return err
}

func (db *DB) GetEpisodeByURLID(feedID, urlID string) (*Episode, error) {
	row := db.QueryRow(`
		SELECT id, feed_id, title, original_url, cached_path, pub_date, duration_sec, size_bytes, cache_status, content_type, url_id
		FROM episodes WHERE feed_id = ? AND url_id = ?`, feedID, urlID)
	return scanEpisode(row)
}

func (db *DB) ListEpisodesByFeed(feedID string) ([]*Episode, error) {
	rows, err := db.Query(`
		SELECT id, feed_id, title, original_url, cached_path, pub_date, duration_sec, size_bytes, cache_status, content_type, url_id
		FROM episodes WHERE feed_id = ? ORDER BY pub_date DESC`, feedID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var eps []*Episode
	for rows.Next() {
		e, err := scanEpisode(rows)
		if err != nil {
			return nil, err
		}
		eps = append(eps, e)
	}
	return eps, rows.Err()
}

func (db *DB) UpdateEpisodeCacheStatus(id, status string, cachedPath *string, sizeBytes int64, contentType string) error {
	_, err := db.Exec(`
		UPDATE episodes SET cache_status = ?, cached_path = ?, size_bytes = ?, content_type = ? WHERE id = ?`,
		status, cachedPath, sizeBytes, contentType, id)
	return err
}

func scanEpisode(s scanner) (*Episode, error) {
	var e Episode
	var cachedPath sql.NullString
	var pubDate sql.NullTime
	var contentType sql.NullString
	err := s.Scan(&e.ID, &e.FeedID, &e.Title, &e.OriginalURL, &cachedPath, &pubDate,
		&e.DurationSec, &e.SizeBytes, &e.CacheStatus, &contentType, &e.URLID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("episode %w", ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if cachedPath.Valid {
		e.CachedPath = cachedPath.String
	}
	if pubDate.Valid {
		e.PubDate = &pubDate.Time
	}
	if contentType.Valid {
		e.ContentType = contentType.String
	}
	return &e, nil
}
