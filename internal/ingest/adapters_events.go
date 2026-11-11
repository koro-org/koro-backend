package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html/charset"
)

// The events adapters: the page (or each item of a feed or JSON list) as a
// text document for the extractor; the sitemap's pages as child sources.

// maxItems bounds the items read from one feed or JSON list.
const maxItems = 100

// maxLinks bounds the pages one sitemap run adds (newest first).
const maxLinks = 30

// htmlPageAdapter reads an events page: its main text (<main>, else
// <article>, else <body>, without navigation, header and footer).
type htmlPageAdapter struct{}

func (htmlPageAdapter) Name() AdapterName { return AdapterHTMLPage }
func (htmlPageAdapter) Kind() Kind        { return KindEvents }
func (htmlPageAdapter) Accept() []string  { return acceptHTML }

func (htmlPageAdapter) Parse(_ context.Context, _ Source, p Page) (Parsed, error) {
	root, err := parseHTML(p.Body)
	if err != nil {
		return Parsed{}, fmt.Errorf("%w: %w", errParse, err)
	}
	text := nodeText(mainNode(root), false)
	if text == "" {
		return Parsed{}, nil
	}
	return Parsed{Docs: []Document{{URL: p.URL, Text: text}}}, nil
}

// jsonAPIAdapter reads a public JSON list (İTÜ's etkinlik embed): every
// object of the top-level array, or of each array-valued property of the
// top-level object, becomes a document of "key: value" lines (strings,
// numbers and booleans; image and logo fields left out; HTML tags
// stripped). No field mapping per site: the extractor reads the lines.
type jsonAPIAdapter struct{}

func (jsonAPIAdapter) Name() AdapterName { return AdapterJSONAPI }
func (jsonAPIAdapter) Kind() Kind        { return KindEvents }
func (jsonAPIAdapter) Accept() []string  { return acceptJSON }

func (jsonAPIAdapter) Parse(_ context.Context, _ Source, p Page) (Parsed, error) {
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimPrefix(p.Body, []byte("\xef\xbb\xbf"))))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return Parsed{}, fmt.Errorf("%w: %w", errParse, err)
	}
	var items []map[string]any
	collect := func(a []any) {
		for _, x := range a {
			if m, ok := x.(map[string]any); ok && len(items) < maxItems {
				items = append(items, m)
			}
		}
	}
	switch x := v.(type) {
	case []any:
		collect(x)
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if a, ok := x[k].([]any); ok {
				collect(a)
			}
		}
	default:
		return Parsed{}, fmt.Errorf("%w: not a list", errParse)
	}
	out := Parsed{}
	for _, m := range items {
		if text := jsonItemText(m); text != "" {
			out.Docs = append(out.Docs, Document{URL: p.URL, Text: text})
		}
	}
	return out, nil
}

// jsonItemText renders an object's scalar fields, sorted by key.
func jsonItemText(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "gorsel") || strings.Contains(lk, "image") || strings.Contains(lk, "img") ||
			strings.Contains(lk, "logo") || strings.Contains(lk, "photo") || strings.Contains(lk, "foto") {
			continue
		}
		var val string
		switch x := m[k].(type) {
		case string:
			val = stripTags(x)
		case json.Number:
			val = x.String()
		case bool:
			val = strconv.FormatBool(x)
		default:
			continue
		}
		if val = tidyLines(val); val != "" {
			b.WriteString(k + ": " + val + "\n")
		}
	}
	return strings.TrimSpace(b.String())
}

// stripTags returns the text of an HTML fragment (a plain string stays).
func stripTags(s string) string {
	if !strings.ContainsAny(s, "<&") {
		return s
	}
	root, err := parseHTML([]byte("<body>" + s + "</body>"))
	if err != nil {
		return s
	}
	return nodeText(root, true)
}

// rssAdapter reads an RSS 2.0 or Atom feed: each item's title, date and
// description (tags stripped) is a document whose source_url is the item's
// link.
type rssAdapter struct{}

func (rssAdapter) Name() AdapterName { return AdapterRSS }
func (rssAdapter) Kind() Kind        { return KindEvents }
func (rssAdapter) Accept() []string  { return acceptFeed }

type rssFeed struct {
	Items []struct {
		Title       string `xml:"title"`
		Link        string `xml:"link"`
		Description string `xml:"description"`
		PubDate     string `xml:"pubDate"`
	} `xml:"channel>item"`
	Entries []struct {
		Title   string `xml:"title"`
		Summary string `xml:"summary"`
		Content string `xml:"content"`
		Updated string `xml:"updated"`
		Links   []struct {
			Href string `xml:"href,attr"`
			Rel  string `xml:"rel,attr"`
		} `xml:"link"`
	} `xml:"entry"`
}

func (rssAdapter) Parse(_ context.Context, _ Source, p Page) (Parsed, error) {
	var f rssFeed
	dec := xml.NewDecoder(bytes.NewReader(p.Body))
	dec.Strict = false
	dec.CharsetReader = charset.NewReaderLabel
	if err := dec.Decode(&f); err != nil {
		return Parsed{}, fmt.Errorf("%w: %w", errParse, err)
	}
	base := pageURL(p)
	out := Parsed{}
	add := func(title, link, body, date string) {
		if len(out.Docs) >= maxItems {
			return
		}
		u := resolve(base, link)
		if u == "" {
			u = p.URL
		}
		text := tidyLines(strings.Join([]string{stripTags(title), date, stripTags(body)}, "\n"))
		if text != "" {
			out.Docs = append(out.Docs, Document{URL: u, Text: text})
		}
	}
	for _, it := range f.Items {
		add(it.Title, it.Link, it.Description, it.PubDate)
	}
	for _, e := range f.Entries {
		link := ""
		for _, l := range e.Links {
			if l.Rel == "" || l.Rel == "alternate" {
				link = l.Href
				break
			}
		}
		body := e.Summary
		if body == "" {
			body = e.Content
		}
		add(e.Title, link, body, e.Updated)
	}
	return out, nil
}

// sitemapAdapter reads a sitemap (Boğaziçi's takvim): the pages on the
// same host become one-off htmlpage sources (newest lastmod first, at most
// maxLinks per run). A sitemap index is not followed.
type sitemapAdapter struct{}

func (sitemapAdapter) Name() AdapterName { return AdapterSitemap }
func (sitemapAdapter) Kind() Kind        { return KindEvents }
func (sitemapAdapter) Accept() []string  { return acceptFeed }

type urlSet struct {
	URLs []struct {
		Loc     string `xml:"loc"`
		LastMod string `xml:"lastmod"`
	} `xml:"url"`
}

func (sitemapAdapter) Parse(_ context.Context, src Source, p Page) (Parsed, error) {
	var s urlSet
	dec := xml.NewDecoder(bytes.NewReader(p.Body))
	dec.Strict = false
	dec.CharsetReader = charset.NewReaderLabel
	if err := dec.Decode(&s); err != nil {
		return Parsed{}, fmt.Errorf("%w: %w", errParse, err)
	}
	base := pageURL(p)
	var links []Link
	seen := map[string]bool{}
	for _, x := range s.URLs {
		u := resolve(base, strings.TrimSpace(x.Loc))
		if u == "" || seen[u] {
			continue
		}
		_, host, err := ParseSourceURL(u)
		if err != nil || host != src.Host {
			continue // a sitemap never sends the crawler to another host
		}
		seen[u] = true
		l := Link{URL: u}
		for _, layout := range []string{time.RFC3339, time.DateOnly} {
			if t, err := time.Parse(layout, strings.TrimSpace(x.LastMod)); err == nil {
				l.LastMod = &t
				break
			}
		}
		links = append(links, l)
	}
	slices.SortStableFunc(links, func(a, b Link) int {
		switch {
		case a.LastMod == nil && b.LastMod == nil:
			return 0
		case a.LastMod == nil:
			return 1
		case b.LastMod == nil:
			return -1
		}
		return b.LastMod.Compare(*a.LastMod)
	})
	if len(links) > maxLinks {
		links = links[:maxLinks]
	}
	return Parsed{Links: links}, nil
}
