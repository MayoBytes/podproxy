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
	"strconv"
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

	if item.Enclosures[0].Length != "" {
		if n, err := strconv.ParseInt(item.Enclosures[0].Length, 10, 64); err == nil && n > 0 {
			ep.SizeBytes = n
		}
	}

	if item.ITunesExt != nil && item.ITunesExt.Duration != "" {
		ep.DurationSec = parseDuration(item.ITunesExt.Duration)
	}

	return ep
}

// parseDuration parses an iTunes duration string into seconds.
// Accepts "HH:MM:SS", "MM:SS", or a plain integer seconds value.
func parseDuration(s string) int {
	parts := strings.Split(s, ":")
	switch len(parts) {
	case 1:
		n, _ := strconv.Atoi(parts[0])
		return n
	case 2:
		m, _ := strconv.Atoi(parts[0])
		sec, _ := strconv.Atoi(parts[1])
		return m*60 + sec
	case 3:
		h, _ := strconv.Atoi(parts[0])
		m, _ := strconv.Atoi(parts[1])
		sec, _ := strconv.Atoi(parts[2])
		return h*3600 + m*60 + sec
	}
	return 0
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

// rewritePattern matches the full enclosure or media:content tag.
var rewritePattern = regexp.MustCompile(`<(?:enclosure|media:content)[^>]*/?>`)

// atomSelfPattern matches an atom:link tag that carries rel="self".
var atomSelfPattern = regexp.MustCompile(`<atom:link[^>]*\brel="self"[^>]*/?>`)

// urlAttrPattern extracts the url="..." attribute from a tag.
var urlAttrPattern = regexp.MustCompile(`\burl="([^"]*)"`)

// hrefAttrPattern extracts the href="..." attribute from a tag.
var hrefAttrPattern = regexp.MustCompile(`\bhref="[^"]*"`)

// typeAttrPattern extracts the type="..." attribute from a tag.
var typeAttrPattern = regexp.MustCompile(`\btype="([^"]*)"`)

// itunesBlockPattern matches any existing itunes:block element so it can be removed.
var itunesBlockPattern = regexp.MustCompile(`(?i)<itunes:block>[^<]*</itunes:block>`)

// channelOpenPattern matches the opening <channel> tag to inject after it.
var channelOpenPattern = regexp.MustCompile(`<channel>`)

// mimeToExt maps common podcast MIME types to file extensions, used as a
// fallback when the original enclosure URL has no extension in its path.
var mimeToExt = map[string]string{
	"audio/mpeg":  ".mp3",
	"audio/mp4":   ".m4a",
	"audio/x-m4a": ".m4a",
	"audio/ogg":   ".ogg",
	"audio/opus":  ".opus",
	"audio/aac":   ".aac",
	"audio/wav":   ".wav",
	"audio/flac":  ".flac",
	"video/mp4":   ".mp4",
}

// RewriteXML rewrites all enclosure/media:content URLs in raw RSS XML to point
// to the proxy server, using the episode URL IDs from the DB. It also rewrites
// the atom:link rel="self" href to the proxy feed URL so that podcast clients
// (e.g. Apple Podcasts) do not follow the original upstream feed URL.
func RewriteXML(raw []byte, feedID string, urlMap map[string]string, baseURL string) []byte {
	result := rewritePattern.ReplaceAllFunc(raw, func(match []byte) []byte {
		urlSub := urlAttrPattern.FindSubmatch(match)
		if urlSub == nil {
			return match
		}
		origURL := string(urlSub[1])
		urlID, ok := urlMap[origURL]
		if !ok {
			return match
		}
		ext := EpisodeFileExt(origURL)
		if ext == "" {
			if tm := typeAttrPattern.FindSubmatch(match); tm != nil {
				ext = mimeToExt[string(tm[1])]
			}
		}
		newURL := fmt.Sprintf("%s/episodes/%s/%s%s", baseURL, feedID, urlID, ext)
		return urlAttrPattern.ReplaceAll(match, []byte(`url="`+newURL+`"`))
	})

	proxyFeedURL := fmt.Sprintf("%s/feeds/%s.rss", baseURL, feedID)
	result = atomSelfPattern.ReplaceAllFunc(result, func(match []byte) []byte {
		return hrefAttrPattern.ReplaceAll(match, []byte(`href="`+proxyFeedURL+`"`))
	})

	// Force itunes:block=Yes so the proxy feed is never picked up by a podcast
	// directory index, regardless of what the upstream feed says.
	result = itunesBlockPattern.ReplaceAll(result, nil)
	result = channelOpenPattern.ReplaceAllLiteral(result, []byte("<channel><itunes:block>Yes</itunes:block>"))

	return result
}
