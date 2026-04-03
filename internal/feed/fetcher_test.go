package feed_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"podproxy/internal/config"
	"podproxy/internal/feed"
)

// minimalRSS is a small, valid RSS feed used as a test fixture.
const minimalRSS = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Test Podcast</title>
    <link>https://example.com</link>
    <description>A test podcast</description>
    <item>
      <title>Episode One</title>
      <guid>ep-guid-001</guid>
      <enclosure url="https://cdn.example.com/ep1.mp3" type="audio/mpeg" length="12345"/>
      <pubDate>Mon, 01 Jan 2024 00:00:00 +0000</pubDate>
    </item>
    <item>
      <title>No Enclosure — should be skipped</title>
      <guid>ep-no-enc</guid>
    </item>
  </channel>
</rss>`

// ---------------------------------------------------------------------------
// Slugify
// ---------------------------------------------------------------------------

var slugTests = []struct {
	input string
	want  string
}{
	{"Hello World", "hello-world"},
	{"  leading spaces  ", "leading-spaces"},
	{"Hello --- World", "hello-world"},
	{"Darknet Diaries", "darknet-diaries"},
	{"99% Invisible", "99-invisible"},
	{"ALL CAPS", "all-caps"},
	{"Already-hyphenated", "already-hyphenated"},
	{"", ""},
	{"---", ""},
	// Length clamped to 60 runes.
	{strings.Repeat("a", 70), strings.Repeat("a", 60)},
	// Trailing hyphen trimmed after truncation.
	{strings.Repeat("a", 59) + "-extra", strings.Repeat("a", 59)},
}

func TestSlugify(t *testing.T) {
	for _, tc := range slugTests {
		got := feed.Slugify(tc.input)
		if got != tc.want {
			t.Errorf("Slugify(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// RewriteXML
// ---------------------------------------------------------------------------

func TestRewriteXML_ReplacesEnclosureURL(t *testing.T) {
	raw := []byte(`<rss><channel><item><enclosure url="https://cdn.example.com/ep1.mp3" type="audio/mpeg" length="0"/></item></channel></rss>`)
	urlMap := map[string]string{
		"https://cdn.example.com/ep1.mp3": "deadbeef",
	}
	got := string(feed.RewriteXML(raw, "mypod", urlMap, "http://proxy.local:8080"))
	if !strings.Contains(got, `url="http://proxy.local:8080/episodes/mypod/deadbeef.mp3"`) {
		t.Errorf("enclosure URL not rewritten; output:\n%s", got)
	}
	if strings.Contains(got, "cdn.example.com") {
		t.Errorf("original CDN URL should have been removed; output:\n%s", got)
	}
}

func TestRewriteXML_ReplacesMediaContentURL(t *testing.T) {
	raw := []byte(`<rss><channel><item><media:content url="https://cdn.example.com/ep1.mp3" type="audio/mpeg"/></item></channel></rss>`)
	urlMap := map[string]string{
		"https://cdn.example.com/ep1.mp3": "abcd1234",
	}
	got := string(feed.RewriteXML(raw, "pod", urlMap, "http://proxy:8080"))
	if !strings.Contains(got, `url="http://proxy:8080/episodes/pod/abcd1234.mp3"`) {
		t.Errorf("media:content URL not rewritten; output:\n%s", got)
	}
}

func TestRewriteXML_UsesTypeAttributeWhenURLHasNoExtension(t *testing.T) {
	raw := []byte(`<rss><channel><item><enclosure url="https://cdn.example.com/episode/abc123" type="audio/mpeg" length="0"/></item></channel></rss>`)
	urlMap := map[string]string{
		"https://cdn.example.com/episode/abc123": "deadbeef",
	}
	got := string(feed.RewriteXML(raw, "mypod", urlMap, "http://proxy.local:8080"))
	if !strings.Contains(got, `url="http://proxy.local:8080/episodes/mypod/deadbeef.mp3"`) {
		t.Errorf("expected .mp3 from type attribute fallback; output:\n%s", got)
	}
}

func TestRewriteXML_LeavesMissingURLsUnchanged(t *testing.T) {
	raw := []byte(`<rss><channel><item><enclosure url="https://cdn.example.com/unknown.mp3" type="audio/mpeg" length="0"/></item></channel></rss>`)
	urlMap := map[string]string{} // empty — no match
	got := string(feed.RewriteXML(raw, "pod", urlMap, "http://proxy:8080"))
	if !strings.Contains(got, `url="https://cdn.example.com/unknown.mp3"`) {
		t.Errorf("original URL was unexpectedly rewritten; output:\n%s", got)
	}
}

func TestRewriteXML_MultipleItems(t *testing.T) {
	raw := []byte(`<rss><channel>` +
		`<item><enclosure url="https://cdn.example.com/ep1.mp3" type="audio/mpeg" length="0"/></item>` +
		`<item><enclosure url="https://cdn.example.com/ep2.mp3" type="audio/mpeg" length="0"/></item>` +
		`</channel></rss>`)
	urlMap := map[string]string{
		"https://cdn.example.com/ep1.mp3": "id0001",
		"https://cdn.example.com/ep2.mp3": "id0002",
	}
	got := string(feed.RewriteXML(raw, "pod", urlMap, "http://proxy:8080"))
	if !strings.Contains(got, "/episodes/pod/id0001") {
		t.Errorf("ep1 URL not rewritten; output:\n%s", got)
	}
	if !strings.Contains(got, "/episodes/pod/id0002") {
		t.Errorf("ep2 URL not rewritten; output:\n%s", got)
	}
}

func TestRewriteXML_RewritesAtomSelfLink(t *testing.T) {
	raw := []byte(`<rss><channel>` +
		`<atom:link href="https://upstream.example.com/feed.rss" rel="self" type="application/rss+xml"/>` +
		`<item><enclosure url="https://cdn.example.com/ep1.mp3" type="audio/mpeg" length="0"/></item>` +
		`</channel></rss>`)
	urlMap := map[string]string{
		"https://cdn.example.com/ep1.mp3": "deadbeef",
	}
	got := string(feed.RewriteXML(raw, "mypod", urlMap, "http://proxy.local:8080"))
	if !strings.Contains(got, `href="http://proxy.local:8080/feeds/mypod.rss"`) {
		t.Errorf("atom:link self href not rewritten; output:\n%s", got)
	}
	if strings.Contains(got, "upstream.example.com") {
		t.Errorf("original upstream URL should have been removed; output:\n%s", got)
	}
}

func TestRewriteXML_RewritesAtomSelfLinkHrefBeforeRel(t *testing.T) {
	// href appears before rel in the tag
	raw := []byte(`<rss><channel>` +
		`<atom:link href="https://upstream.example.com/feed.rss" rel="self"/>` +
		`</channel></rss>`)
	got := string(feed.RewriteXML(raw, "pod", nil, "http://proxy:8080"))
	if !strings.Contains(got, `href="http://proxy:8080/feeds/pod.rss"`) {
		t.Errorf("atom:link self href not rewritten; output:\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// Fetch
// ---------------------------------------------------------------------------

func newTestFetcher(t *testing.T) *feed.Fetcher {
	t.Helper()
	cfg := &config.Config{
		Server:   config.ServerConfig{BaseURL: "http://proxy.local"},
		Defaults: config.DefaultsConfig{RefreshIntervalMinutes: 60},
	}
	return feed.NewFetcher(cfg)
}

func TestFetch_ParsesFeedAndEpisodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(minimalRSS))
	}))
	defer srv.Close()

	result, err := newTestFetcher(t).Fetch("test-podcast", srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if result.Feed.Title != "Test Podcast" {
		t.Errorf("feed title: want %q, got %q", "Test Podcast", result.Feed.Title)
	}
	if result.Feed.ID != "test-podcast" {
		t.Errorf("feed id: want %q, got %q", "test-podcast", result.Feed.ID)
	}
	// Item without enclosure must be skipped.
	if len(result.Episodes) != 1 {
		t.Fatalf("want 1 episode (no-enclosure item skipped), got %d", len(result.Episodes))
	}
	ep := result.Episodes[0]
	if ep.Title != "Episode One" {
		t.Errorf("episode title: want %q, got %q", "Episode One", ep.Title)
	}
	if ep.OriginalURL != "https://cdn.example.com/ep1.mp3" {
		t.Errorf("episode url: want %q, got %q", "https://cdn.example.com/ep1.mp3", ep.OriginalURL)
	}
	if ep.FeedID != "test-podcast" {
		t.Errorf("episode feed_id: want %q, got %q", "test-podcast", ep.FeedID)
	}
	if ep.CacheStatus != "none" {
		t.Errorf("cache_status: want %q, got %q", "none", ep.CacheStatus)
	}
	// URLID is hex(sha256(guid)[:8]) — 16 hex chars.
	if len(ep.URLID) != 16 {
		t.Errorf("urlid length: want 16, got %d (%q)", len(ep.URLID), ep.URLID)
	}
	if len(result.RawXML) == 0 {
		t.Error("RawXML must not be empty")
	}
}

func TestFetch_SetsRefreshIntervalFromConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(minimalRSS))
	}))
	defer srv.Close()

	cfg := &config.Config{
		Server:   config.ServerConfig{BaseURL: "http://proxy.local"},
		Defaults: config.DefaultsConfig{RefreshIntervalMinutes: 30},
	}
	result, err := feed.NewFetcher(cfg).Fetch("pod", srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if result.Feed.RefreshIntervalMinutes != 30 {
		t.Errorf("refresh_interval_minutes: want 30, got %d", result.Feed.RefreshIntervalMinutes)
	}
}

func TestFetch_NonOKStatus_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := newTestFetcher(t).Fetch("pod", srv.URL)
	if err == nil {
		t.Fatal("expected error for 404 response, got nil")
	}
}

func TestFetch_InvalidXML_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("this is not valid RSS XML"))
	}))
	defer srv.Close()

	_, err := newTestFetcher(t).Fetch("pod", srv.URL)
	if err == nil {
		t.Fatal("expected error for invalid XML, got nil")
	}
}

func TestFetch_UnreachableServer_ReturnsError(t *testing.T) {
	_, err := newTestFetcher(t).Fetch("pod", "http://127.0.0.1:1")
	if err == nil {
		t.Fatal("expected error for unreachable server, got nil")
	}
}

// ---------------------------------------------------------------------------
// EpisodeFileExt
// ---------------------------------------------------------------------------

var episodeFileExtTests = []struct {
	rawURL string
	want   string
}{
	{"https://cdn.example.com/ep1.mp3", ".mp3"},
	{"https://cdn.example.com/ep1.mp3?t=123&v=abc", ".mp3"},
	{"https://cdn.example.com/ep1.M4A", ".M4A"},
	{"https://cdn.example.com/episode", ""},
	{"https://cdn.example.com/", ""},
	{"not a url :// bad", ""},
	{"", ""},
}

func TestEpisodeFileExt(t *testing.T) {
	for _, tc := range episodeFileExtTests {
		got := feed.EpisodeFileExt(tc.rawURL)
		if got != tc.want {
			t.Errorf("EpisodeFileExt(%q) = %q, want %q", tc.rawURL, got, tc.want)
		}
	}
}
