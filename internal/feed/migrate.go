package feed

import (
	"errors"
	"fmt"

	"podproxy/internal/db"
)

// MigrationPreview summarises the consequences of pointing a feed at a new
// upstream URL. It is the result of fetching the candidate URL, parsing it,
// and comparing the new episode set against what is already stored.
//
// Warnings are "soft" — they can be acknowledged and bypassed via a force
// flag at commit time. Hard errors (URL unreachable, invalid RSS, no-op
// migration) are surfaced as Go errors instead, never as warnings.
type MigrationPreview struct {
	NewURL           string   `json:"new_url"`
	CurrentURL       string   `json:"current_url"`
	CurrentTitle     string   `json:"current_title"`
	NewTitle         string   `json:"new_title"`
	NewEpisodeCount  int      `json:"new_episode_count"`  // items with enclosures in the new feed
	MatchingGUIDs    int      `json:"matching_guids"`     // new-feed GUIDs already known to us
	NewGUIDs         int      `json:"new_guids"`          // NewEpisodeCount - MatchingGUIDs
	ExistingEpisodes int      `json:"existing_episodes"`  // total episodes currently stored for this feed
	OrphanCount      int      `json:"orphan_count"`       // existing episodes that won't appear in the new feed
	TitleChanged     bool     `json:"title_changed"`
	NewArtworkURL    string   `json:"new_artwork_url"`
	Warnings         []string `json:"warnings"`
}

// ErrMigrationNoChange is returned when the candidate URL exactly matches the
// feed's current original_url. Callers should surface this as a hard 400 / UI
// error rather than treating it as a warning.
var ErrMigrationNoChange = errors.New("new URL is the same as the current URL")

// PreviewMigration fetches newURL, compares it against the episodes stored for
// feedID, and returns a structured summary plus any soft warnings. The parsed
// FetchResult is also returned so a subsequent commit can reuse it without
// re-fetching the upstream feed.
//
// Hard failures (unreachable URL, parse failure, identical URL) come back as
// errors. Soft mismatches (partial GUID overlap, empty feed, etc.) live in
// Warnings so the caller can decide whether to require a force flag.
func (f *Fetcher) PreviewMigration(database *db.DB, feedID, newURL string) (*MigrationPreview, *FetchResult, error) {
	cur, err := database.GetFeed(feedID)
	if err != nil {
		return nil, nil, err
	}
	if newURL == cur.OriginalURL {
		return nil, nil, ErrMigrationNoChange
	}

	// Fetch using the existing feed ID so episode IDs and url_ids would be
	// derived identically to a real commit — we only need them to compute
	// overlap against the existing DB rows.
	result, err := f.Fetch(feedID, newURL)
	if err != nil {
		return nil, nil, err
	}

	urlIDs := make([]string, 0, len(result.Episodes))
	for _, ep := range result.Episodes {
		urlIDs = append(urlIDs, ep.URLID)
	}

	matched, err := database.CountURLIDOverlap(feedID, urlIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("count url_id overlap: %w", err)
	}
	existing, err := database.CountEpisodes(feedID)
	if err != nil {
		return nil, nil, fmt.Errorf("count existing episodes: %w", err)
	}

	preview := &MigrationPreview{
		NewURL:           newURL,
		CurrentURL:       cur.OriginalURL,
		CurrentTitle:     cur.Title,
		NewTitle:         result.Feed.Title,
		NewEpisodeCount:  len(result.Episodes),
		MatchingGUIDs:    matched,
		NewGUIDs:         len(result.Episodes) - matched,
		ExistingEpisodes: existing,
		OrphanCount:      existing - matched,
		TitleChanged:     cur.Title != result.Feed.Title,
		NewArtworkURL:    result.ArtworkURL,
	}

	// Soft warnings — operator can choose to override.
	if preview.NewEpisodeCount == 0 {
		preview.Warnings = append(preview.Warnings,
			"the new feed contains no episodes with enclosures")
	}
	if existing > 0 && preview.NewEpisodeCount > 0 {
		switch {
		case matched == 0:
			preview.Warnings = append(preview.Warnings,
				"none of the new feed's episode GUIDs match the existing feed — this may be a different podcast")
		case matched*2 < existing:
			pct := (matched * 100) / existing
			preview.Warnings = append(preview.Warnings,
				fmt.Sprintf("only %d of %d existing episodes (%d%%) appear in the new feed", matched, existing, pct))
		}
	}

	return preview, result, nil
}
