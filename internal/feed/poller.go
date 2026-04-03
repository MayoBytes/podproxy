package feed

import (
	"log"
	"sync"
	"time"

	"podproxy/internal/db"
)

// Poller periodically re-fetches all feeds whose refresh interval has elapsed,
// upserting any new episodes it finds.
type Poller struct {
	database *db.DB
	fetcher  *Fetcher
	stop     chan struct{}
	wg       sync.WaitGroup
}

func NewPoller(database *db.DB, fetcher *Fetcher) *Poller {
	return &Poller{database: database, fetcher: fetcher, stop: make(chan struct{})}
}

// Start launches the poll loop in the background. Call Stop to shut it down.
func (p *Poller) Start() {
	go p.run()
}

// Stop signals the poll loop to exit and waits for all in-flight refreshes to finish.
func (p *Poller) Stop() {
	close(p.stop)
	p.wg.Wait()
}

func (p *Poller) run() {
	// Check every minute which feeds are due for a refresh.
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			p.refreshStaleFeeds()
		case <-p.stop:
			return
		}
	}
}

func (p *Poller) refreshStaleFeeds() {
	feeds, err := p.database.ListFeeds()
	if err != nil {
		log.Printf("poller: list feeds: %v", err)
		return
	}
	for _, f := range feeds {
		due := f.LastFetchedAt == nil ||
			time.Since(*f.LastFetchedAt) >= time.Duration(f.RefreshIntervalMinutes)*time.Minute
		if due {
			p.wg.Add(1)
			go func() {
				defer p.wg.Done()
				p.refreshFeed(f)
			}()
		}
	}
}

func (p *Poller) refreshFeed(f *db.Feed) {
	result, err := p.fetcher.Fetch(f.ID, f.OriginalURL)
	if err != nil {
		log.Printf("poller: fetch %s: %v", f.ID, err)
		return
	}
	for _, ep := range result.Episodes {
		if err := p.database.UpsertEpisode(ep); err != nil {
			log.Printf("poller: upsert episode %s: %v", ep.ID, err)
		}
	}
	if err := p.database.UpdateFeedFetchedAt(f.ID, time.Now()); err != nil {
		log.Printf("poller: update fetched_at %s: %v", f.ID, err)
	}
	log.Printf("poller: refreshed %s (%d episodes)", f.ID, len(result.Episodes))
}
