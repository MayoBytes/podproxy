package feed

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/mmcdole/gofeed"
	"podproxy/internal/config"
	"podproxy/internal/db"
)

type Fetcher struct {
	client *http.Client
	cfg    *config.Config
}

func NewFetcher(cfg *config.Config) *Fetcher {
	return &Fetcher{
		client: &http.Client{Timeout: 30 * time.Second},
		cfg:    cfg,
	}
}

// FetchResult contains parsed feed data ready to store.
type FetchResult struct {
	Feed     *db.Feed
	Episodes []*db.Episode
	RawXML   []byte
}

// Fetch downloads and parses an RSS feed URL, returning structured data for DB
// storage plus the raw XML bytes for rewriting. A single HTTP request is made.
func (f *Fetcher) Fetch(feedID, rawURL string) (*FetchResult, error) {
	resp, err := f.client.Get(rawURL)
	if err != nil {
		return nil, fmt.Errorf("fetch feed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("feed returned %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read feed body: %w", err)
	}

	parser := gofeed.NewParser()
	parsed, err := parser.ParseString(string(data))
	if err != nil {
		return nil, fmt.Errorf("parse feed: %w", err)
	}

	feed := &db.Feed{
		ID:                     feedID,
		Title:                  parsed.Title,
		OriginalURL:            rawURL,
		RefreshIntervalMinutes: f.cfg.Defaults.RefreshIntervalMinutes,
		AutoPrefetch:           f.cfg.Defaults.AutoPrefetch,
	}

	episodes := make([]*db.Episode, 0, len(parsed.Items))
	for _, item := range parsed.Items {
		ep := itemToEpisode(feedID, item)
		if ep != nil {
			episodes = append(episodes, ep)
		}
	}

	return &FetchResult{Feed: feed, Episodes: episodes, RawXML: data}, nil
}

func itemToEpisode(feedID string, item *gofeed.Item) *db.Episode {
	if len(item.Enclosures) == 0 {
		return nil
	}

	guid := item.GUID
	if guid == "" {
		guid = item.Link
	}
	if guid == "" {
		return nil
	}

	urlID := episodeURLID(guid)
	dbID := feedID + "/" + guid

	ep := &db.Episode{
		ID:          dbID,
		FeedID:      feedID,
		Title:       item.Title,
		OriginalURL: item.Enclosures[0].URL,
		CacheStatus: "none",
		URLID:       urlID,
	}

	if item.PublishedParsed != nil {
		ep.PubDate = item.PublishedParsed
	}

	return ep
}

// episodeURLID returns a short, URL-safe identifier derived from the RSS GUID.
func episodeURLID(guid string) string {
	h := sha256.Sum256([]byte(guid))
	return hex.EncodeToString(h[:8])
}

// EpisodeFileExt extracts the file extension from an episode URL's path (e.g. ".mp3").
// Returns an empty string if no extension is found or the URL is malformed.
func EpisodeFileExt(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		if ext := filepath.Ext(u.Path); ext != "" {
			return ext
		}
	}
	return ""
}

// Slugify converts a feed title into a URL-safe identifier.
func Slugify(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	prevHyphen := false
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			prevHyphen = false
		} else if !prevHyphen && b.Len() > 0 {
			b.WriteByte('-')
			prevHyphen = true
		}
	}
	result := []rune(strings.TrimRight(b.String(), "-"))
	if len(result) > 60 {
		result = result[:60]
		// trim trailing hyphen after truncation
		for len(result) > 0 && result[len(result)-1] == '-' {
			result = result[:len(result)-1]
		}
	}
	return string(result)
}

// rewritePattern matches enclosure and media:content URLs in RSS XML.
var rewritePattern = regexp.MustCompile(`(<(?:enclosure|media:content)[^>]*\s)url="([^"]*)"`)

// RewriteXML rewrites all enclosure/media:content URLs in raw RSS XML to point
// to the proxy server, using the episode URL IDs from the DB.
func RewriteXML(raw []byte, feedID string, urlMap map[string]string, baseURL string) []byte {
	result := rewritePattern.ReplaceAllFunc(raw, func(match []byte) []byte {
		sub := rewritePattern.FindSubmatch(match)
		if sub == nil {
			return match
		}
		origURL := string(sub[2])
		if urlID, ok := urlMap[origURL]; ok {
			newURL := fmt.Sprintf("%s/episodes/%s/%s", baseURL, feedID, urlID)
			return []byte(string(sub[1]) + `url="` + newURL + `"`)
		}
		return match
	})
	return result
}
