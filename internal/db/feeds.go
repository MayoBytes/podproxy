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
