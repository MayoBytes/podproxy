package db

// GlobalStats holds aggregate counts and storage totals across all feeds.
type GlobalStats struct {
	FeedCount    int
	EpisodeCount int
	CachedCount  int
	DiskBytes    int64
}

// GetGlobalStats returns aggregate episode and storage statistics from the DB.
func (db *DB) GetGlobalStats() (GlobalStats, error) {
	var s GlobalStats
	err := db.QueryRow(`
		SELECT
			(SELECT COUNT(*) FROM feeds)                                             AS feed_count,
			COUNT(*)                                                                 AS episode_count,
			COALESCE(SUM(CASE WHEN cache_status='cached' THEN 1 ELSE 0 END), 0)     AS cached_count,
			COALESCE(SUM(CASE WHEN cache_status='cached' THEN size_bytes ELSE 0 END), 0) AS disk_bytes
		FROM episodes`).Scan(&s.FeedCount, &s.EpisodeCount, &s.CachedCount, &s.DiskBytes)
	return s, err
}
