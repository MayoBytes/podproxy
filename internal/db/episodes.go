package db

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strings"
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

// CountEpisodes returns the total number of episodes recorded for feedID.
func (db *DB) CountEpisodes(feedID string) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM episodes WHERE feed_id = ?`, feedID).Scan(&n)
	return n, err
}

// CountURLIDOverlap returns how many of the supplied urlIDs are already present
// for feedID in the episodes table. Empty input returns 0.
func (db *DB) CountURLIDOverlap(feedID string, urlIDs []string) (int, error) {
	if len(urlIDs) == 0 {
		return 0, nil
	}
	placeholders := strings.Repeat("?,", len(urlIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(urlIDs)+1)
	args = append(args, feedID)
	for _, u := range urlIDs {
		args = append(args, u)
	}
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM episodes WHERE feed_id = ? AND url_id IN (`+placeholders+`)`,
		args...,
	).Scan(&n)
	return n, err
}

// HasInProgressEpisodes reports whether any episode for feedID is currently
// being written to the cache.
func (db *DB) HasInProgressEpisodes(feedID string) (bool, error) {
	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM episodes WHERE feed_id = ? AND cache_status = 'in_progress'`,
		feedID).Scan(&count)
	return count > 0, err
}

func (db *DB) UpdateEpisodeCacheStatus(id, status string, cachedPath *string, sizeBytes int64, contentType string) error {
	_, err := db.Exec(`
		UPDATE episodes SET cache_status = ?, cached_path = ?, size_bytes = ?, content_type = ? WHERE id = ?`,
		status, cachedPath, sizeBytes, contentType, id)
	return err
}

// ReconcileOnStartup resets stale cache state that can occur when the server
// stops while files are being written or when cached files are deleted from
// disk while the server is offline.
//
// It performs two passes:
//  1. Any episode still marked in_progress is reset to none (no write can be
//     in flight if the server just started).
//  2. Any episode marked cached whose file is missing from disk is reset to none.
//
// Both corrections are logged so the operator can see what was reconciled.
func (db *DB) ReconcileOnStartup() error {
	res, err := db.Exec(
		`UPDATE episodes SET cache_status = 'none', cached_path = NULL WHERE cache_status = 'in_progress'`,
	)
	if err != nil {
		return fmt.Errorf("reset in_progress: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("reconcile: reset %d in_progress episodes to none", n)
	}

	rows, err := db.Query(
		`SELECT id, cached_path FROM episodes WHERE cache_status = 'cached' AND cached_path IS NOT NULL`,
	)
	if err != nil {
		return fmt.Errorf("query cached episodes: %w", err)
	}
	defer rows.Close()

	type stale struct{ id, path string }
	var stales []stale
	for rows.Next() {
		var id, path string
		if err := rows.Scan(&id, &path); err != nil {
			return fmt.Errorf("scan cached episode: %w", err)
		}
		_, statErr := os.Stat(path)
		if statErr == nil {
			continue
		}
		if errors.Is(statErr, fs.ErrNotExist) {
			stales = append(stales, stale{id, path})
		} else {
			log.Printf("reconcile: stat %s: %v (skipping)", path, statErr)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate cached episodes: %w", err)
	}

	if len(stales) == 0 {
		return nil
	}

	ids := make([]any, len(stales))
	for i, s := range stales {
		ids[i] = s.id
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	if _, err := db.Exec(
		`UPDATE episodes SET cache_status = 'none', cached_path = NULL WHERE id IN (`+placeholders+`)`,
		ids...,
	); err != nil {
		return fmt.Errorf("reset missing episodes: %w", err)
	}
	log.Printf("reconcile: reset %d cached episodes with missing files to none", len(stales))

	return nil
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
