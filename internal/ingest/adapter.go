package ingest

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
)

// SourceAdapter reads the fetched body of one kind of source (05 §12.4:
// one adapter per source type, chosen by ingest_sources.adapter). Adding a
// site is a new adapter file, its name in AdapterNames and the migration
// CHECK, and one line in defaultAdapters; the adapter's fixture test proves
// it reads the site (testdata/).
type SourceAdapter interface {
	// Name is the ingest_sources.adapter value.
	Name() AdapterName
	// Kind is what the adapter yields.
	Kind() Kind
	// Accept lists the content types it reads; nil: the source is never
	// fetched (instagram_graph, manual_paste: Aşama 2/3).
	Accept() []string
	// Parse reads one page (untrusted data).
	Parse(ctx context.Context, src Source, page Page) (Parsed, error)
}

// Parsed is what an adapter read from a page.
type Parsed struct {
	// Clubs are the entries of an official club list.
	Clubs []ClubCandidate
	// Docs are the texts for the extractor (not yet masked).
	Docs []Document
	// Links are the pages a sitemap names (child sources, fetched once).
	Links []Link
}

// ClubCandidate is one entry of an official club list: the org's facts
// only. A board member's name or address is never read (Sabancı's detail
// pages carry them: they are not fetched).
type ClubCandidate struct {
	Name           string
	Abbr           string
	SourceCategory string            // the university's own category label
	Website        string            // the club's own site (absolute http(s) URL) or ""
	Email          string            // the club's address on the page or ""
	Social         map[string]string // network → profile URL (never fetched)
}

// Link is a page a sitemap names.
type Link struct {
	URL     string
	LastMod *time.Time
}

// errParse marks a body the adapter cannot read.
var errParse = errors.New("ingest: unreadable page")

// errUnsupported marks an adapter that never fetches.
var errUnsupported = errors.New("ingest: this adapter does not fetch")

// Content types.
var (
	acceptHTML = []string{"text/html", "application/xhtml+xml"}
	acceptJSON = []string{"application/json", "text/json", "text/plain"}
	acceptFeed = []string{"application/rss+xml", "application/atom+xml", "application/xml", "text/xml"}
)

// defaultAdapters are the adapters by name.
func defaultAdapters() map[AdapterName]SourceAdapter {
	out := map[AdapterName]SourceAdapter{}
	for _, a := range []SourceAdapter{
		htmlPageAdapter{}, jsonAPIAdapter{}, rssAdapter{}, sitemapAdapter{},
		placeholderAdapter{name: AdapterInstagramGraph}, placeholderAdapter{name: AdapterManualPaste},
		ytuClubList{}, bounClubList{}, sabanciClubList{}, metuClubList{}, ituClubList{},
	} {
		out[a.Name()] = a
	}
	return out
}

// placeholderAdapter holds the place of the adapters of Aşama 2 and 3
// (research §4): manual_paste (the club pastes its own post) and
// instagram_graph (the club's own account, Graph API after Meta's review).
// Neither fetches anything here; a source of theirs is a run "unsupported".
type placeholderAdapter struct{ name AdapterName }

func (a placeholderAdapter) Name() AdapterName { return a.name }
func (placeholderAdapter) Kind() Kind          { return KindEvents }
func (placeholderAdapter) Accept() []string    { return nil }
func (placeholderAdapter) Parse(context.Context, Source, Page) (Parsed, error) {
	return Parsed{}, errUnsupported
}

// pageURL parses the page's final URL (its links resolve against it).
func pageURL(p Page) *url.URL {
	u, err := url.Parse(p.URL)
	if err != nil {
		return nil
	}
	return u
}

// socialHosts maps the profile hosts to the social_links key. A link to
// one of them is a profile link only: never a source, never fetched.
var socialHosts = map[string]string{
	"instagram.com": "instagram", "twitter.com": "x", "x.com": "x", "facebook.com": "facebook",
	"fb.com": "facebook", "youtube.com": "youtube", "youtu.be": "youtube", "linkedin.com": "linkedin",
	"tiktok.com": "tiktok", "t.me": "telegram", "discord.gg": "discord", "discord.com": "discord",
	"linktr.ee": "linktree", "wa.me": "whatsapp", "whatsapp.com": "whatsapp", "chat.whatsapp.com": "whatsapp",
	"github.com": "github", "medium.com": "medium", "spotify.com": "spotify", "open.spotify.com": "spotify",
}

// socialKey returns the network of a profile URL and the URL forced to
// https, or ok=false for another host, a login wall or a malformed URL.
func socialKey(raw string) (key, link string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return "", "", false
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	host = strings.TrimPrefix(host, "m.")
	key, ok = socialHosts[host]
	if !ok || strings.Contains(strings.ToLower(u.Path), "login") || len(raw) > 300 {
		return "", "", false
	}
	u.Scheme, u.Fragment = "https", ""
	return key, u.String(), true
}

// isSocial reports whether a URL points at a social network or a link
// aggregator (such a "website" is a profile link, not a source).
func isSocial(raw string) bool {
	_, _, ok := socialKey(raw)
	return ok
}
