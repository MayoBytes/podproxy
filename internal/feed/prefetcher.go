package feed

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"podproxy/internal/config"
	"podproxy/internal/db"
)

// retryDelays controls the wait between download attempts. maxAttempts is
// derived from it so the two values can never get out of sync.
var retryDelays = []time.Duration{5 * time.Second, 30 * time.Second}

const prefetchQueueSize = 256

// Prefetcher downloads episodes to disk in the background using a bounded
// worker pool. Enqueue is non-blocking — jobs are dropped if the queue is full.
type Prefetcher struct {
	database *db.DB
	cfg      *config.Config
	client   *http.Client
	queue    chan *db.Episode
	wg       sync.WaitGroup
	ctx      context.Context
	cancel   context.CancelFunc
}

func NewPrefetcher(database *db.DB, cfg *config.Config) *Prefetcher {
	ctx, cancel := context.WithCancel(context.Background())
	return &Prefetcher{
		database: database,
		cfg:      cfg,
		// HTTP/2 disabled for the same reason as the proxy handler: some CDNs
		// (e.g. Podbean/Cloudflare) reset multiplexed connections when they detect
		// concurrent streams, causing unexpected EOF on every download attempt.
		client: &http.Client{
			Transport: &http.Transport{
				ResponseHeaderTimeout: 30 * time.Second,
				TLSNextProto:          make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
			},
		},
		queue:  make(chan *db.Episode, prefetchQueueSize),
		ctx:    ctx,
		cancel: cancel,
	}
}

// Start launches the worker goroutines. Must be called before Enqueue.
func (p *Prefetcher) Start() {
	concurrency := p.cfg.Defaults.PrefetchConcurrency
	if concurrency <= 0 {
		concurrency = 2
	}
	log.Printf("prefetcher: starting %d workers (queue size %d)", concurrency, prefetchQueueSize)
	for i := 0; i < concurrency; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for ep := range p.queue {
				p.downloadWithRetry(ep)
			}
		}()
	}
}

// Stop cancels in-flight downloads, closes the work queue, and waits for all
// workers to exit. Must only be called once.
func (p *Prefetcher) Stop() {
	p.cancel()     // abort any in-flight HTTP requests
	close(p.queue) // unblock workers waiting for the next item
	p.wg.Wait()
}

// Enqueue adds an episode to the prefetch queue. Returns false if the queue is full.
func (p *Prefetcher) Enqueue(ep *db.Episode) bool {
	select {
	case p.queue <- ep:
		return true
	default:
		log.Printf("prefetcher: queue full, dropping %s", ep.ID)
		return false
	}
}

// EnqueueFeedEpisodes lists all episodes for the given feed and enqueues any
// that are not yet cached and within the prefetch_max_age_days window.
func (p *Prefetcher) EnqueueFeedEpisodes(feedID string) {
	eps, err := p.database.ListEpisodesByFeed(feedID)
	if err != nil {
		log.Printf("prefetcher: list episodes %s: %v", feedID, err)
		return
	}
	// A zero or negative max age means no age limit.
	var cutoff time.Time
	if p.cfg.Defaults.PrefetchMaxAgeDays > 0 {
		cutoff = time.Now().AddDate(0, 0, -p.cfg.Defaults.PrefetchMaxAgeDays)
	}
	queued := 0
	for _, ep := range eps {
		if ep.CacheStatus == "cached" || ep.CacheStatus == "in_progress" {
			continue
		}
		if !cutoff.IsZero() && ep.PubDate != nil && ep.PubDate.Before(cutoff) {
			continue
		}
		if p.Enqueue(ep) {
			queued++
		}
	}
	if queued > 0 {
		log.Printf("prefetcher: enqueued %d episodes for %s", queued, feedID)
	}
}

func (p *Prefetcher) downloadWithRetry(ep *db.Episode) {
	// Re-check cache status — may have been cached by a concurrent proxy request.
	current, err := p.database.GetEpisodeByURLID(ep.FeedID, ep.URLID)
	if err == nil && (current.CacheStatus == "cached" || current.CacheStatus == "in_progress") {
		log.Printf("prefetcher: skip %s (already %s)", ep.ID, current.CacheStatus)
		return
	}

	maxAttempts := len(retryDelays) + 1
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if p.ctx.Err() != nil {
			return
		}
		if attempt > 0 {
			delay := retryDelays[attempt-1]
			log.Printf("prefetcher: retry %d/%d for %s (waiting %s)", attempt, maxAttempts-1, ep.ID, delay)
			select {
			case <-time.After(delay):
			case <-p.ctx.Done():
				return
			}
		}
		if err := p.download(ep); err != nil {
			lastErr = err
			log.Printf("prefetcher: attempt %d failed for %s: %v", attempt+1, ep.ID, err)
			continue
		}
		return
	}
	log.Printf("prefetcher: giving up on %s after %d attempts: %v", ep.ID, maxAttempts, lastErr)
	_ = p.database.UpdateEpisodeCacheStatus(ep.ID, "failed", nil, 0, "")
}

func (p *Prefetcher) download(ep *db.Episode) error {
	cachePath := p.episodeCachePath(ep)

	req, err := http.NewRequestWithContext(p.ctx, http.MethodGet, ep.OriginalURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("origin returned %d", resp.StatusCode)
	}

	_ = p.database.UpdateEpisodeCacheStatus(ep.ID, "in_progress", nil, 0, "")
	contentType := resp.Header.Get("Content-Type")

	return p.cacheBody(ep, cachePath, contentType, resp.Body)
}

// cacheBody writes all bytes from body into a temp file, atomically renames it
// to cachePath, and records the cached state in the DB on success.
func (p *Prefetcher) cacheBody(ep *db.Episode, cachePath, contentType string, body io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(cachePath), ".tmp-")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := f.Name()
	committed := false
	defer func() {
		f.Close()
		if !committed {
			os.Remove(tmpPath)
		}
	}()

	if _, err := io.Copy(f, body); err != nil {
		return fmt.Errorf("write body: %w", err)
	}
	if err := os.Rename(tmpPath, cachePath); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	committed = true

	var sizeBytes int64
	if info, err := os.Stat(cachePath); err == nil {
		sizeBytes = info.Size()
	}
	_ = p.database.UpdateEpisodeCacheStatus(ep.ID, "cached", &cachePath, sizeBytes, contentType)
	log.Printf("prefetcher: cached %s (%d bytes)", ep.ID, sizeBytes)
	return nil
}

// episodeCachePath returns the local disk path for an episode's audio file,
// mirroring the logic in proxy/handler.go.
func (p *Prefetcher) episodeCachePath(ep *db.Episode) string {
	slug := Slugify(ep.Title)
	if slug == "" {
		slug = "episode"
	}
	const maxSlugLen = 80
	if len(slug) > maxSlugLen {
		slug = slug[:maxSlugLen]
	}
	ext := EpisodeFileExt(ep.OriginalURL)
	name := slug + "-" + ep.URLID + ext
	return filepath.Join(p.cfg.Storage.CacheDir, "episodes", ep.FeedID, name)
}
